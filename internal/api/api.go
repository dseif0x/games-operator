// Package api serves the REST API, the SSE feed, the Wolf-compatible
// pairing API for moonlight-web, health and metrics, and hands everything
// else to the embedded UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dseif0x/games-operator/internal/apps"
	"github.com/dseif0x/games-operator/internal/auth"
	"github.com/dseif0x/games-operator/internal/config"
	"github.com/dseif0x/games-operator/internal/moonlight"
	"github.com/dseif0x/games-operator/internal/store"
)

// Server holds the handler dependencies.
type Server struct {
	Cfg     *config.Config
	Store   store.Store
	Apps    *apps.Service
	Pairing *moonlight.PairingManager
	Auth    auth.Authenticator
	Cookies *auth.Sessions
	Limiter *auth.RateLimiter
	Ready   func() bool
	UI      http.Handler
	// Browser serves everything outside Cfg.BasePath (moonlight-web).
	Browser http.Handler
	Metrics http.Handler
	Log     *slog.Logger
}

type ctxKey int

const principalKey ctxKey = 1

func principal(r *http.Request) *auth.Principal {
	p, _ := r.Context().Value(principalKey).(*auth.Principal)
	return p
}

// Handler builds the full router. The hub's routes live under
// Cfg.BasePath; probes and metrics stay at the root for the kubelet and
// Prometheus. With a prefix, the rest of the host goes to Browser
// (moonlight-web) or redirects to the UI.
func (s *Server) Handler() http.Handler {
	hub := s.recover(s.securityHeaders(s.hubMux()))
	base := s.Cfg.BasePath
	root := http.NewServeMux()
	s.probes(root)
	if s.Browser != nil && s.Cfg.BrowserPath != "" {
		// The embedded moonlight-web, below its own prefix (default /play).
		// It carries its own security headers; the hub's would break it.
		bp := s.Cfg.BrowserPath
		root.Handle(bp+"/", s.recover(s.Browser))
		root.HandleFunc(bp, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, bp+"/", http.StatusTemporaryRedirect)
		})
	}
	if base == "" {
		root.Handle("/", hub)
	} else {
		root.Handle(base+"/", http.StripPrefix(base, hub))
		root.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, base+"/", http.StatusTemporaryRedirect)
		})
	}
	return s.recover(s.hostAllowlist(s.logging(root)))
}

// Authenticated reports whether the request carries a valid hub session;
// the browser proxy uses it to decide whom to trust.
func (s *Server) Authenticated(r *http.Request) bool {
	_, err := s.Cookies.Read(r)
	return err == nil
}

func (s *Server) probes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeText(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.Ready != nil && !s.Ready() {
			writeText(w, http.StatusServiceUnavailable, "not ready")
			return
		}
		writeText(w, http.StatusOK, "ok")
	})
	if s.Metrics != nil {
		mux.Handle("GET /metrics", s.Metrics)
	}
}

