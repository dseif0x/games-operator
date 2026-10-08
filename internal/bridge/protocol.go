// Package bridge is the contract between the hub and the wolf-bridge
// sidecar that exposes Wolf's unix socket API inside a session pod.
package bridge

import "time"

// Port is the TCP port the bridge listens on inside the pod.
const Port = 8443

// EnvToken is the environment variable carrying the bearer token the hub
// must present; it is unique per app generation.
const EnvToken = "WOLF_BRIDGE_TOKEN"

// EnvSocket is the path of Wolf's API socket.
const EnvSocket = "WOLF_SOCKET_PATH"

// DefaultSocket is where the session pod mounts Wolf's socket directory.
const DefaultSocket = "/etc/wolf/wolf.sock"

// Status is what GET /status on the bridge returns: whether a Moonlight
// client is currently streaming, derived from Wolf's event stream.
type Status struct {
	Ready     bool      `json:"ready"`
	Streaming bool      `json:"streaming"`
	Since     time.Time `json:"since"`
	// Events counts Wolf events seen, as a liveness hint for the hub.
	Events int64 `json:"events"`
}

// Wolf event types on /api/v1/events that change the streaming state.
const (
	EventStreamSession = "wolf::core::events::StreamSession"
	EventResumeStream  = "wolf::core::events::ResumeStreamEvent"
	EventPauseStream   = "wolf::core::events::PauseStreamEvent"
	EventStopStream    = "wolf::core::events::StopStreamEvent"
)
