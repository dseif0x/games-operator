package store

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Memory is an in-memory Store for tests.
type Memory struct {
	mu       sync.Mutex
	users    map[string]*User
	catalog  map[string]*Template
	apps     map[string]*App
	pairings map[string]*Pairing
	events   map[string][]*Event
	eventSeq int64
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{users: map[string]*User{}, catalog: map[string]*Template{}, apps: map[string]*App{}, pairings: map[string]*Pairing{}, events: map[string][]*Event{}}
}

func (m *Memory) Users() Users       { return memUsers{m} }
func (m *Memory) Catalog() Catalog   { return memCatalog{m} }
func (m *Memory) Apps() Apps         { return memApps{m} }
func (m *Memory) Pairings() Pairings { return memPairings{m} }
func (m *Memory) Events() Events     { return memEvents{m} }

// Ping always succeeds.
func (m *Memory) Ping(context.Context) error { return nil }

// Close is a no-op.
func (m *Memory) Close() {}

func copyApp(a *App) *App {
	c := *a
	c.Env = map[string]string{}
	for k, v := range a.Env {
		c.Env[k] = v
	}
	c.Capabilities = append([]string{}, a.Capabilities...)
	if a.Stream != nil {
		s := *a.Stream
		c.Stream = &s
	}
	return &c
}

type memUsers struct{ m *Memory }

func (r memUsers) Create(_ context.Context, u *User) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	for _, x := range r.m.users {
		if x.Username == u.Username {
			return ErrConflict
		}
	}
	if u.ID == "" {
		u.ID = NewID()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	if u.Role == "" {
		u.Role = RoleUser
	}
	c := *u
	r.m.users[u.ID] = &c
	return nil
}

