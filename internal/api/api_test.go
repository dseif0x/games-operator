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
	t      *testing.T
	base   string
	http   *http.Client
	csrf   string
	server *Server
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
	cfg := &config.Config{PublicURL: pub, BasePath: basePath, BrowserPath: "/play", AllowedHosts: []string{"games.example.com"}, CookieSecret: bytes.Repeat([]byte{1}, 32)}
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
	return srv, &client{t: t, base: srv.URL, http: &http.Client{Jar: jar}, server: s}
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
	// The player lives under its own prefix; the proxy sees the full path.
	if code, body := get("/play/"); code != 200 || body != "mw:/play/" {
		t.Fatalf("player root: %d %q", code, body)
	}
	if code, body := get("/play/ws/stream"); code != 200 || body != "mw:/play/ws/stream" {
		t.Fatalf("player websocket path: %d %q", code, body)
	}
	if code, _ := get("/play"); code != 307 {
		t.Fatalf("bare player prefix must redirect: %d", code)
	}
	// The hub under its base path, everything else redirected there.
	if code, body := get("/hub/"); code != 200 || body != "ui:/" {
		t.Fatalf("hub ui: %d %q", code, body)
	}
	if code, body := get("/hub/apps/123"); code != 200 || body != "ui:/apps/123" {
		t.Fatalf("hub ui deep route: %d %q", code, body)
	}
	if code, _ := get("/"); code != 307 {
		t.Fatalf("root must redirect to the hub: %d", code)
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
	// At the root the player keeps its own prefix even when the hub has none.
	srv2, _ := newServerAt(t, "", browser)
	req, _ := http.NewRequest("GET", srv2.URL+"/play/api/hosts", nil)
	req.Host = "games.example.com"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "mw:/play/api/hosts" {
		t.Fatalf("player next to a root hub: %d %q", res.StatusCode, body)
	}
}

func TestPlayInBrowser(t *testing.T) {
	srv, c := newServerAt(t, "", nil)
	_ = srv
	code, out := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "secret"})
	if code != 200 {
		t.Fatal("login")
	}
	c.csrf, _ = out["csrf"].(string)
	code, out = c.do("POST", "/api/v1/apps", map[string]any{"name": "Steam", "preset": "steam"})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["id"].(string)
	// Without a Moonlight address there is nothing a browser could connect to.
	if code, _ := c.do("POST", "/api/v1/apps/"+id+"/play", map[string]any{}); code != 409 {
		t.Fatalf("play without LB IP: %d", code)
	}
	c.server.Apps.Defaults.MoonlightHost = "10.0.0.9"
	code, out = c.do("POST", "/api/v1/apps/"+id+"/play", map[string]any{})
	if code != 200 {
		t.Fatalf("play: %d %v", code, out)
	}
	backend := out["backend"].(map[string]any)
	tok, _ := backend["api_token"].(string)
	if tok == "" || backend["api_url"] != "https://games.example.com/wolf" || out["moonlight_host"] != "10.0.0.9" {
		t.Fatalf("play answer: %v", out)
	}
	if app := out["app"].(map[string]any); app["state"] != "starting" && app["state"] != "running" {
		t.Fatalf("play must start the app: %v", app["state"])
	}
	// The browser token opens the Wolf-compatible API as this user.
	req, _ := http.NewRequest("GET", srv.URL+"/wolf/api/v1/pair/pending", nil)
	req.Host = "games.example.com"
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("browser token on the wolf api: %d", res.StatusCode)
	}
	// Each play mints a new token; the old one stops working.
	if code, _ := c.do("POST", "/api/v1/apps/"+id+"/play", map[string]any{}); code != 200 {
		t.Fatal("second play")
	}
	res2, _ := http.DefaultClient.Do(req)
	res2.Body.Close()
	if res2.StatusCode != 401 {
		t.Fatalf("stale browser token: %d", res2.StatusCode)
	}
}
