// Package browser reverse-proxies the embedded moonlight-web (the
// games-operator fork, docs/GAMES-OPERATOR.md there) below a path prefix
// of the hub's own host, so the browser client shares the hub's Ingress,
// certificate and tunnel, and the hub's login decides who may use it.
//
// The hub authenticates the browser with its session cookie and marks every
// request it forwards with X-MW-Embedded: <secret>; moonlight-web treats
// those like the host machine's own (no PIN, no session of its own). The
// header is stripped from what clients send, so it can only come from here.
// WebSocket upgrades pass through; the stream itself then runs as WebRTC
// where UDP reaches the browser and over that WebSocket where it does not.
package browser

import (
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Header is what the proxy adds to every forwarded request.
const Header = "X-MW-Embedded"

// Options configures the proxy.
type Options struct {
	// Upstream is moonlight-web's in-cluster address (https://…:8443).
	Upstream *url.URL
	// Prefix is where the proxy is mounted on the hub ("/play"); it is
	// stripped before forwarding, and put back on redirects coming back.
	Prefix string
	// Secret goes into Header on forwarded requests.
	Secret string
	// Authenticated reports whether the request carries a hub login.
	Authenticated func(*http.Request) bool
	// LoginPath is where a browser without a login is sent (the hub's
	// login page, with ?next= back to the player).
	LoginPath string
	Log       *slog.Logger
}

// New returns the proxy handler.
func New(o Options) http.Handler {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // in-cluster hop to moonlight-web's self-signed listener
	}
	prefix := strings.TrimSuffix(o.Prefix, "/")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(o.Upstream)
			r.Out.Host = r.In.Host
			r.SetXForwarded()
			r.Out.URL.Path = strip(r.In.URL.Path, prefix)
			r.Out.URL.RawPath = ""
			r.Out.Header.Set(Header, o.Secret)
		},
		Transport: transport,
		// Media over the WebSocket fallback and SSE must not be buffered.
		FlushInterval: -1,
		ModifyResponse: func(res *http.Response) error {
			// moonlight-web redirects to root-absolute paths of its own
			// (e.g. /setup); put them back under the prefix.
			if loc := res.Header.Get("Location"); prefix != "" && strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") && !strings.HasPrefix(loc, prefix+"/") && loc != prefix {
				res.Header.Set("Location", prefix+loc)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("moonlight-web unreachable", "err", err, "path", r.URL.Path)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("moonlight-web is not reachable right now; try again in a moment.\n"))
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never trust the header from a client.
		r.Header.Del(Header)
		if o.Authenticated != nil && !o.Authenticated(r) {
			if wantsPage(r) && o.LoginPath != "" {
				// The #app=… fragment survives a redirect whose Location has none.
				http.Redirect(w, r, o.LoginPath+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"authentication_required"}`))
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func strip(p, prefix string) string {
	if prefix == "" {
		return p
	}
	if p == prefix {
		return "/"
	}
	if strings.HasPrefix(p, prefix+"/") {
		return p[len(prefix):]
	}
	return p
}

// wantsPage is true for a navigation (the player page itself) rather than
// an API or asset fetch, which gets a plain 401.
func wantsPage(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" {
		return d == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
