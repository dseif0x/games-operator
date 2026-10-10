// Command games-operator is the hub: the web UI and REST API, the
// Moonlight-compatible front door, the Kubernetes reconciler and the
// Wolf-compatible pairing API, in one binary.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dseif0x/games-operator/internal/api"
	"github.com/dseif0x/games-operator/internal/apps"
	"github.com/dseif0x/games-operator/internal/auth"
	"github.com/dseif0x/games-operator/internal/browser"
	"github.com/dseif0x/games-operator/internal/config"
	"github.com/dseif0x/games-operator/internal/k8s"
	"github.com/dseif0x/games-operator/internal/moonlight"
	"github.com/dseif0x/games-operator/internal/reconcile"
	"github.com/dseif0x/games-operator/internal/store"
	"github.com/dseif0x/games-operator/internal/ui"
)

// version is set by the linker from the git tag.
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hash-password":
			os.Exit(hashPassword())
		case "version":
			fmt.Println(version)
			return
		}
	}
	if err := run(); err != nil {
		slog.Error("games-operator exited", "err", err)
		os.Exit(1)
	}
}

// hashPassword prints an argon2id hash for auth.adminPasswordHash. The
// password is read from GAMES_OPERATOR_PASSWORD or stdin.
func hashPassword() int {
	pw := os.Getenv("GAMES_OPERATOR_PASSWORD")
	if pw == "" {
		var line string
		if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
			fmt.Fprintln(os.Stderr, "usage: GAMES_OPERATOR_PASSWORD=... games-operator hash-password  (or pipe the password on stdin)")
			return 2
		}
		pw = line
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(h)
	return 0
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	log.Info("games-operator starting", "version", version, "namespace", cfg.Namespace, "public_url", cfg.PublicURL.String(), "ui", ui.Built)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Storage.
	st, err := store.Open(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer st.Close()
	adminHash := cfg.AdminPasswordHash
	if adminHash == "" && cfg.AdminPassword != "" {
		if adminHash, err = auth.HashPassword(cfg.AdminPassword); err != nil {
			return err
		}
	}
	if adminHash != "" {
		admin, err := st.Users().UpsertPassword(ctx, cfg.AdminUsername, adminHash)
		if err != nil {
			return fmt.Errorf("bootstrap admin user: %w", err)
		}
		if err := st.Users().SetRole(ctx, admin.ID, store.RoleAdmin); err != nil {
			return fmt.Errorf("bootstrap admin role: %w", err)
		}
		log.Info("admin user ready", "username", cfg.AdminUsername)
	}

	// Moonlight server certificate: clients pin it when they pair.
	cert, err := moonlight.LoadOrCreateCert(cfg.CertDir)
	if err != nil {
		return fmt.Errorf("moonlight certificate: %w", err)
	}
	// The hub's own client certificate, with which it is a paired client
	// of every Wolf and drives their streams over Wolf's HTTPS.
	clientCert, err := moonlight.LoadOrCreateClientCert(cfg.CertDir)
	if err != nil {
		return fmt.Errorf("wolf client certificate: %w", err)
	}

	// Kubernetes.
	cs, err := k8s.NewClientset(cfg.Kubeconfig)
	if err != nil {
		return err
	}
	inf := k8s.NewInformers(cs, cfg.Namespace, 10*time.Minute)

	// Metrics.
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// Wiring.
	rcfg := reconcile.Config{
		Namespace: cfg.Namespace, WolfImage: cfg.WolfImage, InitImage: cfg.InitImage, BridgeImage: cfg.BridgeImageRef(),
		ImagePullPolicy: cfg.ImagePullPolicy, RuntimeClass: cfg.RuntimeClass, WolfGPURequest: cfg.WolfGPURequest, UinputResource: cfg.UinputResource,
		RenderNode: cfg.RenderNode, TimeZone: cfg.TimeZone, MoonlightHostname: cfg.MoonlightHostname,
		ClientCert: clientCert, ClientCertPEM: moonlight.CertPEM(clientCert),
		DefaultStorageClass: cfg.DefaultStorageClass, DefaultPVCSize: cfg.DefaultPVCSize, DefaultResources: cfg.DefaultResources, MaxResources: cfg.MaxResources,
		NodeSelector: cfg.NodeSelector, Tolerations: cfg.Tolerations, ExtraEnv: cfg.ExtraEnv,
		LBSharingKey: cfg.LBSharingKey, LBIP: cfg.LBIP, StreamPortBase: cfg.StreamPortBase, MaxConcurrent: cfg.MaxConcurrent,
	}
	svc := &apps.Service{
		Store: st, Broker: apps.NewBroker(), IdleStopAfter: cfg.IdleStopAfter, Log: log,
		Defaults: apps.Defaults{
			PVCSize: cfg.DefaultPVCSize, StorageClass: cfg.DefaultStorageClass, Resources: cfg.DefaultResources, MaxResources: cfg.MaxResources,
			MaxConcurrent: cfg.MaxConcurrent, BrowserURL: cfg.BrowserURL, MoonlightHost: cfg.LBIP,
			MaxApps: cfg.DefaultMaxApps, MaxStorage: cfg.DefaultMaxStorage,
		},
	}
	rec := reconcile.New(rcfg, st, cs, inf, svc, cfg.ReconcileInterval, log)
	svc.Orch = rec

	pm := moonlight.NewPairingManager(cert, st.Pairings(), log)
	ml := moonlight.NewServer(moonlight.Options{
		HTTPPort: cfg.MoonlightHTTPPort, HTTPSPort: cfg.MoonlightHTTPSPort, Hostname: cfg.MoonlightHostname,
		UniqueID: hostUniqueID(cfg), Cert: cert,
	}, pm, st.Users(), st.Pairings(), svc, log)

	srv := &api.Server{
		Cfg: cfg, Store: st, Apps: svc, Pairing: pm,
		Auth:    auth.PasswordAuthenticator{Users: st.Users()},
		Cookies: auth.NewSessions(cfg.CookieSecret, cfg.Secure(), st.Users()),
		Limiter: auth.NewRateLimiter(10, 15*time.Minute),
		Ready: func() bool {
			pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return inf.Synced() && st.Ping(pctx) == nil
		},
		UI:      ui.Handler(cfg.BasePath),
		Metrics: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
		Log:     log,
	}
	if cfg.BrowserUpstream != nil {
		srv.Browser = browser.New(browser.Options{
			Upstream: cfg.BrowserUpstream, Prefix: cfg.BrowserPath, Secret: cfg.BrowserSecret,
			Authenticated: srv.Authenticated, LoginPath: cfg.BasePath + "/login", Log: log,
		})
		log.Info("proxying moonlight-web", "upstream", cfg.BrowserUpstream.String(), "path", cfg.BrowserPath)
	}

	// Listen first: /healthz answers while the informers sync, /readyz
	// stays 503 until they have. A missing RBAC rule then shows up as a
	// pod that is up but not ready, with the reason in the log, instead of
	// a restart loop.
	errc := make(chan error, 2)
	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	if err := waitForInformers(ctx, inf, log); err != nil {
		return err
	}

	go rec.Run(ctx)
	go svc.RunPoller(ctx, cfg.StatusPollEvery)
	go func() {
		if err := ml.Run(ctx); err != nil {
			errc <- fmt.Errorf("moonlight: %w", err)
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return httpSrv.Shutdown(sctx)
}

// hostUniqueID is the id Moonlight uses to tell hosts apart: derived from
// the public URL so it stays the same across restarts.
func hostUniqueID(cfg *config.Config) string {
	h := auth.HashToken("games-operator:" + cfg.PublicURL.Host)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// waitForInformers starts the informers and blocks until their caches are
// in sync, retrying for as long as the process runs. The usual cause of a
// long wait is a missing RBAC rule; client-go logs the forbidden error.
func waitForInformers(ctx context.Context, inf *k8s.Informers, log *slog.Logger) error {
	inf.Start(ctx)
	for {
		sctx, cancel := context.WithTimeout(ctx, time.Minute)
		err := inf.WaitForSync(sctx)
		cancel()
		if err == nil {
			log.Info("informers synced")
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Warn("informers not synced yet; check the hub's RBAC", "err", err)
	}
}
