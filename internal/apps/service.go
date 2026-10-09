// Package apps holds the business logic: create, update, launch, stop and
// delete apps, the Moonlight launcher, the idle policy and the views the
// API returns. It calls the store and the orchestrator; it never talks to
// Kubernetes itself.
package apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/dseif0x/games-operator/internal/config"
	"github.com/dseif0x/games-operator/internal/preset"
	"github.com/dseif0x/games-operator/internal/reconcile"
	"github.com/dseif0x/games-operator/internal/store"
)

// ErrInvalidTransition is returned when an action does not apply to the
// app's current state.
var ErrInvalidTransition = errors.New("action not allowed in the app's current state")

// ErrNoSlot is returned when every stream slot is taken.
var ErrNoSlot = errors.New("all stream slots are in use; stop another app first")

// ValidationError describes a bad request.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Defaults are applied to create requests that leave fields empty and
// shown on the form.
type Defaults struct {
	PVCSize       string           `json:"pvc_size"`
	StorageClass  string           `json:"storage_class"`
	Resources     config.Resources `json:"resources"`
	MaxResources  config.Resources `json:"max_resources"`
	MaxConcurrent int              `json:"max_concurrent"`
	// BrowserURL is the moonlight-web instance for "Play in browser".
	BrowserURL string `json:"browser_url"`
	// MoonlightHost is the address Moonlight clients add as a host.
	MoonlightHost string `json:"moonlight_host"`
}

// Service is the app business logic.
type Service struct {
	Store         store.Store
	Orch          reconcile.Orchestrator
	Broker        *Broker
	Defaults      Defaults
	IdleStopAfter time.Duration
	Log           *slog.Logger
	now           func() time.Time
}

// CreateRequest is the JSON body of POST /apps and PATCH /apps/{id}.
type CreateRequest struct {
	Name         string            `json:"name"`
	Preset       string            `json:"preset"`
	Image        string            `json:"image"`
	IconURL      string            `json:"icon_url"`
	HDR          bool              `json:"hdr"`
	Command      string            `json:"command"`
	PVCSize      string            `json:"pvc_size"`
	StorageClass string            `json:"storage_class"`
	Resources    config.Resources  `json:"resources"`
	Env          map[string]string `json:"env"`
	HostIPC      *bool             `json:"host_ipc"`
	Capabilities []string          `json:"capabilities"`
}

// View is the API representation of an app.
type View struct {
	ID           string            `json:"id"`
	MoonlightID  int32             `json:"moonlight_id"`
	Name         string            `json:"name"`
	Preset       string            `json:"preset"`
	Image        string            `json:"image"`
	IconURL      string            `json:"icon_url"`
	HDR          bool              `json:"hdr"`
	Command      string            `json:"command"`
	PVCSize      string            `json:"pvc_size"`
	StorageClass string            `json:"storage_class"`
	Resources    config.Resources  `json:"resources"`
	Env          map[string]string `json:"env"`
	HostIPC      bool              `json:"host_ipc"`
	Capabilities []string          `json:"capabilities"`
	State        string            `json:"state"`
	StateReason  string            `json:"state_reason"`
	Slot         int               `json:"slot"`
	StreamURL    string            `json:"stream_url"`
	// Streaming is true once Wolf has a session for a client; a running
	// app without one is warm and waiting for a Moonlight launch.
	Streaming    bool        `json:"streaming"`
	Stream       *StreamView `json:"stream,omitempty"`
	PodName      string      `json:"pod_name"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
	LastActiveAt *time.Time  `json:"last_active_at"`
}

// StreamView is the non-secret part of the current stream.
type StreamView struct {
	ClientIP   string `json:"client_ip"`
	ClientName string `json:"client_name"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	FPS        int    `json:"fps"`
	StartedAt  string `json:"started_at"`
}

