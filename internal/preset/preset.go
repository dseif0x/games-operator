// Package preset describes the ready-made app configurations (Steam,
// Firefox, …) and the launch wrapper every app container runs.
package preset

import "sort"

// Preset is a known-good app configuration from the Games on Whales
// catalogue. Users pick one and only override what they need.
type Preset struct {
	Key         string            `json:"key"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Image       string            `json:"image"`
	IconURL     string            `json:"icon_url"`
	Env         map[string]string `json:"env"`
	// Capabilities the app container needs beyond the defaults.
	Capabilities []string `json:"capabilities"`
	// HostIPC shares the node's IPC namespace (Steam's compositor needs it).
	HostIPC bool `json:"host_ipc"`
	// Unconfined drops the seccomp and AppArmor profiles (Steam/Proton).
	Unconfined bool `json:"unconfined"`
	// Custom marks the free-form preset whose image the user supplies.
	Custom bool `json:"custom"`
}

// DefaultCommand is the launch wrapper: wait for the compositor and audio
// sockets that Wolf creates for the stream, point PulseAudio at Wolf's
// virtual sink, then hand over to the image's own entrypoint.
const DefaultCommand = `cleanup() { echo "Cleaning up..." >&2; exit 0; }
trap cleanup SIGINT SIGTERM
while [ ! -S "$XDG_RUNTIME_DIR/$WAYLAND_DISPLAY" ]; do echo "Waiting for wayland display..." >&2; sleep 1; done
while [ ! -S "$XDG_RUNTIME_DIR/pulse-socket" ]; do echo "Waiting for pulse socket..." >&2; sleep 1; done
export PULSE_SINK="$(pactl list sinks short | awk '$2 ~ /^virtual_sink/ {print $2; exit}')"
export PULSE_SOURCE="${PULSE_SINK}.monitor"
chown -R "$PUID:$PGID" /home/retro
exec /entrypoint.sh`

// commonEnv is what every Games on Whales image expects.
var commonEnv = map[string]string{
	"RUN_SWAY":             "true",
	"GOW_REQUIRED_DEVICES": "/dev/input/* /dev/dri/* /dev/nvidia*",
}

// steamCaps are what Steam (Proton, bubblewrap, gamescope) asks for.
var steamCaps = []string{"SYS_ADMIN", "SYS_NICE", "SYS_PTRACE", "NET_RAW", "MKNOD", "NET_ADMIN", "SYS_RESOURCE"}

// lightCaps are enough for launchers that spawn games themselves.
var lightCaps = []string{"NET_RAW", "MKNOD", "NET_ADMIN"}

var presets = map[string]Preset{
	"steam": {
		Key: "steam", Title: "Steam", Description: "Steam Big Picture with Proton; the heavy one.",
		Image: "ghcr.io/games-on-whales/steam:edge", IconURL: "https://cdn.freebiesupply.com/images/large/2x/steam-logo-transparent.png",
		Env: withCommon(map[string]string{"PROTON_LOG": "1"}), Capabilities: steamCaps, HostIPC: true, Unconfined: true,
	},
	"firefox": {
		Key: "firefox", Title: "Firefox", Description: "A browser on the big screen.",
		Image: "ghcr.io/games-on-whales/firefox:edge", IconURL: "https://raw.githubusercontent.com/games-on-whales/gow/master/apps/firefox/assets/icon.png",
		Env: withCommon(nil),
	},
	"prismlauncher": {
		Key: "prismlauncher", Title: "Prism Launcher", Description: "Minecraft, with mod packs.",
		Image: "ghcr.io/games-on-whales/prismlauncher:edge", IconURL: "https://raw.githubusercontent.com/games-on-whales/gow/master/apps/prismlauncher/assets/icon.png",
		Env: withCommon(nil), Capabilities: lightCaps, HostIPC: true,
	},
	"retroarch": {
		Key: "retroarch", Title: "RetroArch", Description: "Emulation frontend.",
		Image: "ghcr.io/games-on-whales/retroarch:edge", IconURL: "https://raw.githubusercontent.com/games-on-whales/gow/master/apps/retroarch/assets/icon.png",
		Env: withCommon(nil), Capabilities: lightCaps, HostIPC: true,
	},
	"lutris": {
		Key: "lutris", Title: "Lutris", Description: "GOG, Epic and everything Wine.",
		Image: "ghcr.io/games-on-whales/lutris:edge", IconURL: "https://raw.githubusercontent.com/games-on-whales/gow/master/apps/lutris/assets/icon.png",
		Env: withCommon(nil), Capabilities: steamCaps, HostIPC: true, Unconfined: true,
	},
	"heroic": {
		Key: "heroic", Title: "Heroic", Description: "Epic, GOG and Amazon launcher.",
		Image: "ghcr.io/games-on-whales/heroic:edge", IconURL: "https://raw.githubusercontent.com/games-on-whales/gow/master/apps/heroic/assets/icon.png",
		Env: withCommon(nil), Capabilities: steamCaps, HostIPC: true, Unconfined: true,
	},
	"pegasus": {
		Key: "pegasus", Title: "Pegasus", Description: "Game library frontend.",
		Image: "ghcr.io/games-on-whales/pegasus:edge", IconURL: "https://raw.githubusercontent.com/games-on-whales/gow/master/apps/pegasus/assets/icon.png",
		Env: withCommon(nil), Capabilities: lightCaps, HostIPC: true,
	},
	"custom": {
		Key: "custom", Title: "Custom image", Description: "Any Games on Whales compatible image.",
		Env: withCommon(nil), Capabilities: lightCaps, HostIPC: true, Custom: true,
	},
}

func withCommon(extra map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range commonEnv {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// Get returns the preset for key.
func Get(key string) (Preset, bool) {
	p, ok := presets[key]
	return p, ok
}

// All lists the presets, custom last.
func All() []Preset {
	out := make([]Preset, 0, len(presets))
	for _, p := range presets {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Custom != out[j].Custom {
			return !out[i].Custom
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// Keys lists the preset keys.
func Keys() []string {
	ps := All()
	keys := make([]string, len(ps))
	for i, p := range ps {
		keys[i] = p.Key
	}
	return keys
}
