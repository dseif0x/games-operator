package apps

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dseif0x/games-operator/internal/bridge"
	"github.com/dseif0x/games-operator/internal/config"
	"github.com/dseif0x/games-operator/internal/store"
)

// fakeOrch plays the reconciler: on Notify it moves a starting app to
// running with a stream URL, and a stopping app to stopped.
type fakeOrch struct {
	st       store.Store
	mu       sync.Mutex
	notified []string
	status   bridge.Status
	// slow leaves starting apps starting, like a node that is still booting.
	slow bool
}

func (f *fakeOrch) Notify(id string) {
	f.mu.Lock()
	f.notified = append(f.notified, id)
	f.mu.Unlock()
	ctx := context.Background()
	a, err := f.st.Apps().Get(ctx, id)
	if err != nil {
		return
	}
	switch a.State {
	case store.StateStarting:
		if f.slow {
			return
		}
		if a.Stream == nil {
			_, _ = f.st.Apps().SetState(ctx, id, store.StateRunning, "ready, waiting for a Moonlight client")
			return
		}
		_, _ = f.st.Apps().SetRuntime(ctx, id, "wolf-1", "rtsp://10.13.254.9:48100")
		_, _ = f.st.Apps().SetState(ctx, id, store.StateRunning, "")
	case store.StateRunning:
		if a.WolfSessionID == "" && a.Stream != nil {
			url := a.StreamURL
			if url == "" { // first stream of a warm app
				url = "rtsp://10.13.254.9:48100"
			}
			_, _ = f.st.Apps().SetRuntime(ctx, id, "wolf-2", url)
		}
	case store.StateStopping:
		_, _ = f.st.Apps().ClearRuntime(ctx, id)
		_, _ = f.st.Apps().SetState(ctx, id, store.StateStopped, "")
	}
}
func (f *fakeOrch) PodLogs(context.Context, string, string, int64) ([]byte, error) {
	return []byte("logs"), nil
}
func (f *fakeOrch) BridgeStatus(context.Context, string) (bridge.Status, error) { return f.status, nil }

