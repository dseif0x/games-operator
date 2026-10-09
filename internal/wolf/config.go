package wolf

import (
	_ "embed"
	"fmt"

	"github.com/pelletier/go-toml/v2"
)

// defaultConfig is Wolf's own default config.toml (stable, version 7). The
// gstreamer section is what matters: it carries the encoder pipelines for
// nvcodec, QSV and VA-API that Wolf expects to find in the file.
//
//go:embed config.default.toml
var defaultConfig []byte

// ConfigOptions shape the generated config.toml.
type ConfigOptions struct {
	Hostname string
	UUID     string
	// AppTitle names the one app in the Moonlight profile. Wolf's
	// sessions/add uses the first profile app as the pipeline template.
	AppTitle string
	// RenderNode pins the DRI render node; empty lets Wolf pick.
	RenderNode string
	// ClientCertPEM is the hub's Moonlight client certificate. Listed as a
	// paired client, it lets the hub call Wolf's own HTTPS launch, resume
	// and cancel: Wolf then keeps the compositor and devices across a
	// resume, which its API cannot do. AppStateFolder names the client's
	// state directory under Wolf's data folder (the app id).
	ClientCertPEM  string
	AppStateFolder string
}

// GenerateConfig renders the config.toml for a session pod: Wolf's default
// gstreamer settings, no paired clients (the hub does the pairing), and a
// single Moonlight profile whose only app is a no-op process. The real
// application runs in a sibling container that attaches to the compositor
// and audio sockets Wolf creates for the stream.
func GenerateConfig(o ConfigOptions) ([]byte, error) {
	var cfg map[string]any
	if err := toml.Unmarshal(defaultConfig, &cfg); err != nil {
		return nil, fmt.Errorf("parse default wolf config: %w", err)
	}
	app := map[string]any{
		"title":                    o.AppTitle,
		"start_virtual_compositor": true,
		"start_audio_server":       true,
		"runner": map[string]any{
			"type":    "process",
			"run_cmd": `sh -c "while :; do sleep 3600; done"`,
		},
	}
	if o.RenderNode != "" {
		app["render_node"] = o.RenderNode
	}
	cfg["hostname"] = o.Hostname
	cfg["uuid"] = o.UUID
	cfg["paired_clients"] = []any{}
	if o.ClientCertPEM != "" {
		folder := o.AppStateFolder
		if folder == "" {
			folder = o.UUID
		}
		cfg["paired_clients"] = []any{map[string]any{"client_cert": o.ClientCertPEM, "app_state_folder": folder}}
	}
	cfg["profiles"] = []any{map[string]any{"id": "moonlight-profile-id", "apps": []any{app}}}
	out, err := toml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("render wolf config: %w", err)
	}
	return out, nil
}
