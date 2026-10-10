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
	if err := st.Users().SetBrowserTokenHash(ctx, u.ID, "btok"); err != nil {
		t.Fatal(err)
	}
	if byTok, err := st.Users().GetByBrowserTokenHash(ctx, "btok"); err != nil || byTok.ID != u.ID {
		t.Fatalf("by browser token: %v %+v", err, byTok)
	}
	if _, err := st.Users().GetByAPITokenHash(ctx, "btok"); !errors.Is(err, ErrNotFound) {
		t.Fatal("browser token must not pass as api token")
	}
	if _, err := st.Users().GetByBrowserTokenHash(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatal("empty browser token must not match")
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
	// Browser pairings follow whoever presses Play.
	// Roles, quotas, the user list and deletion.
	if u2, _ := st.Users().GetByID(ctx, u.ID); u2.Role != RoleUser {
		t.Fatalf("default role %q", u2.Role)
	}
	if err := st.Users().SetRole(ctx, u.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	three := 3
	if upd, err := st.Users().Update(ctx, &User{ID: u.ID, Role: RoleAdmin, Disabled: false, Quota: Quota{MaxApps: &three}}); err != nil || upd.Quota.MaxApps == nil || *upd.Quota.MaxApps != 3 || !upd.IsAdmin() {
		t.Fatalf("update user: %+v %v", upd, err)
	}
	if err := st.Users().SetPasswordHash(ctx, u.ID, "hash3"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Users().GetByUsername(ctx, "alice"); got.PasswordHash != "hash3" || got.Quota.MaxApps == nil {
		t.Fatalf("password/quota not kept: %+v", got)
	}
	if list, err := st.Users().List(ctx); err != nil || len(list) != 1 || list[0].Username != "alice" {
		t.Fatalf("users: %v %v", list, err)
	}
	if err := st.Users().SetPasswordHash(ctx, NewID(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
	// The catalog and instances of it.
	tpl := &Template{Name: "Steam", Preset: "steam", Image: "ghcr.io/games-on-whales/steam:edge", Enabled: true, PVCSize: "100Gi"}
	if err := st.Catalog().Create(ctx, tpl); err != nil {
		t.Fatal(err)
	}
	if err := st.Catalog().Create(ctx, &Template{Name: "Steam", Preset: "steam", Image: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate template name: %v", err)
	}
	tpl.Description = "Big Picture"
	if upd, err := st.Catalog().Update(ctx, tpl); err != nil || upd.Description != "Big Picture" || upd.Env == nil {
		t.Fatalf("update template: %+v %v", upd, err)
	}
	if list, _ := st.Catalog().List(ctx); len(list) != 1 || list[0].ID != tpl.ID {
		t.Fatalf("catalog: %v", list)
	}
	inst := &App{OwnerID: u.ID, MoonlightID: 77, Name: "Steam", Preset: "steam", Image: tpl.Image, PVCSize: "100Gi", TemplateID: tpl.ID}
	if err := st.Apps().Create(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Apps().Get(ctx, inst.ID); got.TemplateID != tpl.ID {
		t.Fatalf("template id not kept: %+v", got)
	}
	if err := st.Catalog().Delete(ctx, tpl.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Apps().Get(ctx, inst.ID); got.TemplateID != "" {
		t.Fatalf("deleting the template must detach the instance: %+v", got)
	}
	if err := st.Catalog().Delete(ctx, tpl.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing template: %v", err)
	}
	if err := st.Apps().Delete(ctx, inst.ID); err != nil {
		t.Fatal(err)
	}

	// Deleting a user takes its pairings and apps along (the cascade).
	bob, _ := st.Users().UpsertPassword(ctx, "bob", "h")
	if err := st.Pairings().Upsert(ctx, &Pairing{ID: "mw", UserID: bob.ID, CertPEM: "c", Name: "moonlight-web", Via: PairingViaBrowser}); err != nil {
		t.Fatal(err)
	}
	bobApp := &App{OwnerID: bob.ID, MoonlightID: 5, Name: "Bob's", Preset: "steam", Image: "i", PVCSize: "1Gi"}
	if err := st.Apps().Create(ctx, bobApp); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().Delete(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pairings().Get(ctx, "mw"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob's pairing must go with bob: %v", err)
	}
	if _, err := st.Apps().Get(ctx, bobApp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob's app must go with bob: %v", err)
	}
	if err := st.Users().Delete(ctx, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice: %v", err)
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