// hubMux is the hub's own API and UI, addressed relative to the base path.
func (s *Server) hubMux() *http.ServeMux {
	mux := http.NewServeMux()
	s.probes(mux)

	// Public auth routes.
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/logout", s.logout)

	// Authenticated routes (cookie + CSRF).
	authed := http.NewServeMux()
	authed.HandleFunc("GET /api/v1/auth/me", s.me)
	authed.HandleFunc("GET /api/v1/apps", s.listApps)
	authed.HandleFunc("POST /api/v1/apps", s.createApp)
	authed.HandleFunc("GET /api/v1/apps/events", s.appEvents)
	authed.HandleFunc("GET /api/v1/apps/{id}", s.getApp)
	authed.HandleFunc("PATCH /api/v1/apps/{id}", s.updateApp)
	authed.HandleFunc("DELETE /api/v1/apps/{id}", s.deleteApp)
	authed.HandleFunc("POST /api/v1/apps/{id}/start", s.startApp)
	authed.HandleFunc("POST /api/v1/apps/{id}/play", s.playApp)
	authed.HandleFunc("POST /api/v1/apps/{id}/stop", s.stopApp)
	authed.HandleFunc("GET /api/v1/apps/{id}/events", s.appEventLog)
	authed.HandleFunc("GET /api/v1/apps/{id}/logs", s.appLogs)
	authed.HandleFunc("GET /api/v1/pairings", s.listPairings)
	authed.HandleFunc("POST /api/v1/pairings/pin", s.submitPin)
	authed.HandleFunc("DELETE /api/v1/pairings/{id}", s.deletePairing)
	authed.HandleFunc("POST /api/v1/me/api-token", s.newAPIToken)
	authed.HandleFunc("DELETE /api/v1/me/api-token", s.deleteAPIToken)
	authed.HandleFunc("POST /api/v1/me/password", s.changePassword)
	authed.HandleFunc("GET /api/v1/catalog", s.listCatalog)

	// Admin routes: users, the catalog, every app.
	admin := http.NewServeMux()
	admin.HandleFunc("GET /api/v1/users", s.listUsers)
	admin.HandleFunc("POST /api/v1/users", s.createUser)
	admin.HandleFunc("PATCH /api/v1/users/{id}", s.updateUser)
	admin.HandleFunc("DELETE /api/v1/users/{id}", s.deleteUser)
	admin.HandleFunc("POST /api/v1/catalog", s.createTemplate)
	admin.HandleFunc("PATCH /api/v1/catalog/{id}", s.updateTemplate)
	admin.HandleFunc("DELETE /api/v1/catalog/{id}", s.deleteTemplate)
	admin.HandleFunc("GET /api/v1/admin/apps", s.listAllApps)
	admin.HandleFunc("GET /api/v1/admin/events", s.allAppEvents)
	admin.HandleFunc("POST /api/v1/admin/apps/{id}/stop", s.stopAnyApp)
	admin.HandleFunc("DELETE /api/v1/admin/apps/{id}", s.deleteAnyApp)
	authed.Handle("/api/v1/users", s.requireAdmin(admin))
	authed.Handle("/api/v1/users/", s.requireAdmin(admin))
	authed.Handle("/api/v1/admin/", s.requireAdmin(admin))
	authed.Handle("POST /api/v1/catalog", s.requireAdmin(admin))
	authed.Handle("PATCH /api/v1/catalog/{id}", s.requireAdmin(admin))
	authed.Handle("DELETE /api/v1/catalog/{id}", s.requireAdmin(admin))
	mux.Handle("/api/v1/", s.requireAuth(s.requireCSRF(authed)))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) { writeErr(w, http.StatusNotFound, "no such route") })

	// Wolf-compatible API (bearer token) so moonlight-web pairs on its own.
	wolfAPI := http.NewServeMux()
	wolfAPI.HandleFunc("GET /wolf/api/v1/pair/pending", s.wolfPending)
	wolfAPI.HandleFunc("POST /wolf/api/v1/pair/client", s.wolfPairClient)
	wolfAPI.HandleFunc("GET /wolf/api/v1/clients", s.wolfClients)
	wolfAPI.HandleFunc("POST /wolf/api/v1/unpair/client", s.wolfUnpair)
	mux.Handle("/wolf/", s.requireToken(wolfAPI))

	if s.UI != nil {
		mux.Handle("/", s.UI)
	}
	return mux
}

