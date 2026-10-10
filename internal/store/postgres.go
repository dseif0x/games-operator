package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Postgres implements Store on pgx.
type Postgres struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// Open connects to Postgres, waits for it to answer, and applies the
// embedded migrations under a session-level advisory lock.
func Open(ctx context.Context, url string, log *slog.Logger) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Retry the first ping: the chart's Postgres may still be starting.
	var lastErr error
	for i := 0; i < 30; i++ {
		if lastErr = pool.Ping(ctx); lastErr == nil {
			break
		}
		log.Warn("postgres not ready", "err", lastErr, "attempt", i+1)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if lastErr != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres unreachable: %w", lastErr)
	}
	p := &Postgres{pool: pool, log: log}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) migrate(ctx context.Context) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(p.pool)
	defer db.Close()
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	prov, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("goose: %w", err)
	}
	results, err := prov.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for _, r := range results {
		p.log.Info("applied migration", "version", r.Source.Version, "path", r.Source.Path)
	}
	return nil
}

// Ping checks the connection.
func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// Close releases the pool.
func (p *Postgres) Close() { p.pool.Close() }

// Users returns the user aggregate.
func (p *Postgres) Users() Users { return pgUsers{p.pool} }

// Catalog returns the template aggregate.
func (p *Postgres) Catalog() Catalog { return pgCatalog{p.pool} }

// Apps returns the app aggregate.
func (p *Postgres) Apps() Apps { return pgApps{p.pool} }

// Pairings returns the pairing aggregate.
func (p *Postgres) Pairings() Pairings { return pgPairings{p.pool} }

// Events returns the event aggregate.
func (p *Postgres) Events() Events { return pgEvents{p.pool} }

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}

// ---- users ----

type pgUsers struct{ pool *pgxpool.Pool }

const userCols = "id, username, password_hash, created_at, disabled, api_token_hash, browser_token_hash, role, quota"

func scanUser(row pgx.Row) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt, &u.Disabled, &u.APITokenHash, &u.BrowserTokenHash, &u.Role, &u.Quota); err != nil {
		return nil, mapErr(err)
	}
	return &u, nil
}

func scanUsers(rows pgx.Rows) ([]*User, error) {
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, mapErr(rows.Err())
}

func (r pgUsers) Create(ctx context.Context, u *User) error {
	if u.ID == "" {
		u.ID = NewID()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	if u.Role == "" {
		u.Role = RoleUser
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO users (`+userCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		u.ID, u.Username, u.PasswordHash, u.CreatedAt, u.Disabled, u.APITokenHash, u.BrowserTokenHash, u.Role, u.Quota)
	return mapErr(err)
}

func (r pgUsers) List(ctx context.Context) ([]*User, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanUsers(rows)
}

func (r pgUsers) Update(ctx context.Context, u *User) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx, `UPDATE users SET role=$2, disabled=$3, quota=$4 WHERE id=$1 RETURNING `+userCols,
		u.ID, u.Role, u.Disabled, u.Quota))
}

func (r pgUsers) SetPasswordHash(ctx context.Context, id, hash string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE users SET password_hash=$2 WHERE id=$1`, id, hash)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r pgUsers) SetRole(ctx context.Context, id, role string) error {
	_, err := r.pool.Exec(ctx, `UPDATE users SET role=$2 WHERE id=$1`, id, role)
	return mapErr(err)
}

func (r pgUsers) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r pgUsers) GetByID(ctx context.Context, id string) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

func (r pgUsers) GetByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE username=$1`, username))
}

func (r pgUsers) GetByAPITokenHash(ctx context.Context, hash string) (*User, error) {
	if hash == "" {
		return nil, ErrNotFound
	}
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE api_token_hash=$1`, hash))
}

func (r pgUsers) UpsertPassword(ctx context.Context, username, hash string) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx, `
		INSERT INTO users (id, username, password_hash) VALUES ($1,$2,$3)
		ON CONFLICT (username) DO UPDATE SET password_hash = EXCLUDED.password_hash
		RETURNING `+userCols, NewID(), username, hash))
}

