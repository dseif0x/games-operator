package browser

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestProxy(t *testing.T) {
	var seen *http.Request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(r.Context())
		if r.URL.Path == "/setup" {
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "mw:"+r.URL.Path+":"+r.Header.Get(Header))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	h := New(Options{
		Upstream: u, Prefix: "/play", Secret: "s3cret",
		Authenticated: func(r *http.Request) bool { return r.Header.Get("Cookie") == "hub=yes" },
		LoginPath:     "/login", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string, hdr map[string]string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}

	// No hub login: a page navigation goes to the login, an API call gets 401.
	if res, _ := get("/play/", map[string]string{"Sec-Fetch-Dest": "document"}); res.StatusCode != 302 || res.Header.Get("Location") != "/login?next=%2Fplay%2F" {
		t.Fatalf("anonymous page: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := get("/play/api/hosts", nil); res.StatusCode != 401 {
		t.Fatalf("anonymous api: %d", res.StatusCode)
	}
	// Logged in: the prefix is stripped, the secret added, a spoofed header dropped.
	res, body := get("/play/api/hosts", map[string]string{"Cookie": "hub=yes", Header: "spoof"})
	if res.StatusCode != 200 || body != "mw:/api/hosts:s3cret" {
		t.Fatalf("proxied: %d %q", res.StatusCode, body)
	}
	if seen.Header.Get("X-Forwarded-Host") == "" {
		t.Fatal("forwarded headers missing")
	}
	if _, body := get("/play/", map[string]string{"Cookie": "hub=yes"}); body != "mw:/:s3cret" {
		t.Fatalf("root of the mount: %q", body)
	}
	// Redirects from moonlight-web come back under the prefix.
	if res, _ := get("/play/setup", map[string]string{"Cookie": "hub=yes"}); res.StatusCode != 302 || res.Header.Get("Location") != "/play/admin" {
		t.Fatalf("redirect rewrite: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestRules(t *testing.T) {
	var seen []string
	var seenBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		seenBody = string(b)
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	h := New(Options{
		Upstream: u, Prefix: "/play", Secret: "s",
		Authenticated: func(*http.Request) bool { return true },
		Rules: &Rules{MoonlightAddress: "10.0.0.9", Backend: func(*http.Request) (string, string, error) {
			return "https://hub.example/wolf", "tok-for-user", nil
		}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	do := func(method, path, body string) int {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	// The player's own routes pass.
	for _, r := range []struct{ method, path string }{
		{"GET", "/play/"}, {"GET", "/play/js/app.js"}, {"GET", "/play/api/hosts"}, {"GET", "/play/api/auth/status"},
		{"GET", "/play/api/hosts/h1/apps?client_uniqueid=AB"}, {"GET", "/play/api/hosts/h1/running-app?client_uniqueid=AB"},
		{"POST", "/play/api/hosts/h1/pair/start"}, {"POST", "/play/api/hosts/h1/pair"}, {"POST", "/play/api/hosts/h1/start"},
		{"POST", "/play/api/hosts/h1/quit"}, {"GET", "/play/ws/control"}, {"GET", "/play/api/settings/streaming"},
	} {
		if code := do(r.method, r.path, ""); code != 200 {
			t.Errorf("%s %s: %d", r.method, r.path, code)
		}
	}
	// The rest of moonlight-web is closed.
	for _, r := range []struct{ method, path string }{
		{"GET", "/play/api/admin/settings"}, {"POST", "/play/api/admin/settings"}, {"POST", "/play/api/auth/logout"},
		{"GET", "/play/api/auth/sessions"}, {"POST", "/play/api/setup/apply"}, {"POST", "/play/api/update/start"},
		{"DELETE", "/play/api/hosts/h1"}, {"DELETE", "/play/api/hosts/h1/backend"}, {"POST", "/play/api/hosts/h1/wol"},
		{"POST", "/play/api/hosts/scan"}, {"POST", "/play/api/hosts/h1/seats"}, {"DELETE", "/play/api/hosts/h1/seats/s1/owner"},
		{"POST", "/play/api/share/feed"}, {"POST", "/play/api/settings/streaming"}, {"POST", "/play/api/metrics/reporting"},
		{"POST", "/play/index.html"}, {"GET", "/play/api/logs/archive"},
	} {
		if code := do(r.method, r.path, "{}"); code != 403 {
			t.Errorf("%s %s must be closed, got %d", r.method, r.path, code)
		}
	}
	// The two host-writing requests carry the hub's values, not the browser's.
	seen, seenBody = nil, ""
	if code := do("POST", "/play/api/hosts/manual", `{"address":"10.66.6.6"}`); code != 200 {
		t.Fatalf("add host: %d", code)
	}
	if seenBody != `{"address":"10.0.0.9"}` {
		t.Fatalf("host address not forced: %q", seenBody)
	}
	if code := do("POST", "/play/api/hosts/h1/backend", `{"type":"wolf","apiUrl":"http://evil/","apiToken":"stolen"}`); code != 200 {
		t.Fatalf("set backend: %d", code)
	}
	if seenBody != `{"apiToken":"tok-for-user","apiUrl":"https://hub.example/wolf","type":"wolf"}` {
		t.Fatalf("backend not forced: %q", seenBody)
	}
	if len(seen) != 2 || seen[1] != "POST /api/hosts/h1/backend" {
		t.Fatalf("upstream saw %v", seen)
	}
}
