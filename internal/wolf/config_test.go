package wolf

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestGenerateConfig(t *testing.T) {
	b, err := GenerateConfig(ConfigOptions{Hostname: "games-operator", UUID: "u", AppTitle: "Steam", RenderNode: "/dev/dri/renderD128"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Hostname      string `toml:"hostname"`
		ConfigVersion int    `toml:"config_version"`
		PairedClients []any  `toml:"paired_clients"`
		Profiles      []struct {
			ID   string `toml:"id"`
			Apps []struct {
				Title      string         `toml:"title"`
				RenderNode string         `toml:"render_node"`
				Runner     map[string]any `toml:"runner"`
			} `toml:"apps"`
		} `toml:"profiles"`
		Gstreamer map[string]any `toml:"gstreamer"`
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "games-operator" || cfg.ConfigVersion != 7 || len(cfg.PairedClients) != 0 {
		t.Fatalf("%+v", cfg)
	}
	if len(cfg.Profiles) != 1 || cfg.Profiles[0].ID != "moonlight-profile-id" || len(cfg.Profiles[0].Apps) != 1 {
		t.Fatalf("profiles: %+v", cfg.Profiles)
	}
	app := cfg.Profiles[0].Apps[0]
	if app.Title != "Steam" || app.RenderNode != "/dev/dri/renderD128" || app.Runner["type"] != "process" {
		t.Fatalf("app: %+v", app)
	}
	if _, ok := cfg.Gstreamer["video"]; !ok {
		t.Fatal("gstreamer defaults missing")
	}
	if strings.Contains(string(b), "wolf-ui") {
		t.Fatal("default docker apps must not survive")
	}
}