// View renders an app.
func (s *Service) View(a *store.App) View {
	v := View{
		ID: a.ID, MoonlightID: a.MoonlightID, Name: a.Name, Preset: a.Preset, Image: a.Image, IconURL: a.IconURL, HDR: a.HDR,
		Command: a.Command, PVCSize: a.PVCSize, StorageClass: a.StorageClass, Resources: a.Resources, Env: a.Env, HostIPC: a.HostIPC,
		Capabilities: a.Capabilities, State: a.State, StateReason: a.StateReason, Slot: a.Slot, StreamURL: a.StreamURL,
		Streaming: a.State == store.StateRunning && a.WolfSessionID != "" && a.Stream != nil,
		PodName:   reconcile.ObjectName(a.ID), CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, LastActiveAt: a.LastActiveAt,
	}
	if v.Env == nil {
		v.Env = map[string]string{}
	}
	if v.Capabilities == nil {
		v.Capabilities = []string{}
	}
	if a.Stream != nil && a.State != store.StateStopped {
		v.Stream = &StreamView{ClientIP: a.Stream.ClientIP, ClientName: a.Stream.ClientName, Width: a.Stream.Width, Height: a.Stream.Height, FPS: a.Stream.FPS, StartedAt: a.Stream.StartedAt}
	}
	return v
}

// Presets lists the available presets.
func (s *Service) Presets() []preset.Preset { return preset.All() }

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

var (
	nameRE     = regexp.MustCompile(`^[\pL\pN][\pL\pN ._'-]{0,62}$`)
	envKeyRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	capRE      = regexp.MustCompile(`^[A-Z_]{3,32}$`)
	dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// reservedEnv cannot be overridden per app.
var reservedEnv = map[string]bool{
	"XDG_RUNTIME_DIR": true, "WAYLAND_DISPLAY": true, "PULSE_SERVER": true, "PUID": true, "PGID": true, "UNAME": true, "HOME": true, "PATH": true,
}

var extendedResourceRE = regexp.MustCompile(`^(hugepages-[A-Za-z0-9]+|([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z0-9]([-a-z0-9]*[a-z0-9])?/[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)$`)

// validate checks and normalises a request in place.
func (s *Service) validate(req *CreateRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	if !nameRE.MatchString(req.Name) {
		return &ValidationError{"name must be 1-63 characters: letters, digits, space, dot, apostrophe, underscore or dash"}
	}
	p, ok := preset.Get(req.Preset)
	if !ok {
		return &ValidationError{"preset must be one of " + strings.Join(preset.Keys(), ", ")}
	}
	req.Image = strings.TrimSpace(req.Image)
	if req.Image == "" {
		req.Image = p.Image
	}
	if req.Image == "" || strings.ContainsAny(req.Image, " \t\n") {
		return &ValidationError{"image is required for the custom preset"}
	}
	req.IconURL = strings.TrimSpace(req.IconURL)
	if req.IconURL == "" {
		req.IconURL = p.IconURL
	}
	if req.IconURL != "" {
		u, err := url.Parse(req.IconURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return &ValidationError{"icon_url must be an http(s) URL"}
		}
	}
	if req.PVCSize != "" {
		if q, err := resource.ParseQuantity(req.PVCSize); err != nil || q.Sign() <= 0 {
			return &ValidationError{"invalid pvc_size " + req.PVCSize}
		}
	}
	if req.StorageClass != "" && !dnsLabelRE.MatchString(req.StorageClass) {
		return &ValidationError{"invalid storage_class"}
	}
	for _, q := range []string{req.Resources.Requests.CPU, req.Resources.Requests.Memory, req.Resources.Limits.CPU, req.Resources.Limits.Memory} {
		if q != "" {
			if _, err := resource.ParseQuantity(q); err != nil {
				return &ValidationError{"invalid resource quantity " + q}
			}
		}
	}
	if len(req.Resources.Requests.Extended) > 0 {
		return &ValidationError{"extended resources go under limits only"}
	}
	for name, v := range req.Resources.Limits.Extended {
		if !extendedResourceRE.MatchString(name) {
			return &ValidationError{"invalid extended resource name " + name}
		}
		q, err := resource.ParseQuantity(v)
		if err != nil || q.Sign() < 0 || (q.MilliValue()%1000 != 0 && !strings.HasPrefix(name, "hugepages-")) {
			return &ValidationError{"extended resource " + name + " must be a whole number"}
		}
	}
	for k := range req.Env {
		if !envKeyRE.MatchString(k) {
			return &ValidationError{"invalid env name " + k}
		}
		if reservedEnv[k] {
			return &ValidationError{"env " + k + " is managed by games-operator"}
		}
	}
	for _, c := range req.Capabilities {
		if !capRE.MatchString(c) {
			return &ValidationError{"invalid capability " + c}
		}
	}
	if len(req.Command) > 8192 {
		return &ValidationError{"command is too long"}
	}
	return nil
}

func (s *Service) apply(a *store.App, req CreateRequest) {
	p, _ := preset.Get(req.Preset)
	a.Name, a.Preset, a.Image, a.IconURL, a.HDR, a.Command = req.Name, req.Preset, req.Image, req.IconURL, req.HDR, strings.TrimSpace(req.Command)
	a.Resources = req.Resources
	a.Env = req.Env
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	a.HostIPC = p.HostIPC
	if req.HostIPC != nil {
		a.HostIPC = *req.HostIPC
	}
	a.Capabilities = req.Capabilities
	if a.Capabilities == nil {
		a.Capabilities = []string{}
	}
}

// MoonlightID derives a stable, positive app id Moonlight can use from
// the app's UUID (djb2, as Wolf and fenrir do for titles).
func MoonlightID(appID string) int32 {
	var h uint32 = 5381
	for _, c := range appID {
		h = (h << 5) + h + uint32(c)
	}
	id := int32(h & 0x7fffffff) //nolint:gosec // masked to 31 bits
	if id == 0 {
		id = 1
	}
	return id
}

// Create validates and stores a new app in the stopped state.
func (s *Service) Create(ctx context.Context, user *store.User, req CreateRequest) (*store.App, error) {
	if err := s.validate(&req); err != nil {
		return nil, err
	}
	a := &store.App{OwnerID: user.ID, PVCSize: req.PVCSize, StorageClass: req.StorageClass, State: store.StateStopped, Slot: -1}
	if a.PVCSize == "" {
		a.PVCSize = s.Defaults.PVCSize
	}
	if a.StorageClass == "" {
		a.StorageClass = s.Defaults.StorageClass
	}
	s.apply(a, req)
	for attempt := 0; attempt < 5; attempt++ {
		a.ID = store.NewID()
		a.MoonlightID = MoonlightID(a.ID)
		err := s.Store.Apps().Create(ctx, a)
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrConflict) || attempt == 4 {
			return nil, err
		}
	}
	_ = s.Store.Events().Add(ctx, a.ID, "user", "created")
	s.Log.Info("app created", "app", a.ID, "owner", user.ID, "name", a.Name, "preset", a.Preset)
	s.publish(ctx, a)
	return a, nil
}

