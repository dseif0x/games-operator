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
	// HostIPC shares the node's IPC namespace. Off: Wolf and the app share
	// the pod's IPC namespace already, which is all the compositor needs.
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
// steamCaps: SYS_NICE for Steam's thread priorities, SYS_PTRACE for its
// crash handler, SYS_RESOURCE for its limits. Proton's sandbox
// (pressure-vessel, bwrap) runs in an unprivileged user namespace, which
// needs no capability once the node allows it (devices.sysctls); SYS_ADMIN
// is not granted any more, as it is a known way to root on the node.
var steamCaps = []string{"SYS_NICE", "SYS_PTRACE", "SYS_RESOURCE"}

// lightCaps are enough for launchers that spawn games themselves: the
// runtime's default set (NET_RAW, MKNOD and friends) covers them.
var lightCaps = []string{}

var presets = map[string]Preset{
	"steam": {
		Key: "steam", Title: "Steam", Description: "Steam Big Picture with Proton; the heavy one.",
		Image: "ghcr.io/games-on-whales/steam:edge", IconURL: "https://cdn.freebiesupply.com/images/large/2x/steam-logo-transparent.png",
		Env: withCommon(map[string]string{"PROTON_LOG": "1"}), Capabilities: steamCaps, HostIPC: false, Unconfined: true,
	},
	"firefox": {
		Key: "firefox", Title: "Firefox", Description: "A browser on the big screen.",
		Image: "ghcr.io/games-on-whales/firefox:edge", IconURL: "https://raw.githubusercontent.com/games-on-whales/gow/master/apps/firefox/assets/icon.png",
		Env: withCommon(nil),
	},
	"prismlauncher": {
		Key: "prismlauncher", Title: "Prism Launcher", Description: "Minecraft, with mod packs.",
		Image: "ghcr.io/games-on-whales/prismlauncher:edge", IconURL: "https://raw.githubusercontent.com/games-on-whales/gow/master/apps/prismlauncher/assets/icon.png",
		Env: withCommon(nil), Capabilities: lightCaps, HostIPC: false,
	},
	"retroarch": {
		Key: "retroarch", Title: "RetroArch", Description: "Emulation frontend.",
		Image: "ghcr.io/games-on-whales/retroarch:edge", IconURL: "https://raw.githubusercontent.com/games-on-whales/gow/master/apps/retroarch/assets/icon.png",
		Env: withCommon(nil), Capabilities: lightCaps, HostIPC: false,
	},
	"lutris": {
		Key: "lutris", Title: "Lutris", Description: "GOG, Epic and everything Wine.",
		Image: "ghcr.io/games-on-whales/lutris:edge", IconURL: "https://raw.githubusercontent.com/games-on-whales/gow/master/apps/lutris/assets/icon.png",
		Env: withCommon(nil), Capabilities: steamCaps, HostIPC: false, Unconfined: true,
	},
	"heroic": {
		Key: "heroic", Title: "Heroic", Description: "Epic, GOG and Amazon launcher.",
		Image: "ghcr.io/games-on-whales/heroic:edge", IconURL: "https://raw.githubusercontent.com/games-on-whales/gow/master/apps/heroic/assets/icon.png",
		Env: withCommon(nil), Capabilities: steamCaps, HostIPC: false, Unconfined: true,
	},
	"pegasus": {
		Key: "pegasus", Title: "Pegasus", Description: "Game library frontend.",
		Image: "ghcr.io/games-on-whales/pegasus:edge", IconURL: "https://raw.githubusercontent.com/games-on-whales/gow/master/apps/pegasus/assets/icon.png",
		Env: withCommon(nil), Capabilities: lightCaps, HostIPC: false,
	},
	"custom": {
		Key: "custom", Title: "Custom image", Description: "Any Games on Whales compatible image.",
		Env: withCommon(nil), Capabilities: lightCaps, HostIPC: false, Custom: true,
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
