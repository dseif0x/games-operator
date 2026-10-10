package apps

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/dseif0x/games-operator/internal/store"
)

// ---- catalog ----

// TemplateView is the API representation of a catalog entry.
type TemplateView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Preset       string            `json:"preset"`
	Image        string            `json:"image"`
	IconURL      string            `json:"icon_url"`
	HDR          bool              `json:"hdr"`
	Command      string            `json:"command"`
	PVCSize      string            `json:"pvc_size"`
	StorageClass string            `json:"storage_class"`
	Resources    any               `json:"resources"`
	Env          map[string]string `json:"env"`
	HostIPC      bool              `json:"host_ipc"`
	Capabilities []string          `json:"capabilities"`
	Enabled      bool              `json:"enabled"`
	// Instances is how many apps follow the entry (filled in by Templates).
	Instances int `json:"instances"`
}

// TemplateView renders a catalog entry.
func (s *Service) TemplateView(t *store.Template) TemplateView {
	v := TemplateView{
		ID: t.ID, Name: t.Name, Description: t.Description, Preset: t.Preset, Image: t.Image, IconURL: t.IconURL, HDR: t.HDR,
		Command: t.Command, PVCSize: t.PVCSize, StorageClass: t.StorageClass, Resources: t.Resources, Env: t.Env, HostIPC: t.HostIPC,
		Capabilities: t.Capabilities, Enabled: t.Enabled,
	}
	if v.Env == nil {
		v.Env = map[string]string{}
	}
	if v.Capabilities == nil {
		v.Capabilities = []string{}
	}
	return v
}

// Templates lists the catalog: every entry for an admin, the enabled ones
// for a user, each with its instance count.
func (s *Service) Templates(ctx context.Context, user *store.User) ([]TemplateView, error) {
	list, err := s.Store.Catalog().List(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	if all, err := s.Store.Apps().ListAll(ctx); err == nil {
		for _, a := range all {
			if a.TemplateID != "" {
				counts[a.TemplateID]++
			}
		}
	}
	out := make([]TemplateView, 0, len(list))
	for _, t := range list {
		if !t.Enabled && !user.IsAdmin() {
			continue
		}
		v := s.TemplateView(t)
		v.Instances = counts[t.ID]
		out = append(out, v)
	}
	return out, nil
}

// templateRequest is the entry as a create request: what an instance is
// made of.
func templateRequest(t *store.Template) CreateRequest {
	hostIPC := t.HostIPC
	return CreateRequest{
		Name: t.Name, Preset: t.Preset, Image: t.Image, IconURL: t.IconURL, HDR: t.HDR, Command: t.Command,
		PVCSize: t.PVCSize, StorageClass: t.StorageClass, Resources: t.Resources, Env: t.Env, HostIPC: &hostIPC, Capabilities: t.Capabilities,
	}
}

func (s *Service) applyTemplate(t *store.Template, req CreateRequest) {
	t.Name, t.Description = req.Name, strings.TrimSpace(req.Description)
	t.Preset, t.Image, t.IconURL, t.HDR, t.Command = req.Preset, req.Image, req.IconURL, req.HDR, strings.TrimSpace(req.Command)
	t.PVCSize, t.StorageClass = req.PVCSize, req.StorageClass
	t.Resources, t.Env, t.Capabilities = req.Resources, req.Env, req.Capabilities
	if t.Env == nil {
		t.Env = map[string]string{}
	}
	if t.Capabilities == nil {
		t.Capabilities = []string{}
	}
	a := &store.App{}
	s.apply(a, req) // the preset's hostIPC default, the same way as for an app
	t.HostIPC = a.HostIPC
	if req.Enabled != nil {
		t.Enabled = *req.Enabled
	}
}

// CreateTemplate adds a catalog entry (admin).
func (s *Service) CreateTemplate(ctx context.Context, user *store.User, req CreateRequest) (*store.Template, error) {
	if !user.IsAdmin() {
		return nil, ErrForbidden
	}
	if err := s.validate(&req); err != nil {
		return nil, err
	}
	t := &store.Template{Enabled: true}
	s.applyTemplate(t, req)
	if err := s.Store.Catalog().Create(ctx, t); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, &ValidationError{"a catalog entry with that name exists"}
		}
		return nil, err
	}
	s.Log.Info("catalog entry created", "template", t.ID, "name", t.Name, "by", user.Username)
	return t, nil
}

// UpdateTemplate replaces a catalog entry (admin). Running instances keep
// what they run; they pick the change up on their next start.
func (s *Service) UpdateTemplate(ctx context.Context, user *store.User, id string, req CreateRequest) (*store.Template, error) {
	if !user.IsAdmin() {
		return nil, ErrForbidden
	}
	t, err := s.Store.Catalog().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.validate(&req); err != nil {
		return nil, err
	}
	s.applyTemplate(t, req)
	updated, err := s.Store.Catalog().Update(ctx, t)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, &ValidationError{"a catalog entry with that name exists"}
		}
		return nil, err
	}
	s.Log.Info("catalog entry updated", "template", t.ID, "name", t.Name, "by", user.Username)
	return updated, nil
}

// DeleteTemplate removes a catalog entry (admin). Its instances stay with
// their users as they last ran, detached from the catalog.
func (s *Service) DeleteTemplate(ctx context.Context, user *store.User, id string) error {
	if !user.IsAdmin() {
		return ErrForbidden
	}
	if err := s.Store.Catalog().Delete(ctx, id); err != nil {
		return err
	}
	s.Log.Info("catalog entry deleted", "template", id, "by", user.Username)
	return nil
}