func (r memUsers) List(_ context.Context) ([]*User, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	out := make([]*User, 0, len(r.m.users))
	for _, u := range r.m.users {
		c := *u
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func (r memUsers) Update(_ context.Context, u *User) (*User, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	cur, ok := r.m.users[u.ID]
	if !ok {
		return nil, ErrNotFound
	}
	cur.Role, cur.Disabled, cur.Quota = u.Role, u.Disabled, u.Quota
	c := *cur
	return &c, nil
}

func (r memUsers) SetPasswordHash(_ context.Context, id, hash string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	u, ok := r.m.users[id]
	if !ok {
		return ErrNotFound
	}
	u.PasswordHash = hash
	return nil
}

func (r memUsers) SetRole(_ context.Context, id, role string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	u, ok := r.m.users[id]
	if !ok {
		return ErrNotFound
	}
	u.Role = role
	return nil
}

// Delete removes the user and, like the database's cascade, its apps,
// their events and its pairings.
func (r memUsers) Delete(_ context.Context, id string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if _, ok := r.m.users[id]; !ok {
		return ErrNotFound
	}
	delete(r.m.users, id)
	for aid, a := range r.m.apps {
		if a.OwnerID == id {
			delete(r.m.apps, aid)
			delete(r.m.events, aid)
		}
	}
	for pid, p := range r.m.pairings {
		if p.UserID == id {
			delete(r.m.pairings, pid)
		}
	}
	return nil
}

// ---- catalog ----

type memCatalog struct{ m *Memory }

func copyTemplate(t *Template) *Template {
	c := *t
	c.Env = map[string]string{}
	for k, v := range t.Env {
		c.Env[k] = v
	}
	c.Capabilities = append([]string{}, t.Capabilities...)
	return &c
}

func (r memCatalog) Create(_ context.Context, t *Template) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	for _, x := range r.m.catalog {
		if x.Name == t.Name {
			return ErrConflict
		}
	}
	if t.ID == "" {
		t.ID = NewID()
	}
	now := time.Now()
	t.CreatedAt, t.UpdatedAt = now, now
	normaliseTemplate(t)
	r.m.catalog[t.ID] = copyTemplate(t)
	return nil
}

func (r memCatalog) Get(_ context.Context, id string) (*Template, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if t, ok := r.m.catalog[id]; ok {
		return copyTemplate(t), nil
	}
	return nil, ErrNotFound
}

func (r memCatalog) List(_ context.Context) ([]*Template, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	out := make([]*Template, 0, len(r.m.catalog))
	for _, t := range r.m.catalog {
		out = append(out, copyTemplate(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r memCatalog) Update(_ context.Context, t *Template) (*Template, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	cur, ok := r.m.catalog[t.ID]
	if !ok {
		return nil, ErrNotFound
	}
	for id, x := range r.m.catalog {
		if id != t.ID && x.Name == t.Name {
			return nil, ErrConflict
		}
	}
	normaliseTemplate(t)
	c := copyTemplate(t)
	c.CreatedAt, c.UpdatedAt = cur.CreatedAt, time.Now()
	r.m.catalog[t.ID] = c
	return copyTemplate(c), nil
}

func (r memCatalog) Delete(_ context.Context, id string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if _, ok := r.m.catalog[id]; !ok {
		return ErrNotFound
	}
	delete(r.m.catalog, id)
	for _, a := range r.m.apps {
		if a.TemplateID == id {
			a.TemplateID = "" // ON DELETE SET NULL
		}
	}
	return nil
}

func (r memUsers) GetByID(_ context.Context, id string) (*User, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if u, ok := r.m.users[id]; ok {
		c := *u
		return &c, nil
	}
	return nil, ErrNotFound
}

func (r memUsers) GetByUsername(_ context.Context, username string) (*User, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	for _, u := range r.m.users {
		if u.Username == username {
			c := *u
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (r memUsers) GetByAPITokenHash(_ context.Context, hash string) (*User, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if hash == "" {
		return nil, ErrNotFound
	}
	for _, u := range r.m.users {
		if u.APITokenHash == hash {
			c := *u
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (r memUsers) UpsertPassword(ctx context.Context, username, hash string) (*User, error) {
	r.m.mu.Lock()
	for _, u := range r.m.users {
		if u.Username == username {
			u.PasswordHash = hash
			c := *u
			r.m.mu.Unlock()
			return &c, nil
		}
	}
	r.m.mu.Unlock()
	u := &User{Username: username, PasswordHash: hash}
	if err := r.Create(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func (r memUsers) GetByBrowserTokenHash(_ context.Context, hash string) (*User, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if hash == "" {
		return nil, ErrNotFound
	}
	for _, u := range r.m.users {
		if u.BrowserTokenHash == hash {
			c := *u
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (r memUsers) SetBrowserTokenHash(_ context.Context, id, hash string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	u, ok := r.m.users[id]
	if !ok {
		return ErrNotFound
	}
	u.BrowserTokenHash = hash
	return nil
}

func (r memUsers) SetAPITokenHash(_ context.Context, id, hash string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	u, ok := r.m.users[id]
	if !ok {
		return ErrNotFound
	}
	u.APITokenHash = hash
	return nil
}

type memApps struct{ m *Memory }

func (r memApps) Create(_ context.Context, a *App) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	for _, x := range r.m.apps {
		if x.OwnerID == a.OwnerID && x.MoonlightID == a.MoonlightID {
			return ErrConflict
		}
	}
	if a.ID == "" {
		a.ID = NewID()
	}
	now := time.Now()
	a.CreatedAt, a.UpdatedAt = now, now
	if a.Generation == 0 {
		a.Generation = 1
	}
	if a.State == "" {
		a.State = StateStopped
	}
	if a.Slot == 0 {
		a.Slot = -1
	}
	normaliseApp(a)
	r.m.apps[a.ID] = copyApp(a)
	return nil
}

func (r memApps) get(id string) (*App, error) {
	if a, ok := r.m.apps[id]; ok {
		return a, nil
	}
	return nil, ErrNotFound
}

func (r memApps) Get(_ context.Context, id string) (*App, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	a, err := r.get(id)
	if err != nil {
		return nil, err
	}
	return copyApp(a), nil
}

func (r memApps) GetByMoonlightID(_ context.Context, ownerID string, moonlightID int32) (*App, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	for _, a := range r.m.apps {
		if a.OwnerID == ownerID && a.MoonlightID == moonlightID {
			return copyApp(a), nil
		}
	}
	return nil, ErrNotFound
}

func (r memApps) list(filter func(*App) bool) []*App {
	var out []*App
	for _, a := range r.m.apps {
		if filter(a) {
			out = append(out, copyApp(a))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (r memApps) List(_ context.Context, ownerID string) ([]*App, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	return r.list(func(a *App) bool { return a.OwnerID == ownerID }), nil
}

func (r memApps) ListAll(context.Context) ([]*App, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	return r.list(func(*App) bool { return true }), nil
}

func (r memApps) Update(_ context.Context, a *App) (*App, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	cur, err := r.get(a.ID)
	if err != nil {
		return nil, err
	}
	normaliseApp(a)
	cur.Name, cur.Preset, cur.Image, cur.IconURL, cur.HDR, cur.Command = a.Name, a.Preset, a.Image, a.IconURL, a.HDR, a.Command
	cur.Resources, cur.Env, cur.HostIPC, cur.Capabilities = a.Resources, a.Env, a.HostIPC, a.Capabilities
	cur.TemplateID = a.TemplateID
	cur.UpdatedAt = time.Now()
	return copyApp(cur), nil
}

func (r memApps) SetState(_ context.Context, id, state, reason string) (*App, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	a, err := r.get(id)
	if err != nil {
		return nil, err
	}
	a.State, a.StateReason, a.UpdatedAt = state, reason, time.Now()
	return copyApp(a), nil
}

func (r memApps) Launch(_ context.Context, id string, stream *Stream, slot int) (*App, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	a, err := r.get(id)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	a.State, a.StateReason, a.Generation = StateStarting, "", a.Generation+1
	a.Stream, a.Slot, a.WolfSessionID, a.StreamURL = stream, slot, "", ""
	a.UpdatedAt, a.LastActiveAt = now, &now
	return copyApp(a), nil
}

func (r memApps) SetStream(_ context.Context, id string, stream *Stream) (*App, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	a, err := r.get(id)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	a.Stream, a.WolfSessionID, a.UpdatedAt, a.LastActiveAt = stream, "", now, &now
	return copyApp(a), nil
}

func (r memApps) SetRuntime(_ context.Context, id, wolfSessionID, streamURL string) (*App, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	a, err := r.get(id)
	if err != nil {
		return nil, err
	}
	a.WolfSessionID, a.StreamURL, a.UpdatedAt = wolfSessionID, streamURL, time.Now()
	return copyApp(a), nil
}

func (r memApps) ClearRuntime(_ context.Context, id string) (*App, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	a, err := r.get(id)
	if err != nil {
		return nil, err
	}
	a.Slot, a.WolfSessionID, a.StreamURL, a.UpdatedAt = -1, "", "", time.Now()
	return copyApp(a), nil
}

func (r memApps) TouchActive(_ context.Context, id string, at time.Time) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	a, err := r.get(id)
	if err != nil {
		return err
	}
	if a.LastActiveAt == nil || a.LastActiveAt.Before(at) {
		a.LastActiveAt = &at
	}
	return nil
}

func (r memApps) Delete(_ context.Context, id string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if _, ok := r.m.apps[id]; !ok {
		return ErrNotFound
	}
	delete(r.m.apps, id)
	delete(r.m.events, id)
	return nil
}

func (r memApps) UsedSlots(context.Context) ([]int, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	var out []int
	for _, a := range r.m.apps {
		if a.Slot >= 0 && a.State != StateStopped {
			out = append(out, a.Slot)
		}
	}
	return out, nil
}

func (r memApps) CountByState(context.Context) (map[string]int, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	out := map[string]int{}
	for _, a := range r.m.apps {
		out[a.State]++
	}
	return out, nil
}

type memPairings struct{ m *Memory }

func (r memPairings) Upsert(_ context.Context, p *Pairing) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	c := *p
	r.m.pairings[p.ID] = &c
	return nil
}

func (r memPairings) Get(_ context.Context, id string) (*Pairing, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if p, ok := r.m.pairings[id]; ok {
		c := *p
		return &c, nil
	}
	return nil, ErrNotFound
}

func (r memPairings) List(_ context.Context, userID string) ([]*Pairing, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	var out []*Pairing
	for _, p := range r.m.pairings {
		if p.UserID == userID {
			c := *p
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (r memPairings) Delete(_ context.Context, userID, id string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	p, ok := r.m.pairings[id]
	if !ok || p.UserID != userID {
		return ErrNotFound
	}
	delete(r.m.pairings, id)
	return nil
}

func (r memPairings) TouchSeen(_ context.Context, id string, at time.Time) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if p, ok := r.m.pairings[id]; ok {
		if p.LastSeenAt == nil || p.LastSeenAt.Before(at) {
			p.LastSeenAt = &at
		}
	}
	return nil
}

type memEvents struct{ m *Memory }

func (r memEvents) Add(_ context.Context, appID, kind, message string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	r.m.eventSeq++
	r.m.events[appID] = append(r.m.events[appID], &Event{ID: r.m.eventSeq, AppID: appID, At: time.Now(), Kind: kind, Message: message})
	return nil
}

func (r memEvents) List(_ context.Context, appID string, limit int) ([]*Event, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if limit <= 0 {
		limit = EventsKeep
	}
	evs := r.m.events[appID]
	var out []*Event
	for i := len(evs) - 1; i >= 0 && len(out) < limit; i-- {
		c := *evs[i]
		out = append(out, &c)
	}
	return out, nil
}

func (r memEvents) Prune(_ context.Context, appID string, keep int) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	evs := r.m.events[appID]
	if len(evs) > keep {
		r.m.events[appID] = evs[len(evs)-keep:]
	}
	return nil
}

var _ Store = (*Memory)(nil)
var _ Store = (*Postgres)(nil)
