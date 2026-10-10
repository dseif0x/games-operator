package apps

import (
	"context"
	"errors"
	"testing"

	"github.com/dseif0x/games-operator/internal/config"
	"github.com/dseif0x/games-operator/internal/store"
)

// newUser adds a plain user (role user) to the service's store.
func newUser(t *testing.T, s *Service, name string) *store.User {
	t.Helper()
	u, err := s.Store.Users().UpsertPassword(context.Background(), name, "x")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUsersPlayTheCatalogOnly(t *testing.T) {
	s, admin, _ := newService(t)
	ctx := context.Background()
	bob := newUser(t, s, "bob")

	// A custom app is an admin's privilege.
	if _, err := s.Create(ctx, bob, CreateRequest{Name: "Mine", Preset: "steam"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("custom app by a user: %v", err)
	}
	// Bob cannot define catalog entries either.
	if _, err := s.CreateTemplate(ctx, bob, CreateRequest{Name: "Steam", Preset: "steam"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("template by a user: %v", err)
	}
	tpl, err := s.CreateTemplate(ctx, admin, CreateRequest{Name: "Steam", Preset: "steam", Description: "Big Picture", PVCSize: "100Gi", Env: map[string]string{"PROTON_LOG": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTemplate(ctx, admin, CreateRequest{Name: "Steam", Preset: "steam"}); err == nil {
		t.Fatal("duplicate catalog name must be refused")
	}
	hidden, _ := s.CreateTemplate(ctx, admin, CreateRequest{Name: "Hidden", Preset: "firefox", Enabled: ptr(false)})

	// Users see the enabled entries; admins all of them.
	if list, _ := s.Templates(ctx, bob); len(list) != 1 || list[0].ID != tpl.ID {
		t.Fatalf("user catalog: %+v", list)
	}
	if list, _ := s.Templates(ctx, admin); len(list) != 2 {
		t.Fatalf("admin catalog: %+v", list)
	}

	// An instance: the entry's settings, the user's name and volume.
	a, err := s.Create(ctx, bob, CreateRequest{TemplateID: tpl.ID, Name: "Bob's Steam"})
	if err != nil {
		t.Fatal(err)
	}
	if a.TemplateID != tpl.ID || a.Name != "Bob's Steam" || a.Preset != "steam" || a.PVCSize != "100Gi" || a.Env["PROTON_LOG"] != "1" || a.OwnerID != bob.ID {
		t.Fatalf("instance: %+v", a)
	}
	if b, _ := s.Create(ctx, bob, CreateRequest{TemplateID: tpl.ID}); b.Name != "Steam" {
		t.Fatalf("the entry's name is the default: %q", b.Name)
	}
	if _, err := s.Create(ctx, bob, CreateRequest{TemplateID: hidden.ID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a disabled entry is invisible to users: %v", err)
	}
	if _, err := s.Create(ctx, admin, CreateRequest{TemplateID: hidden.ID}); err != nil {
		t.Fatalf("an admin may instantiate a disabled entry: %v", err)
	}
	if list, _ := s.Templates(ctx, admin); list[1].Instances != 2 || list[0].Instances != 1 {
		t.Fatalf("instance counts: %+v", list)
	}
	// (the admin's own instance below is created and deleted before the
	// overview is counted; a deleting app still has its row)

	// Instances are edited through the entry, by nobody else.
	if _, err := s.Update(ctx, bob, a.ID, CreateRequest{Name: "x", Preset: "firefox"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("user edit: %v", err)
	}
	if _, err := s.Get(ctx, admin.ID, a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("an app is its owner's")
	}
	var ve *ValidationError
	own, _ := s.Create(ctx, admin, CreateRequest{TemplateID: tpl.ID, Name: "Admin's"})
	if _, err := s.Update(ctx, admin, own.ID, CreateRequest{Name: "x", Preset: "firefox"}); !errors.As(err, &ve) {
		t.Fatalf("admin edit of an instance: %v", err)
	}
	if _, err := s.Delete(ctx, admin, own.ID); err != nil {
		t.Fatal(err)
	}

	// The entry changes; the instance follows on its next start.
	if _, err := s.UpdateTemplate(ctx, admin, tpl.ID, CreateRequest{Name: "Steam", Preset: "steam", Env: map[string]string{"PROTON_LOG": "0"}, Resources: config.Resources{Limits: config.ResourceList{CPU: "4"}}}); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Store.Apps().Get(ctx, a.ID); cur.Env["PROTON_LOG"] != "1" {
		t.Fatal("a stopped instance keeps its settings until it starts")
	}
	started, err := s.Start(ctx, bob, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if started.Env["PROTON_LOG"] != "0" || started.Resources.Limits.CPU != "4" || started.Name != "Bob's Steam" || started.PVCSize != "100Gi" {
		t.Fatalf("start must copy the entry: %+v", started)
	}

	// Admin oversight.
	if _, err := s.ListAll(ctx, bob); !errors.Is(err, ErrForbidden) {
		t.Fatal("overview is admin only")
	}
	if all, _ := s.ListAll(ctx, admin); len(all) != 4 {
		// bob's two, the admin's hidden one, and "Admin's" still deleting
		// (the fake reconciler never finishes a deletion).
		t.Fatalf("overview: %d", len(all))
	}
	if _, err := s.StopAny(ctx, admin, a.ID); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Store.Apps().Get(ctx, a.ID); cur.State != store.StateStopped {
		t.Fatalf("after admin stop: %s", cur.State)
	}
	// Deleting the entry leaves the instance as it last ran.
	if err := s.DeleteTemplate(ctx, admin, tpl.ID); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Store.Apps().Get(ctx, a.ID); cur.TemplateID != "" || cur.Env["PROTON_LOG"] != "0" {
		t.Fatalf("detached instance: %+v", cur)
	}
	if _, err := s.Start(ctx, bob, a.ID); err != nil {
		t.Fatalf("a detached instance still starts: %v", err)
	}
	if _, err := s.DeleteAny(ctx, admin, a.ID); err != nil {
		t.Fatal(err)
	}
}

func TestQuotas(t *testing.T) {
	s, admin, _ := newService(t)
	ctx := context.Background()
	s.Defaults.MaxApps, s.Defaults.MaxStorage = 2, "150Gi"
	tpl, _ := s.CreateTemplate(ctx, admin, CreateRequest{Name: "Steam", Preset: "steam", PVCSize: "100Gi"})
	bob := newUser(t, s, "bob")

	if _, err := s.Create(ctx, bob, CreateRequest{TemplateID: tpl.ID, Name: "One"}); err != nil {
		t.Fatal(err)
	}
	var qe *QuotaError
	if _, err := s.Create(ctx, bob, CreateRequest{TemplateID: tpl.ID, Name: "Two"}); !errors.As(err, &qe) {
		t.Fatalf("storage quota: %v", err)
	}
	// A bigger allowance for bob alone.
	big := "1Ti"
	bob.Quota.MaxStorage = &big
	if _, err := s.Store.Users().Update(ctx, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, bob, CreateRequest{TemplateID: tpl.ID, Name: "Two"}); err != nil {
		t.Fatalf("with the override: %v", err)
	}
	if _, err := s.Create(ctx, bob, CreateRequest{TemplateID: tpl.ID, Name: "Three"}); !errors.As(err, &qe) {
		t.Fatalf("app count quota: %v", err)
	}
	usage, err := s.Usage(ctx, bob)
	if err != nil || usage.Apps != 2 || usage.MaxApps != 2 || usage.Storage != "200Gi" || usage.MaxStorage != "1Ti" {
		t.Fatalf("usage: %+v %v", usage, err)
	}
	// Admins are not limited.
	for _, n := range []string{"a", "b", "c"} {
		if _, err := s.Create(ctx, admin, CreateRequest{TemplateID: tpl.ID, Name: n}); err != nil {
			t.Fatalf("admin %s: %v", n, err)
		}
	}
	if usage, _ := s.Usage(ctx, admin); usage.MaxApps != 0 || usage.MaxStorage != "" {
		t.Fatalf("admin usage: %+v", usage)
	}
}

func TestDeleteUser(t *testing.T) {
	s, admin, orch := newService(t)
	ctx := context.Background()
	tpl, _ := s.CreateTemplate(ctx, admin, CreateRequest{Name: "Steam", Preset: "steam"})
	bob := newUser(t, s, "bob")
	a, _ := s.Create(ctx, bob, CreateRequest{TemplateID: tpl.ID})
	if _, err := s.Start(ctx, bob, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, bob, admin.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a user deleting: %v", err)
	}
	var ve *ValidationError
	if err := s.DeleteUser(ctx, admin, admin.ID); !errors.As(err, &ve) {
		t.Fatalf("self-delete: %v", err)
	}
	if err := s.DeleteUser(ctx, admin, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Users().GetByID(ctx, bob.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("bob must be gone")
	}
	if _, err := s.Store.Apps().Get(ctx, a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("bob's app row must be gone")
	}
	orch.mu.Lock()
	defer orch.mu.Unlock()
	if len(orch.notified) == 0 || orch.notified[len(orch.notified)-1] != a.ID {
		t.Fatalf("the reconciler must be told to tear bob's app down: %v", orch.notified)
	}
}

func TestBrokerAdminFeed(t *testing.T) {
	b := NewBroker()
	mine, stopMine := b.Subscribe("u1")
	defer stopMine()
	all, stopAll := b.Subscribe(AllOwners)
	defer stopAll()
	b.Publish("u2", Event{Type: "app", ID: "a"})
	select {
	case ev := <-all:
		if ev.ID != "a" {
			t.Fatalf("admin feed: %+v", ev)
		}
	default:
		t.Fatal("the admin feed must see every owner")
	}
	select {
	case ev := <-mine:
		t.Fatalf("u1 must not see u2's app: %+v", ev)
	default:
	}
}

func ptr[T any](v T) *T { return &v }