// ---- middleware ----

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			s.Log.Error("panic", "err", rec, "path", r.URL.Path)
			writeErr(w, http.StatusInternalServerError, "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}

// hostAllowlist rejects requests whose Host is not the public host, which
// blocks DNS rebinding. Health probes come from kubelet and are exempt.
func (s *Server) hostAllowlist(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, h := range s.Cfg.AllowedHosts {
		allowed[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		host := strings.ToLower(r.Host)
		if !allowed[host] {
			if h, _, ok := strings.Cut(host, ":"); !ok || !allowed[h] {
				s.Log.Warn("rejected host", "host", r.Host, "path", r.URL.Path)
				writeErr(w, http.StatusMisdirectedRequest, "host not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/wolf/") {
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the flusher for SSE.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds(), "ip", auth.ClientIP(r))
	})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Cookies.Read(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "not logged in")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

// requireAdmin answers 403 for everyone but admins.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := principal(r); p == nil || !p.User.IsAdmin() {
			writeErr(w, http.StatusForbidden, "admin only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !s.Cookies.CheckCSRF(r, principal(r)) {
				writeErr(w, http.StatusForbidden, "missing or invalid CSRF token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireToken authenticates the Wolf-compatible API with a user's API
// token (Authorization: Bearer).
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" || tok == r.Header.Get("Authorization") {
			writeWolfErr(w, http.StatusUnauthorized, "bearer token required")
			return
		}
		hash := auth.HashToken(tok)
		browser := false
		u, err := s.Store.Users().GetByAPITokenHash(r.Context(), hash)
		if errors.Is(err, store.ErrNotFound) {
			// The hub-managed token the embedded moonlight-web holds.
			u, err = s.Store.Users().GetByBrowserTokenHash(r.Context(), hash)
			browser = true
		}
		if err != nil || u.Disabled {
			writeWolfErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		p := &auth.Principal{User: u, Browser: browser}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeWolfErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"success": false, "error": msg})
}

func writeText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// mapErr turns service errors into HTTP responses.
func (s *Server) mapErr(w http.ResponseWriter, err error) {
	var ve *apps.ValidationError
	var qe *apps.QuotaError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "already exists")
	case errors.Is(err, apps.ErrForbidden):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.As(err, &qe):
		writeErr(w, http.StatusConflict, qe.Msg)
	case errors.Is(err, apps.ErrInvalidTransition), errors.Is(err, apps.ErrNoSlot):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.As(err, &ve):
		writeErr(w, http.StatusBadRequest, ve.Msg)
	default:
		s.Log.Error("request failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

// ---- auth ----

type userJSON struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	HasToken bool   `json:"has_api_token"`
	Disabled bool   `json:"disabled"`
	// Quota is the user's own override (nil fields = defaults); Usage the
	// effective limits next to what the user has.
	Quota store.Quota     `json:"quota"`
	Usage *apps.QuotaView `json:"usage,omitempty"`
}

type meJSON struct {
	User userJSON `json:"user"`
	CSRF string   `json:"csrf"`
}

func (s *Server) userJSON(u *store.User) userJSON {
	return userJSON{ID: u.ID, Username: u.Username, Role: u.Role, HasToken: u.APITokenHash != "", Disabled: u.Disabled, Quota: u.Quota}
}

// userWithUsage adds the quota and usage; a failure there leaves them out
// rather than failing the login.
func (s *Server) userWithUsage(ctx context.Context, u *store.User) userJSON {
	j := s.userJSON(u)
	if usage, err := s.Apps.Usage(ctx, u); err == nil {
		j.Usage = &usage
	}
	return j
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := auth.ClientIP(r)
	if !s.Limiter.Allowed(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many failed logins; try again later")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.Auth.Login(r.Context(), body.Username, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrBadCredentials) {
			s.Limiter.Fail(ip)
			writeErr(w, http.StatusUnauthorized, err.Error())
			return
		}
		s.mapErr(w, err)
		return
	}
	s.Limiter.Reset(ip)
	p := s.Cookies.Issue(w, u)
	writeJSON(w, http.StatusOK, meJSON{User: s.userWithUsage(r.Context(), u), CSRF: s.Cookies.CSRFToken(p)})
}

func (s *Server) logout(w http.ResponseWriter, _ *http.Request) {
	s.Cookies.Clear(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	writeJSON(w, http.StatusOK, meJSON{User: s.userWithUsage(r.Context(), p.User), CSRF: s.Cookies.CSRFToken(p)})
}

// ---- apps ----

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	list, err := s.Apps.List(r.Context(), p.User.ID)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": s.views(r.Context(), list), "presets": s.Apps.Presets(), "defaults": s.Apps.Defaults})
}

// views renders apps with their catalog entry's name filled in.
func (s *Server) views(ctx context.Context, list []*store.App) []apps.View {
	names := map[string]string{}
	if tpls, err := s.Store.Catalog().List(ctx); err == nil {
		for _, t := range tpls {
			names[t.ID] = t.Name
		}
	}
	views := make([]apps.View, 0, len(list))
	for _, a := range list {
		v := s.Apps.View(a)
		v.TemplateName = names[a.TemplateID]
		views = append(views, v)
	}
	return views
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req apps.CreateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a, err := s.Apps.Create(r.Context(), p.User, req)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.Apps.View(a))
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	a, err := s.Apps.Get(r.Context(), p.User.ID, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Apps.View(a))
}

func (s *Server) updateApp(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req apps.CreateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a, err := s.Apps.Update(r.Context(), p.User, r.PathValue("id"), req)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Apps.View(a))
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	a, err := s.Apps.Delete(r.Context(), p.User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.Apps.View(a))
}

func (s *Server) startApp(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	a, err := s.Apps.Start(r.Context(), p.User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.Apps.View(a))
}

// playJSON is what the embedded moonlight-web needs to stream an app.
type playJSON struct {
	App               apps.View `json:"app"`
	MoonlightHost     string    `json:"moonlight_host"`
	MoonlightHostname string    `json:"moonlight_hostname"`
	Backend           struct {
		Type     string `json:"type"`
		APIURL   string `json:"api_url"`
		APIToken string `json:"api_token"`
	} `json:"backend"`
}

