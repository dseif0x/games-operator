// Package store is the Postgres access layer. One interface per aggregate,
// a Postgres implementation, and an in-memory implementation for tests.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/dseif0x/games-operator/internal/config"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on unique violations.
var ErrConflict = errors.New("conflict")

// App states. See docs/ARCHITECTURE.md for the transitions.
const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateFailed   = "failed"
	StateDeleting = "deleting"
)

// States lists every state, for validation and metrics.
var States = []string{StateStopped, StateStarting, StateRunning, StateStopping, StateFailed, StateDeleting}

// User is a login account.
type User struct {
	ID           string
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	Disabled     bool
	// APIToken authenticates Wolf-compatible API calls (moonlight-web's
	// auto-pairing) as this user. Stored hashed.
	APITokenHash string
}

// App is one row of the apps table: a game or desktop app a user can
// launch from Moonlight. The pod exists only while the app is running.
type App struct {
	ID      string
	OwnerID string
	// MoonlightID is the numeric id Moonlight clients use for the app.
	MoonlightID int32
	Name        string
	Preset      string
	Image       string
	IconURL     string
	HDR         bool
	// Command replaces the preset's launch command when set.
	Command      string
	PVCSize      string
	StorageClass string
	Resources    config.Resources
	Env          map[string]string
	// HostIPC and Capabilities come from the preset; stored so a custom
	// preset can set them.
	HostIPC      bool
	Capabilities []string
	State        string
	StateReason  string
	Generation   int
	// Stream holds the parameters of the current/last launch.
	Stream *Stream
	// Slot is the port set the running app uses (-1 = none).
	Slot          int
	WolfSessionID string
	StreamURL     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LastActiveAt  *time.Time
}

// Stream is what a Moonlight launch request carries.
type Stream struct {
	PairingID  string `json:"pairing_id"`
	ClientIP   string `json:"client_ip"`
	AESKey     string `json:"aes_key"`
	AESIV      string `json:"aes_iv"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	FPS        int    `json:"fps"`
	Surround   int    `json:"surround"`
	StartedAt  string `json:"started_at"`
	ClientName string `json:"client_name,omitempty"`
}

// Pairing is a Moonlight client certificate bound to a user.
type Pairing struct {
	ID         string // sha256 fingerprint of the certificate, hex
	UserID     string
	CertPEM    string
	Name       string
	CreatedAt  time.Time
	LastSeenAt *time.Time
}

// Event is one row of app_events.
type Event struct {
	ID      int64
	AppID   string
	At      time.Time
	Kind    string
	Message string
}

// Users is the user aggregate.
type Users interface {
	Create(ctx context.Context, u *User) error
	GetByID(ctx context.Context, id string) (*User, error)
	GetByUsername(ctx context.Context, username string) (*User, error)
	GetByAPITokenHash(ctx context.Context, hash string) (*User, error)
	// UpsertPassword creates the user or replaces its password hash.
	UpsertPassword(ctx context.Context, username, passwordHash string) (*User, error)
	SetAPITokenHash(ctx context.Context, id, hash string) error
}

// Apps is the app aggregate.
type Apps interface {
	Create(ctx context.Context, a *App) error
	Get(ctx context.Context, id string) (*App, error)
	GetByMoonlightID(ctx context.Context, ownerID string, moonlightID int32) (*App, error)
	List(ctx context.Context, ownerID string) ([]*App, error)
	ListAll(ctx context.Context) ([]*App, error)
	// Update replaces the editable settings and returns the updated row.
	Update(ctx context.Context, a *App) (*App, error)
	// SetState updates state and reason. It returns the updated row.
	SetState(ctx context.Context, id, state, reason string) (*App, error)
	// Launch records the stream parameters and slot, bumps the generation
	// and moves the app to starting, in one statement.
	Launch(ctx context.Context, id string, stream *Stream, slot int) (*App, error)
	// SetStream updates the stream parameters of a running app (re-key).
	SetStream(ctx context.Context, id string, stream *Stream) (*App, error)
	// SetRuntime records the Wolf session id and stream URL once the stream
	// is set up.
	SetRuntime(ctx context.Context, id, wolfSessionID, streamURL string) (*App, error)
	// ClearRuntime forgets slot, Wolf session and stream URL (app stopped).
	ClearRuntime(ctx context.Context, id string) (*App, error)
	TouchActive(ctx context.Context, id string, at time.Time) error
	Delete(ctx context.Context, id string) error
	// UsedSlots lists the slots held by apps that are not stopped.
	UsedSlots(ctx context.Context) ([]int, error)
	CountByState(ctx context.Context) (map[string]int, error)
}

// Pairings is the pairing aggregate.
type Pairings interface {
	Upsert(ctx context.Context, p *Pairing) error
	Get(ctx context.Context, id string) (*Pairing, error)
	List(ctx context.Context, userID string) ([]*Pairing, error)
	Delete(ctx context.Context, userID, id string) error
	TouchSeen(ctx context.Context, id string, at time.Time) error
}

// Events is the app_events aggregate.
type Events interface {
	Add(ctx context.Context, appID, kind, message string) error
	List(ctx context.Context, appID string, limit int) ([]*Event, error)
	// Prune keeps only the newest keep events for the app.
	Prune(ctx context.Context, appID string, keep int) error
}

// Store bundles the aggregates.
type Store interface {
	Users() Users
	Apps() Apps
	Pairings() Pairings
	Events() Events
	Ping(ctx context.Context) error
	Close()
}

// EventsKeep is how many events are retained per app.
const EventsKeep = 200

// NewID returns a random UUIDv4 string.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