// Get returns the app if it belongs to ownerID.
func (s *Service) Get(ctx context.Context, ownerID, id string) (*store.App, error) {
	a, err := s.Store.Apps().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.OwnerID != ownerID {
		return nil, store.ErrNotFound
	}
	return a, nil
}

// List returns the owner's apps.
func (s *Service) List(ctx context.Context, ownerID string) ([]*store.App, error) {
	return s.Store.Apps().List(ctx, ownerID)
}

// Update replaces a stopped or failed app's settings.
func (s *Service) Update(ctx context.Context, user *store.User, id string, req CreateRequest) (*store.App, error) {
	a, err := s.Get(ctx, user.ID, id)
	if err != nil {
		return nil, err
	}
	if a.State != store.StateStopped && a.State != store.StateFailed {
		return nil, ErrInvalidTransition
	}
	if err := s.validate(&req); err != nil {
		return nil, err
	}
	s.apply(a, req)
	updated, err := s.Store.Apps().Update(ctx, a)
	if err != nil {
		return nil, err
	}
	_ = s.Store.Events().Add(ctx, a.ID, "user", "settings updated")
	s.publish(ctx, updated)
	return updated, nil
}

// Stop ends a running or starting app; the pod goes, the home PVC stays.
func (s *Service) Stop(ctx context.Context, user *store.User, id string) (*store.App, error) {
	a, err := s.Get(ctx, user.ID, id)
	if err != nil {
		return nil, err
	}
	return s.stop(ctx, a, "user")
}

