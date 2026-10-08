package moonlight

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dseif0x/games-operator/internal/store"
)

// Launcher is what the server needs from the app service.
type Launcher interface {
	// Apps lists the user's apps, in Moonlight list order.
	Apps(ctx context.Context, userID string) ([]*store.App, error)
	// Current returns the user's running app, or nil.
	Current(ctx context.Context, userID string) (*store.App, error)
	// Launch starts the app (or re-keys the running one when resume is set)
	// and blocks until the stream URL is known or ctx ends.
	Launch(ctx context.Context, user *store.User, pairing *store.Pairing, moonlightID int32, stream store.Stream, resume bool) (string, error)
	// Cancel stops the user's running app.
	Cancel(ctx context.Context, user *store.User) error
}

// Options configure the server.
type Options struct {
	HTTPPort  int
	HTTPSPort int
	Hostname  string
	// UniqueID identifies this host to Moonlight; keep it stable.
	UniqueID string
	Cert     tls.Certificate
	// LaunchTimeout bounds how long /launch waits for the stream (the node
	// may have to power on first).
	LaunchTimeout time.Duration
}

// Server speaks the Moonlight protocol on two ports.
type Server struct {
	opts     Options
	pairing  *PairingManager
	users    store.Users
	pairings store.Pairings
	launcher Launcher
	log      *slog.Logger
	plain    *http.ServeMux
	secure   *http.ServeMux
}

// NewServer wires the handlers.
func NewServer(opts Options, pm *PairingManager, users store.Users, pairings store.Pairings, launcher Launcher, log *slog.Logger) *Server {
	if opts.LaunchTimeout == 0 {
		opts.LaunchTimeout = 10 * time.Minute
	}
	s := &Server{opts: opts, pairing: pm, users: users, pairings: pairings, launcher: launcher, log: log, plain: http.NewServeMux(), secure: http.NewServeMux()}
	s.plain.HandleFunc("/serverinfo", s.serverInfo)
	s.plain.HandleFunc("/pair", s.pair)
	s.plain.HandleFunc("/unpair", s.unpair)
	s.plain.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s.secure.HandleFunc("/serverinfo", s.serverInfo)
	s.secure.HandleFunc("/pair", s.pair)
	s.secure.HandleFunc("/applist", s.appList)
	s.secure.HandleFunc("/appasset", s.appAsset)
	s.secure.HandleFunc("/launch", s.launch)
	s.secure.HandleFunc("/resume", s.resume)
	s.secure.HandleFunc("/cancel", s.cancel)
	return s
}

// Run serves until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	plain := &http.Server{Addr: fmt.Sprintf(":%d", s.opts.HTTPPort), Handler: s.logging(s.plain), ReadHeaderTimeout: 10 * time.Second}
	secure := &http.Server{
		Addr:              fmt.Sprintf(":%d", s.opts.HTTPSPort),
		Handler:           s.logging(s.authenticated(s.secure)),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{s.opts.Cert},
			ClientAuth:   tls.RequestClientCert,
			// Any client certificate is accepted at the TLS layer; the
			// authenticated middleware matches it against the pairings.
			VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error { return nil },
			VerifyConnection:      func(tls.ConnectionState) error { return nil },
			// No resumption: every connection presents its certificate.
			SessionTicketsDisabled: true,
			MinVersion:             tls.VersionTLS12,
		},
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var failure atomic.Pointer[error]
	serve := func(name string, fn func() error) {
		defer cancel()
		s.log.Info("moonlight listening", "server", name)
		if err := fn(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failure.CompareAndSwap(nil, &err)
		}
	}
	go serve("http", plain.ListenAndServe)
	go serve("https", func() error { return secure.ListenAndServeTLS("", "") })
	<-ctx.Done()
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	_ = plain.Shutdown(sctx)
	_ = secure.Shutdown(sctx)
	if err := failure.Load(); err != nil {
		return *err
	}
	return nil
}

type ctxKey int

const (
	userKey ctxKey = iota + 1
	pairingKey
)

