package browser

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