func (s *Service) stop(ctx context.Context, a *store.App, by string) (*store.App, error) {
	switch a.State {
	case store.StateRunning, store.StateStarting, store.StateFailed:
	default:
		return nil, ErrInvalidTransition
	}
	updated, err := s.Store.Apps().SetState(ctx, a.ID, store.StateStopping, "")
	if err != nil {
		return nil, err
	}
	_ = s.Store.Events().Add(ctx, a.ID, by, "stop requested")
	s.Orch.Notify(a.ID)
	s.publish(ctx, updated)
	return updated, nil
}

// Delete removes the app and everything it owns.
func (s *Service) Delete(ctx context.Context, user *store.User, id string) (*store.App, error) {
	a, err := s.Get(ctx, user.ID, id)
	if err != nil {
		return nil, err
	}
	updated, err := s.Store.Apps().SetState(ctx, a.ID, store.StateDeleting, "")
	if err != nil {
		return nil, err
	}
	_ = s.Store.Events().Add(ctx, a.ID, "user", "delete requested")
	s.Orch.Notify(a.ID)
	s.publish(ctx, updated)
	return updated, nil
}

// Events lists the app's event log.
func (s *Service) Events(ctx context.Context, ownerID, id string) ([]*store.Event, error) {
	if _, err := s.Get(ctx, ownerID, id); err != nil {
		return nil, err
	}
	return s.Store.Events().List(ctx, id, store.EventsKeep)
}

// Logs returns a container's recent logs.
func (s *Service) Logs(ctx context.Context, ownerID, id, container string) ([]byte, error) {
	if _, err := s.Get(ctx, ownerID, id); err != nil {
		return nil, err
	}
	switch container {
	case "", reconcile.ContainerApp, reconcile.ContainerWolf, reconcile.ContainerBridge, reconcile.ContainerInit:
	default:
		return nil, &ValidationError{"unknown container " + container}
	}
	return s.Orch.PodLogs(ctx, id, container, 500)
}

// ---- Moonlight launcher ----

// Apps implements moonlight.Launcher.
func (s *Service) Apps(ctx context.Context, userID string) ([]*store.App, error) {
	return s.Store.Apps().List(ctx, userID)
}

// Current implements moonlight.Launcher.
func (s *Service) Current(ctx context.Context, userID string) (*store.App, error) {
	list, err := s.Store.Apps().List(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		if a.State == store.StateRunning || a.State == store.StateStarting {
			return a, nil
		}
	}
	return nil, nil
}

// LaunchPoll is how often Launch checks for the stream URL.
var LaunchPoll = 500 * time.Millisecond