func userOf(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

func pairingOf(r *http.Request) *store.Pairing {
	p, _ := r.Context().Value(pairingKey).(*store.Pairing)
	return p
}

// authenticated matches the client certificate to a pairing and the
// pairing to a user, or answers 401.
func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			// Moonlight probes /serverinfo and /pair over HTTPS before
			// pairing; those run unauthenticated.
			if r.URL.Path == "/serverinfo" || r.URL.Path == "/pair" {
				next.ServeHTTP(w, r)
				return
			}
			writeError(w, s.log, 401, errors.New("client certificate required"))
			return
		}
		fp := Fingerprint(r.TLS.PeerCertificates[0])
		pairing, err := s.pairings.Get(r.Context(), fp)
		if err != nil {
			if r.URL.Path == "/serverinfo" || r.URL.Path == "/pair" {
				next.ServeHTTP(w, r)
				return
			}
			writeError(w, s.log, 401, errors.New("client not paired"))
			return
		}
		user, err := s.users.GetByID(r.Context(), pairing.UserID)
		if err != nil || user.Disabled {
			writeError(w, s.log, 401, errors.New("user not found"))
			return
		}
		_ = s.pairings.TouchSeen(r.Context(), fp, time.Now())
		ctx := context.WithValue(r.Context(), userKey, user)
		ctx = context.WithValue(ctx, pairingKey, pairing)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/serverinfo" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Info("moonlight", "method", r.Method, "path", r.URL.Path, "tls", r.TLS != nil, "ip", clientIP(r), "ms", time.Since(start).Milliseconds())
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- handlers ----

func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request) {
	pairStatus, state, currentGame := 0, "SUNSHINE_SERVER_FREE", "0"
	if u := userOf(r); u != nil {
		pairStatus = 1
		if cur, err := s.launcher.Current(r.Context(), u.ID); err == nil && cur != nil {
			state = "SUNSHINE_SERVER_BUSY"
			currentGame = strconv.Itoa(int(cur.MoonlightID))
		}
	}
	sendXML(w, s.log, ServerInfoResponse{
		Response: Response{StatusCode: 200},
		ServerInfo: ServerInfo{
			Hostname:               s.opts.Hostname,
			AppVersion:             "7.1.431.-1",
			GfeVersion:             "3.23.0.74",
			UniqueID:               s.opts.UniqueID,
			MaxLumaPixelsHEVC:      1869449984,
			ServerCodecModeSupport: 65793, // H.264, HEVC and AV1
			HTTPSPort:              s.opts.HTTPSPort,
			ExternalPort:           s.opts.HTTPPort,
			MAC:                    "00:00:00:00:00:00",
			LocalIP:                "127.0.0.1",
			SupportedDisplayModes: DisplayModes{Modes: []DisplayMode{
				{1280, 720, 120}, {1280, 720, 60}, {1280, 720, 30},
				{1920, 1080, 120}, {1920, 1080, 60}, {1920, 1080, 30},
				{2560, 1440, 120}, {2560, 1440, 90}, {2560, 1440, 60},
				{3840, 2160, 120}, {3840, 2160, 90}, {3840, 2160, 60},
			}},
			PairStatus:  pairStatus,
			CurrentGame: currentGame,
			State:       state,
		},
	})
}

// pair multiplexes the pairing phases on the query parameters.
func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("uniqueid")
	if clientID == "" {
		sendXML(w, s.log, failPair(s.log, "uniqueid required"))
		return
	}
	ip := clientIP(r)
	key := keyFor(clientID, ip)
	switch {
	case q.Has("salt"):
		sendXML(w, s.log, s.pairing.phase1(r.Context(), key, ip, q.Get("salt"), q.Get("clientcert")))
	case q.Has("clientchallenge"):
		sendXML(w, s.log, s.pairing.phase2(key, q.Get("clientchallenge")))
	case q.Has("serverchallengeresp"):
		sendXML(w, s.log, s.pairing.phase3(key, q.Get("serverchallengeresp")))
	case q.Has("clientpairingsecret"):
		sendXML(w, s.log, s.pairing.phase4(r.Context(), key, q.Get("clientpairingsecret")))
	case q.Get("phrase") == "pairchallenge":
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			sendXML(w, s.log, failPair(s.log, "client certificate required"))
			return
		}
		sendXML(w, s.log, PairingResponse{Paired: 1, Response: Response{StatusCode: 200}})
	default:
		sendXML(w, s.log, failPair(s.log, "invalid pairing request"))
	}
}

func (s *Server) unpair(w http.ResponseWriter, r *http.Request) {
	s.pairing.Unpair(keyFor(r.URL.Query().Get("uniqueid"), clientIP(r)))
	sendXML(w, s.log, Response{StatusCode: 200})
}

func (s *Server) appList(w http.ResponseWriter, r *http.Request) {
	apps, err := s.launcher.Apps(r.Context(), userOf(r).ID)
	if err != nil {
		writeError(w, s.log, 500, err)
		return
	}
	entries := make([]AppEntry, 0, len(apps))
	for _, a := range apps {
		hdr := 0
		if a.HDR {
			hdr = 1
		}
		entries = append(entries, AppEntry{Title: a.Name, ID: a.MoonlightID, IsHDRSupported: hdr})
	}
	sendXML(w, s.log, AppListResponse{Response: Response{StatusCode: 200}, Apps: entries})
}