// playApp prepares "Play in browser": it starts the app if needed and
// mints the user's browser token (what moonlight-web pairs with).
func (s *Server) playApp(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	ctx := r.Context()
	host := s.Apps.Defaults.MoonlightHost
	if host == "" {
		writeErr(w, http.StatusConflict, "the Moonlight LoadBalancer IP is not configured (moonlight.loadBalancerIP)")
		return
	}
	a, err := s.Apps.Start(ctx, p.User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	tok, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Store.Users().SetBrowserTokenHash(ctx, p.User.ID, auth.HashToken(tok)); err != nil {
		s.mapErr(w, err)
		return
	}
	// The pairings moonlight-web makes with this token belong to this user:
	// the embedded player presents a client certificate per browser and
	// hub user (docs/OPERATIONS.md, In-browser play), so nothing is shared.
	out := playJSON{App: s.Apps.View(a), MoonlightHost: host, MoonlightHostname: s.Cfg.MoonlightHostname}
	out.Backend.Type = "wolf"
	out.Backend.APIURL = s.Cfg.PublicURL.String() + "/wolf"
	out.Backend.APIToken = tok
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) stopApp(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	a, err := s.Apps.Stop(r.Context(), p.User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.Apps.View(a))
}

func (s *Server) appEventLog(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	evs, err := s.Apps.Events(r.Context(), p.User.ID, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	type ev struct {
		ID      int64     `json:"id"`
		At      time.Time `json:"at"`
		Kind    string    `json:"kind"`
		Message string    `json:"message"`
	}
	out := make([]ev, 0, len(evs))
	for _, e := range evs {
		out = append(out, ev{e.ID, e.At, e.Kind, e.Message})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (s *Server) appLogs(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	b, err := s.Apps.Logs(r.Context(), p.User.ID, r.PathValue("id"), r.URL.Query().Get("container"))
	if err != nil {
		var ve *apps.ValidationError
		if errors.Is(err, store.ErrNotFound) || errors.As(err, &ve) {
			s.mapErr(w, err)
			return
		}
		writeErr(w, http.StatusBadGateway, "logs unavailable: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// appEvents streams state changes for the caller's apps as SSE.
func (s *Server) appEvents(w http.ResponseWriter, r *http.Request) {
	s.streamEvents(w, r, principal(r).User.ID)
}

// ---- pairings ----

type pairingJSON struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
}

func (s *Server) listPairings(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	list, err := s.Store.Pairings().List(r.Context(), p.User.ID)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	out := make([]pairingJSON, 0, len(list))
	for _, pr := range list {
		out = append(out, pairingJSON{pr.ID, pr.Name, pr.CreatedAt, pr.LastSeenAt})
	}
	pending := s.Pairing.Pending()
	if pending == nil {
		pending = []moonlight.PendingPair{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pairings": out, "pending": pending})
}

// submitPin takes the PIN Moonlight shows and binds the pairing to the
// logged-in user.
func (s *Server) submitPin(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var body struct {
		Secret string `json:"secret"`
		Pin    string `json:"pin"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Pin = strings.TrimSpace(body.Pin)
	if len(body.Pin) != 4 || strings.Trim(body.Pin, "0123456789") != "" {
		writeErr(w, http.StatusBadRequest, "the PIN is four digits")
		return
	}
	if err := s.Pairing.SubmitPin(body.Secret, body.Pin, p.User.ID, ""); err != nil {
		if errors.Is(err, moonlight.ErrNoPending) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deletePairing(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := s.Store.Pairings().Delete(r.Context(), p.User.ID, r.PathValue("id")); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- API token (moonlight-web) ----

func (s *Server) newAPIToken(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	tok, err := auth.NewToken()
	if err != nil {
		s.mapErr(w, err)
		return
	}
	if err := s.Store.Users().SetAPITokenHash(r.Context(), p.User.ID, auth.HashToken(tok)); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok, "api_url": s.Cfg.PublicURL.String() + "/wolf"})
}

func (s *Server) deleteAPIToken(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := s.Store.Users().SetAPITokenHash(r.Context(), p.User.ID, ""); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- Wolf-compatible API ----

func (s *Server) wolfPending(w http.ResponseWriter, _ *http.Request) {
	type req struct {
		PairSecret string `json:"pair_secret"`
		ClientIP   string `json:"client_ip"`
	}
	pending := s.Pairing.Pending()
	out := make([]req, 0, len(pending))
	for _, p := range pending {
		out = append(out, req{p.Secret, p.ClientIP})
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "requests": out})
}

func (s *Server) wolfPairClient(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var body struct {
		PairSecret string `json:"pair_secret"`
		Pin        string `json:"pin"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeWolfErr(w, http.StatusBadRequest, err.Error())
		return
	}
	via := ""
	if p.Browser {
		via = store.PairingViaBrowser
	}
	if err := s.Pairing.SubmitPin(body.PairSecret, strings.TrimSpace(body.Pin), p.User.ID, via); err != nil {
		writeWolfErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) wolfClients(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	list, err := s.Store.Pairings().List(r.Context(), p.User.ID)
	if err != nil {
		writeWolfErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type client struct {
		ClientID       string `json:"client_id"`
		AppStateFolder string `json:"app_state_folder"`
	}
	out := make([]client, 0, len(list))
	for _, pr := range list {
		out = append(out, client{pr.ID, pr.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "clients": out})
}

func (s *Server) wolfUnpair(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var body struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeWolfErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.Pairings().Delete(r.Context(), p.User.ID, body.ClientID); err != nil {
		writeWolfErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}
