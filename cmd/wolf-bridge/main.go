// Command wolf-bridge runs next to Wolf in a session pod. Wolf's API is a
// unix socket; the bridge serves it over TCP to the hub, guarded by a
// bearer token, and reports readiness once the socket answers.
package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dseif0x/games-operator/internal/bridge"
)

var version = "dev"

func main() {
	socket := flag.String("socket", envOr(bridge.EnvSocket, bridge.DefaultSocket), "path of Wolf's API socket")
	listen := flag.String("listen", fmt.Sprintf(":%d", bridge.Port), "address to serve on")
	flag.Parse()
	token := os.Getenv(bridge.EnvToken)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if token == "" {
		log.Error(bridge.EnvToken + " is required")
		os.Exit(2)
	}
	log.Info("wolf-bridge starting", "version", version, "socket", *socket, "listen", *listen)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var ready atomic.Bool
	go watchSocket(ctx, *socket, &ready, log)
	tracker := &streamTracker{}
	go tracker.follow(ctx, *socket, &ready, log)

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) { return net.Dial("unix", *socket) },
			// Wolf's HTTP server is HTTP/1.0 without keep-alive; one
			// connection per request avoids stale sockets.
			DisableKeepAlives: true,
		},
		Timeout: 60 * time.Second,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "wolf socket not ready", http.StatusServiceUnavailable)
	})
	mux.Handle("GET /status", requireToken(token, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tracker.status(ready.Load()))
	})))
	mux.Handle("/api/", requireToken(token, proxy(client, &ready, log)))

	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	select {
	case err := <-errc:
		log.Error("serve failed", "err", err)
		os.Exit(1)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// watchSocket flips ready once Wolf accepts connections on its socket and
// keeps checking afterwards so a crashed Wolf shows up in readiness.
func watchSocket(ctx context.Context, path string, ready *atomic.Bool, log *slog.Logger) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			_ = conn.Close()
			if ready.CompareAndSwap(false, true) {
				log.Info("wolf socket ready", "path", path)
			}
		} else if ready.CompareAndSwap(true, false) {
			log.Warn("wolf socket gone", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func requireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// proxy forwards /api/* to the socket. Bodies are buffered so the request
// can be sent with a Content-Length, which Wolf's HTTP/1.0 server needs.
func proxy(client *http.Client, ready *atomic.Bool, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "wolf socket not ready", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, "http://wolf"+r.URL.RequestURI(), strings.NewReader(string(body)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.ContentLength = int64(len(body))
		if ct := r.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		req.Header.Set("Accept", "application/json")
		res, err := client.Do(req)
		if err != nil {
			log.Warn("wolf request failed", "method", r.Method, "path", r.URL.Path, "err", err)
			http.Error(w, "wolf: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		for k, vs := range res.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(res.StatusCode)
		_, _ = io.Copy(w, res.Body)
		log.Info("wolf", "method", r.Method, "path", r.URL.Path, "status", res.StatusCode)
	})
}

// streamTracker follows Wolf's server-sent events and remembers whether a
// client is streaming right now. The hub's idle policy reads it.
type streamTracker struct {
	mu        sync.Mutex
	streaming bool
	since     time.Time
	events    int64
}

func (t *streamTracker) status(ready bool) bridge.Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return bridge.Status{Ready: ready, Streaming: t.streaming, Since: t.since, Events: t.events}
}

func (t *streamTracker) apply(event string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events++
	switch event {
	case bridge.EventStreamSession, bridge.EventResumeStream:
		if !t.streaming {
			t.streaming, t.since = true, time.Now()
		}
	case bridge.EventPauseStream, bridge.EventStopStream:
		if t.streaming {
			t.streaming, t.since = false, time.Now()
		}
	}
}

// follow keeps a subscription to /api/v1/events open and reconnects
// whenever it drops (Wolf restarts, socket not there yet).
func (t *streamTracker) follow(ctx context.Context, socket string, ready *atomic.Bool, log *slog.Logger) {
	for {
		if ready.Load() {
			if err := t.subscribe(ctx, socket); err != nil && ctx.Err() == nil {
				log.Warn("wolf event stream ended", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (t *streamTracker) subscribe(ctx context.Context, socket string) error {
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	if _, err := io.WriteString(conn, "GET /api/v1/events HTTP/1.0\r\nHost: wolf\r\nAccept: text/event-stream\r\n\r\n"); err != nil {
		return err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if ev, ok := strings.CutPrefix(line, "event:"); ok {
			t.apply(strings.TrimSpace(ev))
		}
	}
	return sc.Err()
}