// appAsset serves the app's icon: a redirect to its URL, which every
// Moonlight client follows.
func (s *Server) appAsset(w http.ResponseWriter, r *http.Request) {
	app, err := s.appByID(r)
	if err != nil {
		writeError(w, s.log, 404, err)
		return
	}
	if app.IconURL == "" {
		writeError(w, s.log, 404, errors.New("no icon"))
		return
	}
	http.Redirect(w, r, app.IconURL, http.StatusFound)
}

func (s *Server) appByID(r *http.Request) (*store.App, error) {
	id, err := strconv.Atoi(r.URL.Query().Get("appid"))
	if err != nil {
		return nil, errors.New("appid must be an integer")
	}
	apps, err := s.launcher.Apps(r.Context(), userOf(r).ID)
	if err != nil {
		return nil, err
	}
	for _, a := range apps {
		if int(a.MoonlightID) == id {
			return a, nil
		}
	}
	return nil, errors.New("app not found")
}

func (s *Server) launch(w http.ResponseWriter, r *http.Request) { s.launchOrResume(w, r, false) }
func (s *Server) resume(w http.ResponseWriter, r *http.Request) { s.launchOrResume(w, r, true) }

func (s *Server) launchOrResume(w http.ResponseWriter, r *http.Request, resume bool) {
	q := r.URL.Query()
	var app *store.App
	var err error
	if resume && q.Get("appid") == "" {
		// /resume names no app: it continues whatever runs for this user.
		app, err = s.launcher.Current(r.Context(), userOf(r).ID)
		if err == nil && app == nil {
			err = errors.New("nothing to resume")
		}
	} else {
		app, err = s.appByID(r)
	}
	if err != nil {
		writeError(w, s.log, 404, err)
		return
	}
	rikey, rikeyID := q.Get("rikey"), q.Get("rikeyid")
	if rikey == "" || rikeyID == "" {
		writeError(w, s.log, 400, errors.New("rikey and rikeyid are required"))
		return
	}
	width, height, fps, err := parseMode(q.Get("mode"))
	if err != nil {
		writeError(w, s.log, 400, err)
		return
	}
	surround := 196610
	if v := q.Get("surroundAudioInfo"); v != "" {
		if surround, err = strconv.Atoi(v); err != nil {
			writeError(w, s.log, 400, fmt.Errorf("invalid surroundAudioInfo %q", v))
			return
		}
	}
	user, pairing := userOf(r), pairingOf(r)
	stream := store.Stream{
		PairingID: pairing.ID, ClientIP: clientIP(r), AESKey: rikey, AESIV: rikeyID,
		Width: width, Height: height, FPS: fps, Surround: surround,
		StartedAt: time.Now().UTC().Format(time.RFC3339), ClientName: pairing.Name,
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.LaunchTimeout)
	defer cancel()
	url, err := s.launcher.Launch(ctx, user, pairing, app.MoonlightID, stream, resume)
	if err != nil {
		writeError(w, s.log, 500, fmt.Errorf("launch %s: %w", app.Name, err))
		return
	}
	sendXML(w, s.log, LaunchResponse{Response: Response{StatusCode: 200}, RTSPSessionURL: url, GameSession: 1})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if err := s.launcher.Cancel(r.Context(), userOf(r)); err != nil {
		writeError(w, s.log, 500, err)
		return
	}
	sendXML(w, s.log, Response{StatusCode: 200})
}

// parseMode reads "1920x1080x60".
func parseMode(mode string) (width, height, fps int, err error) {
	if mode == "" {
		mode = "1920x1080x60"
	}
	parts := strings.Split(mode, "x")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("invalid mode %q", mode)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		if nums[i], err = strconv.Atoi(p); err != nil || nums[i] <= 0 {
			return 0, 0, 0, fmt.Errorf("invalid mode %q", mode)
		}
	}
	return nums[0], nums[1], nums[2], nil
}

func sendXML(w http.ResponseWriter, log *slog.Logger, resp XMLResponse) {
	b, err := xml.Marshal(resp)
	if err != nil {
		writeError(w, log, 500, fmt.Errorf("marshal response: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(resp.GetStatusCode())
	_, _ = io.WriteString(w, xml.Header)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, log *slog.Logger, status int, err error) {
	log.Warn("moonlight error", "status", status, "err", err)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	if b, merr := xml.Marshal(Response{StatusCode: status, StatusMessage: err.Error()}); merr == nil {
		_, _ = w.Write(b)
		return
	}
	_, _ = fmt.Fprintf(w, `<root status_code="%d"></root>`, status)
}