// Launch implements moonlight.Launcher: it moves the app to starting (or
// re-keys a running one), frees the slot of any other app the user runs,
// and waits for the reconciler to report the stream URL.
func (s *Service) Launch(ctx context.Context, user *store.User, pairing *store.Pairing, moonlightID int32, stream store.Stream, resume bool) (string, error) {
	a, err := s.Store.Apps().GetByMoonlightID(ctx, user.ID, moonlightID)
	if err != nil {
		return "", err
	}
	stream.PairingID = pairing.ID
	stream.ClientName = pairing.Name
	switch a.State {
	case store.StateRunning:
		// Same app again: new keys for a new connection (resume, or a
		// launch after the client dropped). Wolf gets a fresh session.
		if _, err := s.Store.Apps().SetStream(ctx, a.ID, &stream); err != nil {
			return "", err
		}
		_ = s.Store.Events().Add(ctx, a.ID, "moonlight", "stream re-keyed for "+stream.ClientIP)
		s.Orch.Notify(a.ID)
	case store.StateStarting:
		// Still coming up (a node waking, an image pulling). Clients give up
		// on /launch long before that and try again later, so take this
		// attempt's keys: the stream that eventually comes up must match the
		// client that is waiting now, not the one that went away.
		if _, err := s.Store.Apps().SetStream(ctx, a.ID, &stream); err != nil {
			return "", err
		}
		_ = s.Store.Events().Add(ctx, a.ID, "moonlight", "launch retried by "+stream.ClientIP+"; keys updated, still starting")
	case store.StateStopped, store.StateFailed:
		// One stream per user: stop whatever else runs.
		list, err := s.Store.Apps().List(ctx, user.ID)
		if err != nil {
			return "", err
		}
		for _, other := range list {
			if other.ID != a.ID && (other.State == store.StateRunning || other.State == store.StateStarting) {
				if _, err := s.stop(ctx, other, "moonlight"); err != nil && !errors.Is(err, ErrInvalidTransition) {
					return "", err
				}
			}
		}
		slot, err := s.freeSlot(ctx)
		if err != nil {
			return "", err
		}
		launched, err := s.Store.Apps().Launch(ctx, a.ID, &stream, slot)
		if err != nil {
			return "", err
		}
		_ = s.Store.Events().Add(ctx, a.ID, "moonlight", fmt.Sprintf("launch by %s (%dx%d@%d, slot %d, resume=%v)", stream.ClientIP, stream.Width, stream.Height, stream.FPS, slot, resume))
		s.Orch.Notify(a.ID)
		s.publish(ctx, launched)
	default:
		return "", fmt.Errorf("app is %s", a.State)
	}
	return s.waitForStream(ctx, a.ID)
}

// Start warms an app up without a client: the pod comes up and Wolf waits,
// so a later /launch streams at once instead of running into the client's
// timeout. The idle stop reclaims it if no client follows. One app per
// user runs at a time, as with Launch.
func (s *Service) Start(ctx context.Context, user *store.User, id string) (*store.App, error) {
	a, err := s.Get(ctx, user.ID, id)
	if err != nil {
		return nil, err
	}
	switch a.State {
	case store.StateStopped, store.StateFailed:
	case store.StateStarting, store.StateRunning:
		return a, nil
	default:
		return nil, ErrInvalidTransition
	}
	list, err := s.Store.Apps().List(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	for _, other := range list {
		if other.ID != a.ID && (other.State == store.StateRunning || other.State == store.StateStarting) {
			if _, err := s.stop(ctx, other, "user"); err != nil && !errors.Is(err, ErrInvalidTransition) {
				return nil, err
			}
		}
	}
	slot, err := s.freeSlot(ctx)
	if err != nil {
		return nil, err
	}
	started, err := s.Store.Apps().Launch(ctx, a.ID, nil, slot)
	if err != nil {
		return nil, err
	}
	_ = s.Store.Events().Add(ctx, a.ID, "user", fmt.Sprintf("started without a client (slot %d); waiting for Moonlight", slot))
	s.Orch.Notify(a.ID)
	s.publish(ctx, started)
	return started, nil
}

// waitForStream polls the row until the reconciler has registered the
// stream with Wolf.
func (s *Service) waitForStream(ctx context.Context, id string) (string, error) {
	t := time.NewTicker(LaunchPoll)
	defer t.Stop()
	for {
		a, err := s.Store.Apps().Get(ctx, id)
		if err != nil {
			return "", err
		}
		switch a.State {
		case store.StateRunning:
			if a.WolfSessionID != "" && a.StreamURL != "" {
				return a.StreamURL, nil
			}
		case store.StateFailed:
			return "", errors.New(a.StateReason)
		case store.StateStopping, store.StateStopped, store.StateDeleting:
			return "", errors.New("app was stopped")
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for the stream (%s)", a.StateReason)
		case <-t.C:
		}
	}
}

// freeSlot returns the lowest unused stream slot.
func (s *Service) freeSlot(ctx context.Context) (int, error) {
	used, err := s.Store.Apps().UsedSlots(ctx)
	if err != nil {
		return -1, err
	}
	taken := map[int]bool{}
	for _, u := range used {
		taken[u] = true
	}
	max := s.Defaults.MaxConcurrent
	if max <= 0 {
		max = 1
	}
	for i := 0; i < max; i++ {
		if !taken[i] {
			return i, nil
		}
	}
	return -1, ErrNoSlot
}

// Cancel implements moonlight.Launcher.
func (s *Service) Cancel(ctx context.Context, user *store.User) error {
	cur, err := s.Current(ctx, user.ID)
	if err != nil || cur == nil {
		return err
	}
	// /cancel ends the client's stream, never the app. Clients send it for
	// every reason: a launch they gave up waiting for, a codec or transport
	// fallback a second before they re-launch, and the user's Quit. Stopping
	// the pod on any of those throws away the node that just woke up or the
	// game that is running; the idle stop reclaims an app nobody comes back
	// to, and the hub's Stop button is explicit. The stream parameters are
	// cleared so the app shows as ready, not streaming; Wolf's session stays
	// until the next launch replaces it.
	if cur.State == store.StateStarting {
		_ = s.Store.Events().Add(ctx, cur.ID, "moonlight", "cancel while starting ignored; the app keeps starting")
		return nil
	}
	if cur.Stream != nil {
		if _, err := s.Store.Apps().SetStream(ctx, cur.ID, nil); err != nil {
			return err
		}
		_ = s.Store.Events().Add(ctx, cur.ID, "moonlight", "stream ended by the client; the app keeps running")
		s.Orch.Notify(cur.ID)
		if updated, err := s.Store.Apps().Get(ctx, cur.ID); err == nil {
			s.publish(ctx, updated)
		}
	}
	return nil
}

// ---- notifier ----

func (s *Service) publish(ctx context.Context, a *store.App) {
	v := s.View(a)
	s.Broker.Publish(a.OwnerID, Event{Type: "app", ID: a.ID, App: &v})
	_ = ctx
}

// AppChanged implements reconcile.Notifier.
func (s *Service) AppChanged(ctx context.Context, a *store.App) { s.publish(ctx, a) }

// AppDeleted implements reconcile.Notifier.
func (s *Service) AppDeleted(_ context.Context, appID, ownerID string) {
	s.Broker.Publish(ownerID, Event{Type: "deleted", ID: appID})
}

// ---- idle policy ----

// RunPoller stops running apps nobody streams to, every interval.
func (s *Service) RunPoller(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 10 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.PollOnce(ctx)
		}
	}
}

