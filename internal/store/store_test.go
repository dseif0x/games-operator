package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestMemory(t *testing.T) { exercise(t, NewMemory()) }

func TestPostgres(t *testing.T) {
	url := os.Getenv("GAMES_OPERATOR_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("GAMES_OPERATOR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	p, err := Open(ctx, url, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if _, err := p.pool.Exec(ctx, `TRUNCATE users, apps, pairings, app_events CASCADE`); err != nil {
		t.Fatal(err)
	}
	exercise(t, p)
}

func exercise(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	u, err := st.Users().UpsertPassword(ctx, "alice", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Users().UpsertPassword(ctx, "alice", "hash2"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Users().GetByUsername(ctx, "alice")
	if got.PasswordHash != "hash2" || got.ID != u.ID {
		t.Fatalf("upsert: %+v", got)
	}
	if err := st.Users().SetAPITokenHash(ctx, u.ID, "tok"); err != nil {
		t.Fatal(err)
	}
	if byTok, err := st.Users().GetByAPITokenHash(ctx, "tok"); err != nil || byTok.ID != u.ID {
		t.Fatalf("by token: %v %+v", err, byTok)
	}
	if _, err := st.Users().GetByAPITokenHash(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty token must not match: %v", err)
	}

	a := &App{OwnerID: u.ID, MoonlightID: 42, Name: "Steam", Preset: "steam", Image: "ghcr.io/games-on-whales/steam:edge", PVCSize: "50Gi"}
	if err := st.Apps().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if a.State != StateStopped || a.Slot != -1 || a.Generation != 1 {
		t.Fatalf("create defaults: %+v", a)
	}
	if err := st.Apps().Create(ctx, &App{OwnerID: u.ID, MoonlightID: 42, Name: "dup", Preset: "x", Image: "y", PVCSize: "1Gi"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate moonlight id: %v", err)
	}
	if byML, err := st.Apps().GetByMoonlightID(ctx, u.ID, 42); err != nil || byML.ID != a.ID {
		t.Fatalf("by moonlight id: %v", err)
	}
	stream := &Stream{PairingID: "fp", ClientIP: "10.0.0.5", AESKey: "k", AESIV: "iv", Width: 1920, Height: 1080, FPS: 60}
	launched, err := st.Apps().Launch(ctx, a.ID, stream, 3)
	if err != nil {
		t.Fatal(err)
	}
	if launched.State != StateStarting || launched.Generation != 2 || launched.Slot != 3 || launched.Stream == nil || launched.Stream.Width != 1920 {
		t.Fatalf("launch: %+v", launched)
	}
	slots, _ := st.Apps().UsedSlots(ctx)
	if len(slots) != 1 || slots[0] != 3 {
		t.Fatalf("used slots %v", slots)
	}
	if _, err := st.Apps().SetRuntime(ctx, a.ID, "wolf-1", "rtsp://1.2.3.4:48130"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apps().SetState(ctx, a.ID, StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	cur, _ := st.Apps().Get(ctx, a.ID)
	if cur.WolfSessionID != "wolf-1" || cur.StreamURL == "" || cur.State != StateRunning {
		t.Fatalf("runtime: %+v", cur)
	}
	cur.Name = "Steam Big Picture"
	cur.Env = map[string]string{"A": "1"}
	if upd, err := st.Apps().Update(ctx, cur); err != nil || upd.Name != "Steam Big Picture" || upd.Env["A"] != "1" {
		t.Fatalf("update: %v %+v", err, upd)
	}
	if cleared, err := st.Apps().ClearRuntime(ctx, a.ID); err != nil || cleared.Slot != -1 || cleared.StreamURL != "" {
		t.Fatalf("clear: %v %+v", err, cleared)
	}
	if err := st.Events().Add(ctx, a.ID, "state", "stopped → starting"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_ = st.Events().Add(ctx, a.ID, "x", "y")
	}
	_ = st.Events().Prune(ctx, a.ID, 3)
	evs, _ := st.Events().List(ctx, a.ID, 10)
	if len(evs) != 3 {
		t.Fatalf("prune: %d events", len(evs))
	}
	list, _ := st.Apps().List(ctx, u.ID)
	if len(list) != 1 {
		t.Fatalf("list: %d", len(list))
	}
	counts, _ := st.Apps().CountByState(ctx)
	if counts[StateRunning] != 1 {
		t.Fatalf("counts %v", counts)
	}

	p := &Pairing{ID: "fp", UserID: u.ID, CertPEM: "pem", Name: "phone"}
	if err := st.Pairings().Upsert(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := st.Pairings().TouchSeen(ctx, "fp", time.Now()); err != nil {
		t.Fatal(err)
	}
	ps, _ := st.Pairings().List(ctx, u.ID)
	if len(ps) != 1 || ps[0].LastSeenAt == nil {
		t.Fatalf("pairings: %+v", ps)
	}
	if err := st.Pairings().Delete(ctx, "00000000-0000-4000-8000-000000000002", "fp"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete by other user: %v", err)
	}
	if err := st.Pairings().Delete(ctx, u.ID, "fp"); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apps().Get(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