func (r pgUsers) GetByBrowserTokenHash(ctx context.Context, hash string) (*User, error) {
	if hash == "" {
		return nil, ErrNotFound
	}
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE browser_token_hash=$1`, hash))
}

func (r pgUsers) SetBrowserTokenHash(ctx context.Context, id, hash string) error {
	_, err := r.pool.Exec(ctx, `UPDATE users SET browser_token_hash=$2 WHERE id=$1`, id, hash)
	return mapErr(err)
}

func (r pgUsers) SetAPITokenHash(ctx context.Context, id, hash string) error {
	_, err := r.pool.Exec(ctx, `UPDATE users SET api_token_hash=$2 WHERE id=$1`, id, hash)
	return mapErr(err)
}

// ---- catalog ----

type pgCatalog struct{ pool *pgxpool.Pool }

const templateCols = `id, name, description, preset, image, icon_url, hdr, command, pvc_size, storage_class,
	resources, env, host_ipc, capabilities, enabled, created_at, updated_at`

func scanTemplate(row pgx.Row) (*Template, error) {
	var t Template
	err := row.Scan(&t.ID, &t.Name, &t.Description, &t.Preset, &t.Image, &t.IconURL, &t.HDR, &t.Command, &t.PVCSize, &t.StorageClass,
		&t.Resources, &t.Env, &t.HostIPC, &t.Capabilities, &t.Enabled, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

func normaliseTemplate(t *Template) {
	if t.Env == nil {
		t.Env = map[string]string{}
	}
	if t.Capabilities == nil {
		t.Capabilities = []string{}
	}
}

func (r pgCatalog) Create(ctx context.Context, t *Template) error {
	if t.ID == "" {
		t.ID = NewID()
	}
	now := time.Now()
	t.CreatedAt, t.UpdatedAt = now, now
	normaliseTemplate(t)
	_, err := r.pool.Exec(ctx, `INSERT INTO catalog (`+templateCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		t.ID, t.Name, t.Description, t.Preset, t.Image, t.IconURL, t.HDR, t.Command, t.PVCSize, t.StorageClass,
		t.Resources, t.Env, t.HostIPC, t.Capabilities, t.Enabled, t.CreatedAt, t.UpdatedAt)
	return mapErr(err)
}

func (r pgCatalog) Get(ctx context.Context, id string) (*Template, error) {
	return scanTemplate(r.pool.QueryRow(ctx, `SELECT `+templateCols+` FROM catalog WHERE id=$1`, id))
}

func (r pgCatalog) List(ctx context.Context) ([]*Template, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+templateCols+` FROM catalog ORDER BY name`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, mapErr(rows.Err())
}

func (r pgCatalog) Update(ctx context.Context, t *Template) (*Template, error) {
	normaliseTemplate(t)
	return scanTemplate(r.pool.QueryRow(ctx, `UPDATE catalog SET name=$2, description=$3, preset=$4, image=$5, icon_url=$6, hdr=$7, command=$8,
		pvc_size=$9, storage_class=$10, resources=$11, env=$12, host_ipc=$13, capabilities=$14, enabled=$15, updated_at=now()
		WHERE id=$1 RETURNING `+templateCols,
		t.ID, t.Name, t.Description, t.Preset, t.Image, t.IconURL, t.HDR, t.Command, t.PVCSize, t.StorageClass,
		t.Resources, t.Env, t.HostIPC, t.Capabilities, t.Enabled))
}

func (r pgCatalog) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM catalog WHERE id=$1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- apps ----

type pgApps struct{ pool *pgxpool.Pool }

const appCols = `id, owner_id, moonlight_id, name, preset, image, icon_url, hdr, command, pvc_size, storage_class,
	resources, env, host_ipc, capabilities, state, state_reason, generation, stream, slot, wolf_session_id, stream_url,
	created_at, updated_at, last_active_at, template_id`

func scanApp(row pgx.Row) (*App, error) {
	var a App
	var template *string
	err := row.Scan(&a.ID, &a.OwnerID, &a.MoonlightID, &a.Name, &a.Preset, &a.Image, &a.IconURL, &a.HDR, &a.Command, &a.PVCSize, &a.StorageClass,
		&a.Resources, &a.Env, &a.HostIPC, &a.Capabilities, &a.State, &a.StateReason, &a.Generation, &a.Stream, &a.Slot, &a.WolfSessionID, &a.StreamURL,
		&a.CreatedAt, &a.UpdatedAt, &a.LastActiveAt, &template)
	if err != nil {
		return nil, mapErr(err)
	}
	if template != nil {
		a.TemplateID = *template
	}
	return &a, nil
}

// nullable turns "" into SQL NULL for optional foreign keys.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func scanApps(rows pgx.Rows) ([]*App, error) {
	defer rows.Close()
	var out []*App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, mapErr(rows.Err())
}

func normaliseApp(a *App) {
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	if a.Capabilities == nil {
		a.Capabilities = []string{}
	}
}

func (r pgApps) Create(ctx context.Context, a *App) error {
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
	_, err := r.pool.Exec(ctx, `INSERT INTO apps (`+appCols+`) VALUES
		($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)`,
		a.ID, a.OwnerID, a.MoonlightID, a.Name, a.Preset, a.Image, a.IconURL, a.HDR, a.Command, a.PVCSize, a.StorageClass,
		a.Resources, a.Env, a.HostIPC, a.Capabilities, a.State, a.StateReason, a.Generation, a.Stream, a.Slot, a.WolfSessionID, a.StreamURL,
		a.CreatedAt, a.UpdatedAt, a.LastActiveAt, nullable(a.TemplateID))
	return mapErr(err)
}

func (r pgApps) Get(ctx context.Context, id string) (*App, error) {
	return scanApp(r.pool.QueryRow(ctx, `SELECT `+appCols+` FROM apps WHERE id=$1`, id))
}

func (r pgApps) GetByMoonlightID(ctx context.Context, ownerID string, moonlightID int32) (*App, error) {
	return scanApp(r.pool.QueryRow(ctx, `SELECT `+appCols+` FROM apps WHERE owner_id=$1 AND moonlight_id=$2`, ownerID, moonlightID))
}

func (r pgApps) List(ctx context.Context, ownerID string) ([]*App, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+appCols+` FROM apps WHERE owner_id=$1 ORDER BY name, created_at`, ownerID)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanApps(rows)
}

func (r pgApps) ListAll(ctx context.Context) ([]*App, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+appCols+` FROM apps ORDER BY created_at DESC`)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanApps(rows)
}

func (r pgApps) Update(ctx context.Context, a *App) (*App, error) {
	normaliseApp(a)
	return scanApp(r.pool.QueryRow(ctx, `UPDATE apps SET name=$2, preset=$3, image=$4, icon_url=$5, hdr=$6, command=$7,
		resources=$8, env=$9, host_ipc=$10, capabilities=$11, template_id=$12, updated_at=now()
		WHERE id=$1 RETURNING `+appCols,
		a.ID, a.Name, a.Preset, a.Image, a.IconURL, a.HDR, a.Command, a.Resources, a.Env, a.HostIPC, a.Capabilities, nullable(a.TemplateID)))
}

func (r pgApps) SetState(ctx context.Context, id, state, reason string) (*App, error) {
	return scanApp(r.pool.QueryRow(ctx, `UPDATE apps SET state=$2, state_reason=$3, updated_at=now()
		WHERE id=$1 RETURNING `+appCols, id, state, reason))
}

func (r pgApps) Launch(ctx context.Context, id string, stream *Stream, slot int) (*App, error) {
	return scanApp(r.pool.QueryRow(ctx, `UPDATE apps SET state=$2, state_reason='', generation=generation+1, stream=$3, slot=$4,
		wolf_session_id='', stream_url='', updated_at=now(), last_active_at=now()
		WHERE id=$1 RETURNING `+appCols, id, StateStarting, stream, slot))
}

func (r pgApps) SetStream(ctx context.Context, id string, stream *Stream) (*App, error) {
	return scanApp(r.pool.QueryRow(ctx, `UPDATE apps SET stream=$2, wolf_session_id='', updated_at=now(), last_active_at=now()
		WHERE id=$1 RETURNING `+appCols, id, stream))
}

func (r pgApps) SetRuntime(ctx context.Context, id, wolfSessionID, streamURL string) (*App, error) {
	return scanApp(r.pool.QueryRow(ctx, `UPDATE apps SET wolf_session_id=$2, stream_url=$3, updated_at=now()
		WHERE id=$1 RETURNING `+appCols, id, wolfSessionID, streamURL))
}

func (r pgApps) ClearRuntime(ctx context.Context, id string) (*App, error) {
	return scanApp(r.pool.QueryRow(ctx, `UPDATE apps SET slot=-1, wolf_session_id='', stream_url='', updated_at=now()
		WHERE id=$1 RETURNING `+appCols, id))
}

func (r pgApps) TouchActive(ctx context.Context, id string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE apps SET last_active_at=$2 WHERE id=$1 AND (last_active_at IS NULL OR last_active_at < $2)`, id, at)
	return mapErr(err)
}