// PollOnce checks every running app's bridge once. An app whose stream
// has been paused or stopped for longer than IdleStopAfter is stopped.
func (s *Service) PollOnce(ctx context.Context) {
	list, err := s.Store.Apps().ListAll(ctx)
	if err != nil {
		s.Log.Warn("idle poll: list apps", "err", err)
		return
	}
	for _, a := range list {
		if a.State != store.StateRunning {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := s.Orch.BridgeStatus(pctx, a.ID)
		cancel()
		if err != nil {
			s.Log.Debug("idle poll: bridge status", "app", a.ID, "err", err)
			continue
		}
		now := s.clock()
		if st.Streaming {
			_ = s.Store.Apps().TouchActive(ctx, a.ID, now)
			continue
		}
		if s.IdleStopAfter <= 0 {
			continue
		}
		last := a.UpdatedAt
		if a.LastActiveAt != nil && a.LastActiveAt.After(last) {
			last = *a.LastActiveAt
		}
		if !st.Since.IsZero() && st.Since.After(last) {
			last = st.Since
		}
		if idle := now.Sub(last); idle > s.IdleStopAfter {
			s.Log.Info("stopping idle app", "app", a.ID, "idle", idle.Round(time.Second))
			if _, err := s.stop(ctx, a, "idle"); err != nil && !errors.Is(err, ErrInvalidTransition) {
				s.Log.Warn("idle stop failed", "app", a.ID, "err", err)
			}
		}
	}
}

// SortedCapabilities returns a copy sorted for stable output.
func SortedCapabilities(c []string) []string {
	out := append([]string{}, c...)
	sort.Strings(out)
	return out
}
