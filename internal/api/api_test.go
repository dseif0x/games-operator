package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dseif0x/games-operator/internal/apps"
	"github.com/dseif0x/games-operator/internal/auth"
	"github.com/dseif0x/games-operator/internal/bridge"
	"github.com/dseif0x/games-operator/internal/config"
	"github.com/dseif0x/games-operator/internal/moonlight"
	"github.com/dseif0x/games-operator/internal/store"
)

type nopOrch struct{}

func (nopOrch) Notify(string)                                                  {}
func (nopOrch) PodLogs(context.Context, string, string, int64) ([]byte, error) { return nil, nil }
func (nopOrch) BridgeStatus(context.Context, string) (bridge.Status, error) {
	return bridge.Status{}, nil
}

type client struct {
	t    *testing.T
	base string
	http *http.Client
	csrf string
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Host = "games.example.com"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		req.Header.Set(auth.CSRFHeader, c.csrf)
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func newServer(t *testing.T) (*httptest.Server, *client) {
	t.Helper()
	return newServerAt(t, "", nil)
}

// newServerAt mounts the hub under basePath, with browser as the handler
// for the rest of the host.
func newServerAt(t *testing.T, basePath string, browser http.Handler) (*httptest.Server, *client) {
	t.Helper()
	st := store.NewMemory()
	hash, _ := auth.HashPassword("secret")
	if _, err := st.Users().UpsertPassword(context.Background(), "admin", hash); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cert, err := moonlight.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := url.Parse("https://games.example.com" + basePath)
	cfg := &config.Config{PublicURL: pub, BasePath: basePath, AllowedHosts: []string{"games.example.com"}, CookieSecret: bytes.Repeat([]byte{1}, 32)}
	svc := &apps.Service{Store: st, Orch: nopOrch{}, Broker: apps.NewBroker(), Log: log, Defaults: apps.Defaults{PVCSize: "50Gi", MaxConcurrent: 2}}
	s := &Server{
		Cfg: cfg, Store: st, Apps: svc, Pairing: moonlight.NewPairingManager(cert, st.Pairings(), log),
		Auth: auth.PasswordAuthenticator{Users: st.Users()}, Cookies: auth.NewSessions(cfg.CookieSecret, false, st.Users()),
		Limiter: auth.NewRateLimiter(10, time.Minute), Log: log, Browser: browser,
		UI: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ui:"+r.URL.Path) }),
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return srv, &client{t: t, base: srv.URL, http: &http.Client{Jar: jar}}
}

func TestBasePathAndBrowserProxy(t *testing.T) {
	browser := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "mw:"+r.URL.Path) })
	srv, c := newServerAt(t, "/hub", browser)
	get := func(path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Host = "games.example.com"
		res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, body := get("/"); code != 200 || body != "mw:/" {
		t.Fatalf("root must reach moonlight-web: %d %q", code, body)
	}
	if code, body := get("/ws/stream"); code != 200 || body != "mw:/ws/stream" {
		t.Fatalf("moonlight-web path: %d %q", code, body)
	}
	if code, body := get("/api/hosts"); code != 200 || body != "mw:/api/hosts" {
		t.Fatalf("moonlight-web api must not hit the hub: %d %q", code, body)
	}
	if code, body := get("/hub/"); code != 200 || body != "ui:/" {
		t.Fatalf("hub ui: %d %q", code, body)
	}
	if code, body := get("/hub/apps/123"); code != 200 || body != "ui:/apps/123" {
		t.Fatalf("hub ui deep route: %d %q", code, body)
	}
	if code, _ := get("/hub"); code != 307 {
		t.Fatalf("bare prefix must redirect: %d", code)
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Fatalf("probe at the root: %d", code)
	}
	if code, _ := c.do("POST", "/hub/api/v1/auth/login", map[string]string{"username": "admin", "password": "secret"}); code != 200 {
		t.Fatalf("login under the prefix: %d", code)
	}
	if code, _ := c.do("GET", "/hub/api/v1/apps", nil); code != 200 {
		t.Fatalf("api under the prefix: %d", code)
	}
	if code, _ := c.do("GET", "/api/v1/apps", nil); code != 200 {
		t.Fatalf("hub api at the root belongs to moonlight-web now: %d", code)
	}
}

func TestLoginAndApps(t *testing.T) {
	_, c := newServer(t)
	if code, _ := c.do("GET", "/api/v1/apps", nil); code != 401 {
		t.Fatalf("unauthenticated: %d", code)
	}
	if code, _ := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "wrong"}); code != 401 {
		t.Fatalf("bad password: %d", code)
	}
	code, out := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "secret"})
	if code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	c.csrf, _ = out["csrf"].(string)
	// Without the CSRF header writes are refused.
	saved := c.csrf
	c.csrf = ""
	if code, _ := c.do("POST", "/api/v1/apps", map[string]any{"name": "Steam", "preset": "steam"}); code != 403 {
		t.Fatalf("csrf: %d", code)
	}
	c.csrf = saved
	code, out = c.do("POST", "/api/v1/apps", map[string]any{"name": "Steam", "preset": "steam"})
	if code != 201 || out["state"] != "stopped" || out["image"] != "ghcr.io/games-on-whales/steam:edge" {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["id"].(string)
	code, out = c.do("GET", "/api/v1/apps", nil)
	if code != 200 || len(out["apps"].([]any)) != 1 || len(out["presets"].([]any)) < 5 {
		t.Fatalf("list: %d %v", code, out)
	}
	if code, out := c.do("POST", "/api/v1/apps/"+id+"/stop", nil); code != 409 {
		t.Fatalf("stop a stopped app: %d %v", code, out)
	}
	if code, out := c.do("POST", "/api/v1/apps", map[string]any{"name": "", "preset": "steam"}); code != 400 || !strings.Contains(out["error"].(string), "name") {
		t.Fatalf("validation: %d %v", code, out)
	}
	if code, _ := c.do("DELETE", "/api/v1/apps/"+id, nil); code != 202 {
		t.Fatalf("delete: %d", code)
	}
	code, out = c.do("GET", "/api/v1/pairings", nil)
	if code != 200 || len(out["pending"].([]any)) != 0 {
		t.Fatalf("pairings: %d %v", code, out)
	}
	if code, out := c.do("POST", "/api/v1/pairings/pin", map[string]string{"secret": "nope", "pin": "1234"}); code != 404 {
		t.Fatalf("pin for unknown secret: %d %v", code, out)
	}
}

func TestWolfCompatAPI(t *testing.T) {
	srv, c := newServer(t)
	_, out := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "secret"})
	c.csrf, _ = out["csrf"].(string)
	code, out := c.do("POST", "/api/v1/me/api-token", nil)
	if code != 200 || out["token"] == "" || !strings.HasSuffix(out["api_url"].(string), "/wolf") {
		t.Fatalf("token: %d %v", code, out)
	}
	token := out["token"].(string)
	req, _ := http.NewRequest("GET", srv.URL+"/wolf/api/v1/pair/pending", nil)
	req.Host = "games.example.com"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("no bearer: %d", res.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()
	if res.StatusCode != 200 || body["success"] != true {
		t.Fatalf("pending: %d %v", res.StatusCode, body)
	}
	if code, _ := c.do("DELETE", "/api/v1/me/api-token", nil); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	res, _ = http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("revoked token still works: %d", res.StatusCode)
	}
}

func TestHostAllowlist(t *testing.T) {
	srv, _ := newServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/apps", nil)
	req.Host = "evil.example.com"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("expected 421, got %d", res.StatusCode)
	}
}
