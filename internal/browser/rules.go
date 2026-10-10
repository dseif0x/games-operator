package browser

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Rules is what a hub user may do with moonlight-web through the proxy.
// moonlight-web treats every request the proxy forwards as the host
// machine's own, so the proxy, not moonlight-web, decides: the embedded
// player's own routes pass, the two that write host settings get their
// bodies replaced with the hub's values, the rest of moonlight-web's API
// (admin, setup, sessions, sharing, other hosts) is closed.
type Rules struct {
	// MoonlightAddress is the only host address a browser may add: the
	// hub's Moonlight door.
	MoonlightAddress string
	// Backend returns the Wolf backend settings for the request's user
	// (the hub's Wolf-compatible API and a token bound to that user),
	// which replace whatever the browser sent.
	Backend func(r *http.Request) (apiURL, token string, err error)
}

var hostRoute = regexp.MustCompile(`^/api/hosts/[^/]+/(backend|seats|apps|running-app|pair/start|pair|start|quit|stop-session)$`)

// getRoutes are the read-only routes the player and its settings page use.
var getRoutes = map[string]bool{
	"/api/health": true, "/api/app/manifest": true, "/api/app/web-manifest": true,
	"/api/auth/status": true, "/api/backends": true, "/api/hosts": true,
	"/api/server/hostname": true, "/api/server/status": true, "/api/internet/status": true,
	"/api/settings/streaming": true, "/api/metrics/reporting": true,
}

// allowed reports whether the request may reach moonlight-web.
func (ru *Rules) allowed(method, path string) bool {
	if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/ws") {
		// Pages and assets.
		return method == http.MethodGet || method == http.MethodHead
	}
	if strings.HasPrefix(path, "/ws") {
		// The stream's own transport and the control channel.
		return method == http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		if getRoutes[path] {
			return true
		}
		if m := hostRoute.FindStringSubmatch(path); m != nil {
			switch m[1] {
			case "backend", "seats", "apps", "running-app":
				return true
			}
		}
	case http.MethodPost:
		if path == "/api/hosts/manual" {
			return true
		}
		if m := hostRoute.FindStringSubmatch(path); m != nil {
			switch m[1] {
			case "backend", "pair/start", "pair", "start", "quit", "stop-session":
				return true
			}
		}
	}
	return false
}

// rewrite replaces the body of the two requests that configure the host:
// the address a browser adds and the backend it points the host at are
// the hub's, whatever the browser asked for.
func (ru *Rules) rewrite(r *http.Request) error {
	if r.Method != http.MethodPost {
		return nil
	}
	var body any
	switch {
	case r.URL.Path == "/api/hosts/manual":
		body = map[string]string{"address": ru.MoonlightAddress}
	case strings.HasSuffix(r.URL.Path, "/backend") && hostRoute.MatchString(r.URL.Path):
		if ru.Backend == nil {
			return nil
		}
		apiURL, token, err := ru.Backend(r)
		if err != nil {
			return err
		}
		body = map[string]string{"type": "wolf", "apiUrl": apiURL, "apiToken": token}
	default:
		return nil
	}
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<16))
		_ = r.Body.Close()
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Del("Content-Encoding")
	return nil
}