func (r pgApps) Delete(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM apps WHERE id=$1`, id)
	return mapErr(err)
}

func (r pgApps) UsedSlots(ctx context.Context) ([]int, error) {
	rows, err := r.pool.Query(ctx, `SELECT slot FROM apps WHERE slot >= 0 AND state <> $1`, StateStopped)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var s int
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r pgApps) CountByState(ctx context.Context) (map[string]int, error) {
	rows, err := r.pool.Query(ctx, `SELECT state, count(*) FROM apps GROUP BY state`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// ---- pairings ----

type pgPairings struct{ pool *pgxpool.Pool }

const pairingCols = "id, user_id, cert_pem, name, via, created_at, last_seen_at"

func scanPairing(row pgx.Row) (*Pairing, error) {
	var p Pairing
	if err := row.Scan(&p.ID, &p.UserID, &p.CertPEM, &p.Name, &p.Via, &p.CreatedAt, &p.LastSeenAt); err != nil {
		return nil, mapErr(err)
	}
	return &p, nil
}

func (r pgPairings) Upsert(ctx context.Context, p *Pairing) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO pairings (`+pairingCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (id) DO UPDATE SET user_id=EXCLUDED.user_id, cert_pem=EXCLUDED.cert_pem, name=EXCLUDED.name, via=EXCLUDED.via`,
		p.ID, p.UserID, p.CertPEM, p.Name, p.Via, p.CreatedAt, p.LastSeenAt)
	return mapErr(err)
}

func (r pgPairings) Get(ctx context.Context, id string) (*Pairing, error) {
	return scanPairing(r.pool.QueryRow(ctx, `SELECT `+pairingCols+` FROM pairings WHERE id=$1`, id))
}

func (r pgPairings) List(ctx context.Context, userID string) ([]*Pairing, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+pairingCols+` FROM pairings WHERE user_id=$1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*Pairing
	for rows.Next() {
		p, err := scanPairing(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r pgPairings) Delete(ctx context.Context, userID, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM pairings WHERE id=$1 AND user_id=$2`, id, userID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r pgPairings) TouchSeen(ctx context.Context, id string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE pairings SET last_seen_at=$2 WHERE id=$1 AND (last_seen_at IS NULL OR last_seen_at < $2)`, id, at)
	return mapErr(err)
}

// ---- events ----

type pgEvents struct{ pool *pgxpool.Pool }

func (r pgEvents) Add(ctx context.Context, appID, kind, message string) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO app_events (app_id, kind, message) VALUES ($1,$2,$3)`, appID, kind, message)
	return mapErr(err)
}

func (r pgEvents) List(ctx context.Context, appID string, limit int) ([]*Event, error) {
	if limit <= 0 {
		limit = EventsKeep
	}
	rows, err := r.pool.Query(ctx, `SELECT id, app_id, at, kind, message FROM app_events WHERE app_id=$1 ORDER BY id DESC LIMIT $2`, appID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.AppID, &e.At, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (r pgEvents) Prune(ctx context.Context, appID string, keep int) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM app_events WHERE app_id=$1 AND id NOT IN
		(SELECT id FROM app_events WHERE app_id=$1 ORDER BY id DESC LIMIT $2)`, appID, keep)
	return mapErr(err)
}
