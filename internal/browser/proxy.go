// Package browser reverse-proxies moonlight-web through the hub, so the
// browser client shares the hub's host name, Ingress, certificate and
// tunnel instead of needing a port or address of its own.
//
// moonlight-web serves everything from the root of a host (/api, /ws,
// /js, /css, ...), so the hub keeps its own UI and API under a path prefix
// and hands the rest of the host to this proxy. WebSocket upgrades pass
// through; the stream itself then runs as WebRTC where UDP reaches the
// browser, and over that WebSocket where it does not (a Cloudflare tunnel,
// a firewall between subnets).
package browser

import (
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// New returns a handler that forwards every request to upstream. The Host
// header is kept, so moonlight-web builds its WebSocket origin and CSP for
// the public name. A self-signed certificate on upstream is accepted: the
// hop stays inside the cluster.
func New(upstream *url.URL, log *slog.Logger) http.Handler {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // in-cluster hop to moonlight-web's self-signed listener
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		Transport: transport,
		// Media over the WebSocket fallback and SSE must not be buffered.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("moonlight-web unreachable", "err", err, "path", r.URL.Path)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("moonlight-web is not reachable right now; try again in a moment.\n"))
		},
	}
	return proxy
}