// syncTemplate copies the catalog entry's settings into the instance, so
// a start always runs what the admin currently defines. The name and the
// volume stay the user's.
func (s *Service) syncTemplate(ctx context.Context, a *store.App) (*store.App, error) {
	if a.TemplateID == "" {
		return a, nil
	}
	t, err := s.Store.Catalog().Get(ctx, a.TemplateID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return a, nil // detached meanwhile: run as last defined
		}
		return nil, err
	}
	req := templateRequest(t)
	req.Name = a.Name
	s.apply(a, req)
	updated, err := s.Store.Apps().Update(ctx, a)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// ---- quotas ----

// ValidQuantity checks a Kubernetes quantity such as 500Gi.
func ValidQuantity(v string) error {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return err
	}
	if q.Sign() < 0 {
		return errors.New("must not be negative")
	}
	return nil
}

// QuotaView is a user's limits next to what they use.
type QuotaView struct {
	MaxApps    int    `json:"max_apps"`    // 0 = unlimited
	MaxStorage string `json:"max_storage"` // "" = unlimited
	Apps       int    `json:"apps"`
	Storage    string `json:"storage"`
}

// effectiveQuota resolves the user's limits: their own values, else the
// defaults. Admins are not limited.
func (s *Service) effectiveQuota(u *store.User) (maxApps int, maxStorage string) {
	if u.IsAdmin() {
		return 0, ""
	}
	maxApps, maxStorage = s.Defaults.MaxApps, s.Defaults.MaxStorage
	if u.Quota.MaxApps != nil {
		maxApps = *u.Quota.MaxApps
	}
	if u.Quota.MaxStorage != nil {
		maxStorage = *u.Quota.MaxStorage
	}
	return maxApps, maxStorage
}

// Usage returns the user's quota and what counts against it.
func (s *Service) Usage(ctx context.Context, u *store.User) (QuotaView, error) {
	list, err := s.Store.Apps().List(ctx, u.ID)
	if err != nil {
		return QuotaView{}, err
	}
	maxApps, maxStorage := s.effectiveQuota(u)
	total := storageOf(list)
	return QuotaView{MaxApps: maxApps, MaxStorage: maxStorage, Apps: len(list), Storage: total.String()}, nil
}

func storageOf(list []*store.App) resource.Quantity {
	var total resource.Quantity
	for _, a := range list {
		if q, err := resource.ParseQuantity(a.PVCSize); err == nil {
			total.Add(q)
		}
	}
	return total
}

// checkQuota refuses a new app with pvcSize when it would exceed the
// user's limits.
func (s *Service) checkQuota(ctx context.Context, u *store.User, pvcSize string) error {
	maxApps, maxStorage := s.effectiveQuota(u)
	if maxApps == 0 && maxStorage == "" {
		return nil
	}
	list, err := s.Store.Apps().List(ctx, u.ID)
	if err != nil {
		return err
	}
	if maxApps > 0 && len(list) >= maxApps {
		return &QuotaError{fmt.Sprintf("you have %d of %d apps; delete one first", len(list), maxApps)}
	}
	if maxStorage != "" {
		limit, err := resource.ParseQuantity(maxStorage)
		if err != nil {
			return fmt.Errorf("invalid storage quota %q: %w", maxStorage, err)
		}
		total := storageOf(list)
		if q, err := resource.ParseQuantity(pvcSize); err == nil {
			total.Add(q)
		}
		if total.Cmp(limit) > 0 {
			return &QuotaError{fmt.Sprintf("this volume would bring your storage to %s of %s", total.String(), limit.String())}
		}
	}
	return nil
}

// ---- admin ----

// ListAll returns every app (admin overview).
func (s *Service) ListAll(ctx context.Context, user *store.User) ([]*store.App, error) {
	if !user.IsAdmin() {
		return nil, ErrForbidden
	}
	return s.Store.Apps().ListAll(ctx)
}

// StopAny stops any user's app (admin).
func (s *Service) StopAny(ctx context.Context, user *store.User, id string) (*store.App, error) {
	if !user.IsAdmin() {
		return nil, ErrForbidden
	}
	a, err := s.Store.Apps().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.stop(ctx, a, "admin")
}

// DeleteAny deletes any user's app and its volume (admin).
func (s *Service) DeleteAny(ctx context.Context, user *store.User, id string) (*store.App, error) {
	if !user.IsAdmin() {
		return nil, ErrForbidden
	}
	a, err := s.Store.Apps().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.remove(ctx, a, "admin")
}

// DeleteUser removes an account: its apps are torn down (pods, volumes),
// its pairings go, and the row is deleted. Whatever the reconciler has not
// cleaned up by the time the rows are gone is swept as an orphan.
func (s *Service) DeleteUser(ctx context.Context, admin *store.User, id string) error {
	if !admin.IsAdmin() {
		return ErrForbidden
	}
	if admin.ID == id {
		return &ValidationError{"you cannot delete your own account"}
	}
	u, err := s.Store.Users().GetByID(ctx, id)
	if err != nil {
		return err
	}
	list, err := s.Store.Apps().List(ctx, id)
	if err != nil {
		return err
	}
	for _, a := range list {
		if a.State == store.StateDeleting {
			continue
		}
		if _, err := s.remove(ctx, a, "admin"); err != nil {
			return err
		}
	}
	if err := s.Store.Users().Delete(ctx, id); err != nil {
		return err
	}
	s.Log.Info("user deleted", "user", u.Username, "apps", len(list), "by", admin.Username)
	return nil
}