func newService(t *testing.T) (*Service, *store.User, *fakeOrch) {
	t.Helper()
	st := store.NewMemory()
	u, err := st.Users().UpsertPassword(context.Background(), "alice", "x")
	if err != nil {
		t.Fatal(err)
	}
	orch := &fakeOrch{st: st}
	s := &Service{Store: st, Orch: orch, Broker: NewBroker(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Defaults: Defaults{PVCSize: "50Gi", StorageClass: "nfs-fast", MaxConcurrent: 2,
			Resources: config.Resources{Limits: config.ResourceList{CPU: "8", Memory: "16Gi", Extended: map[string]string{"nvidia.com/gpu": "1"}}}}}
	return s, u, orch
}

func TestMoonlightID(t *testing.T) {
	a, b := MoonlightID("11111111-2222-3333-4444-555555555555"), MoonlightID("other")
	if a <= 0 || b <= 0 || a == b {
		t.Fatal(a, b)
	}
	first, again := MoonlightID("x"), MoonlightID("x")
	if first != again {
		t.Fatal("not stable")
	}
}

func TestCreateValidation(t *testing.T) {
	s, u, _ := newService(t)
	ctx := context.Background()
	cases := []struct {
		req  CreateRequest
		want string
	}{
		{CreateRequest{Name: "", Preset: "steam"}, "name"},
		{CreateRequest{Name: "x", Preset: "nope"}, "preset"},
		{CreateRequest{Name: "x", Preset: "custom"}, "image"},
		{CreateRequest{Name: "x", Preset: "steam", IconURL: "ftp://x"}, "icon_url"},
		{CreateRequest{Name: "x", Preset: "steam", Env: map[string]string{"HOME": "/x"}}, "managed"},
		{CreateRequest{Name: "x", Preset: "steam", Resources: config.Resources{Limits: config.ResourceList{Extended: map[string]string{"nvidia.com/gpu": "0.5"}}}}, "whole"},
		{CreateRequest{Name: "x", Preset: "steam", Capabilities: []string{"sys_admin"}}, "capability"},
	}
	for _, c := range cases {
		_, err := s.Create(ctx, u, c.req)
		var ve *ValidationError
		if !errors.As(err, &ve) || !strings.Contains(ve.Msg, c.want) {
			t.Errorf("%+v: got %v, want error mentioning %q", c.req, err, c.want)
		}
	}
	a, err := s.Create(ctx, u, CreateRequest{Name: "Steam", Preset: "steam"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Image != "ghcr.io/games-on-whales/steam:edge" || !a.HostIPC || a.PVCSize != "50Gi" || a.State != store.StateStopped || a.MoonlightID <= 0 {
		t.Fatalf("defaults: %+v", a)
	}
	v := s.View(a)
	if v.PodName != "games-operator-"+a.ID || v.Stream != nil {
		t.Fatalf("view: %+v", v)
	}
}

func TestLaunchStopDelete(t *testing.T) {
	s, u, orch := newService(t)
	ctx := context.Background()
	a, _ := s.Create(ctx, u, CreateRequest{Name: "Steam", Preset: "steam"})
	b, _ := s.Create(ctx, u, CreateRequest{Name: "Firefox", Preset: "firefox"})
	pairing := &store.Pairing{ID: "fp", UserID: u.ID, Name: "phone"}
	stream := store.Stream{ClientIP: "10.0.0.5", AESKey: "k", AESIV: "iv", Width: 1920, Height: 1080, FPS: 60, StartedAt: time.Now().Format(time.RFC3339)}

	url, err := s.Launch(ctx, u, pairing, a.MoonlightID, stream, false)
	if err != nil || url != "rtsp://10.13.254.9:48100" {
		t.Fatalf("launch: %v %q", err, url)
	}
	cur, _ := s.Current(ctx, u.ID)
	if cur == nil || cur.ID != a.ID || cur.Slot != 0 {
		t.Fatalf("current: %+v", cur)
	}
	// Launching another app for the same user stops the first one.
	if _, err := s.Launch(ctx, u, pairing, b.MoonlightID, stream, false); err != nil {
		t.Fatal(err)
	}
	ra, _ := s.Get(ctx, u.ID, a.ID)
	rb, _ := s.Get(ctx, u.ID, b.ID)
	if ra.State != store.StateStopped || rb.State != store.StateRunning || rb.Slot != 0 {
		t.Fatalf("after second launch: a=%s b=%s slot=%d", ra.State, rb.State, rb.Slot)
	}
	// Resume re-keys the running app without a new slot.
	if _, err := s.Launch(ctx, u, pairing, b.MoonlightID, stream, true); err != nil {
		t.Fatal(err)
	}
	rb, _ = s.Get(ctx, u.ID, b.ID)
	if rb.WolfSessionID != "wolf-2" {
		t.Fatalf("re-key: %+v", rb)
	}
	// Edits are refused while running.
	if _, err := s.Update(ctx, u, b.ID, CreateRequest{Name: "Firefox 2", Preset: "firefox"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("update running: %v", err)
	}
	if err := s.Cancel(ctx, u); err != nil {
		t.Fatal(err)
	}
	rb, _ = s.Get(ctx, u.ID, b.ID)
	if rb.State != store.StateStopped || rb.Slot != -1 {
		t.Fatalf("after cancel: %+v", rb)
	}
	if _, err := s.Stop(ctx, u, b.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stop stopped: %v", err)
	}
	if _, err := s.Delete(ctx, u, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "someone-else", a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("other users must not see the app")
	}
	if len(orch.notified) == 0 {
		t.Fatal("orchestrator never notified")
	}
}

func TestIdleStop(t *testing.T) {
	s, u, orch := newService(t)
	ctx := context.Background()
	s.IdleStopAfter = 15 * time.Minute
	a, _ := s.Create(ctx, u, CreateRequest{Name: "Steam", Preset: "steam"})
	pairing := &store.Pairing{ID: "fp", UserID: u.ID}
	if _, err := s.Launch(ctx, u, pairing, a.MoonlightID, store.Stream{Width: 1, Height: 1, FPS: 1}, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.now = func() time.Time { return now.Add(20 * time.Minute) }
	orch.status = bridge.Status{Ready: true, Streaming: true}
	s.PollOnce(ctx)
	if cur, _ := s.Get(ctx, u.ID, a.ID); cur.State != store.StateRunning {
		t.Fatal("streaming app must stay running")
	}
	// The stream ended at +22m; at +40m the app has been idle for 18m.
	orch.status = bridge.Status{Ready: true, Streaming: false, Since: now.Add(22 * time.Minute)}
	s.now = func() time.Time { return now.Add(30 * time.Minute) }
	s.PollOnce(ctx)
	if cur, _ := s.Get(ctx, u.ID, a.ID); cur.State != store.StateRunning {
		t.Fatal("8 minutes idle is under the threshold")
	}
	s.now = func() time.Time { return now.Add(40 * time.Minute) }
	s.PollOnce(ctx)
	if cur, _ := s.Get(ctx, u.ID, a.ID); cur.State != store.StateStopped {
		t.Fatalf("idle app should be stopped, is %s", cur.State)
	}
}

func TestSlotsExhausted(t *testing.T) {
	s, u, _ := newService(t)
	ctx := context.Background()
	s.Defaults.MaxConcurrent = 1
	other, _ := s.Store.Users().UpsertPassword(ctx, "bob", "x")
	a, _ := s.Create(ctx, u, CreateRequest{Name: "A", Preset: "steam"})
	b, _ := s.Create(ctx, other, CreateRequest{Name: "B", Preset: "steam"})
	if _, err := s.Launch(ctx, u, &store.Pairing{ID: "1", UserID: u.ID}, a.MoonlightID, store.Stream{Width: 1, Height: 1, FPS: 1}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Launch(ctx, other, &store.Pairing{ID: "2", UserID: other.ID}, b.MoonlightID, store.Stream{Width: 1, Height: 1, FPS: 1}, false); !errors.Is(err, ErrNoSlot) {
		t.Fatalf("expected ErrNoSlot, got %v", err)
	}
}

func TestLaunchWhileStartingKeepsPod(t *testing.T) {
	s, u, orch := newService(t)
	ctx := context.Background()
	a, _ := s.Create(ctx, u, CreateRequest{Name: "Steam", Preset: "steam"})
	pairing := &store.Pairing{ID: "fp", UserID: u.ID, Name: "phone"}
	first := store.Stream{ClientIP: "10.0.0.5", AESKey: "k1", AESIV: "iv1", Width: 1920, Height: 1080, FPS: 60}
	orch.slow = true

	// The client gives up before the pod is up.
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	_, err := s.Launch(cctx, u, pairing, a.MoonlightID, first, false)
	cancel()
	if err == nil {
		t.Fatal("expected a timeout")
	}
	// ... and sends /cancel, which must not throw the starting pod away.
	if err := s.Cancel(ctx, u); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, u.ID, a.ID); got.State != store.StateStarting {
		t.Fatalf("cancel while starting must keep the app starting: %s", got.State)
	}
	// A retry with new keys updates the pending stream; the stream that
	// comes up must match the client that waits now.
	second := store.Stream{ClientIP: "10.0.0.5", AESKey: "k2", AESIV: "iv2", Width: 1280, Height: 720, FPS: 60}
	cctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
	_, _ = s.Launch(cctx, u, pairing, a.MoonlightID, second, false)
	cancel()
	if got, _ := s.Get(ctx, u.ID, a.ID); got.Stream == nil || got.Stream.AESKey != "k2" || got.Slot != 0 {
		t.Fatalf("retry must re-key without a new slot: %+v", got.Stream)
	}
	// The pod comes up; the next launch streams at once.
	orch.slow = false
	orch.Notify(a.ID)
	url, err := s.Launch(ctx, u, pairing, a.MoonlightID, second, false)
	if err != nil || url == "" {
		t.Fatalf("launch after warm-up: %v %q", err, url)
	}
	// Cancel on a running app still quits it.
	if err := s.Cancel(ctx, u); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, u.ID, a.ID); got.State != store.StateStopped {
		t.Fatalf("cancel while running: %s", got.State)
	}
}

func TestStartWarm(t *testing.T) {
	s, u, _ := newService(t)
	ctx := context.Background()
	a, _ := s.Create(ctx, u, CreateRequest{Name: "Steam", Preset: "steam"})
	started, err := s.Start(ctx, u, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if started.Stream != nil || started.Slot != 0 {
		t.Fatalf("warm start: %+v", started)
	}
	got, _ := s.Get(ctx, u.ID, a.ID)
	if got.State != store.StateRunning || s.View(got).Streaming {
		t.Fatalf("warm app must be running but not streaming: %s %v", got.State, s.View(got).Streaming)
	}
	// Starting again is a no-op; a launch then streams at once.
	if again, err := s.Start(ctx, u, a.ID); err != nil || again.State != store.StateRunning {
		t.Fatalf("start twice: %v %+v", err, again)
	}
	pairing := &store.Pairing{ID: "fp", UserID: u.ID, Name: "phone"}
	stream := store.Stream{ClientIP: "10.0.0.5", AESKey: "k", AESIV: "iv", Width: 1920, Height: 1080, FPS: 60}
	if url, err := s.Launch(ctx, u, pairing, a.MoonlightID, stream, false); err != nil || url == "" {
		t.Fatalf("launch on warm app: %v %q", err, url)
	}
	if got, _ := s.Get(ctx, u.ID, a.ID); !s.View(got).Streaming {
		t.Fatal("launch must make the warm app stream")
	}
}
