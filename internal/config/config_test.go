package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadMinimal(t *testing.T) {
	c, err := load(env(map[string]string{
		"GAMES_OPERATOR_DATABASE_URL":  "postgres://x",
		"GAMES_OPERATOR_PUBLIC_URL":    "https://games.example.com",
		"GAMES_OPERATOR_COOKIE_SECRET": strings.Repeat("ab", 32),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.MoonlightHTTPPort != 47989 || c.StreamPortBase != 48100 || c.MaxConcurrent != 10 {
		t.Fatalf("defaults: %+v", c)
	}
	if got := c.AllowedHosts; len(got) != 1 || got[0] != "games.example.com" {
		t.Fatalf("allowed hosts %v", got)
	}
	if c.DefaultResources.Limits.Extended["nvidia.com/gpu"] != "1" {
		t.Fatalf("default gpu missing: %+v", c.DefaultResources)
	}
}

func TestLoadErrors(t *testing.T) {
	_, err := load(env(map[string]string{
		"GAMES_OPERATOR_PUBLIC_URL":     "ftp://nope",
		"GAMES_OPERATOR_COOKIE_SECRET":  "short",
		"GAMES_OPERATOR_MAX_CONCURRENT": "0",
	}))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"DATABASE_URL", "PUBLIC_URL", "COOKIE_SECRET", "MAX_CONCURRENT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestResourceListJSON(t *testing.T) {
	var r Resources
	if err := r.Limits.UnmarshalJSON([]byte(`{"cpu":2,"memory":"4Gi","nvidia.com/gpu":"1"}`)); err != nil {
		t.Fatal(err)
	}
	if r.Limits.CPU != "2" || r.Limits.Memory != "4Gi" || r.Limits.Extended["nvidia.com/gpu"] != "1" {
		t.Fatalf("%+v", r.Limits)
	}
	b, _ := r.Limits.MarshalJSON()
	if !strings.Contains(string(b), `"nvidia.com/gpu":"1"`) {
		t.Fatalf("round trip: %s", b)
	}
}

func TestBasePathAndBrowserUpstream(t *testing.T) {
	base := map[string]string{
		"GAMES_OPERATOR_DATABASE_URL":  "postgres://x",
		"GAMES_OPERATOR_COOKIE_SECRET": strings.Repeat("ab", 32),
	}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := load(env(with("GAMES_OPERATOR_PUBLIC_URL", "https://games.example.com/hub/", "GAMES_OPERATOR_BROWSER_UPSTREAM", "https://mw:8443", "GAMES_OPERATOR_BROWSER_SECRET", "s3cret")))
	if err != nil {
		t.Fatal(err)
	}
	if c.BasePath != "/hub" || c.BrowserPath != "/play" || c.BrowserURL != "https://games.example.com/play/" || c.BrowserUpstream.Host != "mw:8443" {
		t.Fatalf("base %q browser path %q url %q upstream %v", c.BasePath, c.BrowserPath, c.BrowserURL, c.BrowserUpstream)
	}
	c, err = load(env(with("GAMES_OPERATOR_PUBLIC_URL", "https://games.example.com", "GAMES_OPERATOR_BROWSER_UPSTREAM", "https://mw:8443", "GAMES_OPERATOR_BROWSER_SECRET", "s3cret", "GAMES_OPERATOR_BROWSER_PATH", "/stream/")))
	if err != nil || c.BrowserPath != "/stream" || c.BrowserURL != "https://games.example.com/stream/" {
		t.Fatalf("custom path: %v %q %q", err, c.BrowserPath, c.BrowserURL)
	}
	c, err = load(env(with("GAMES_OPERATOR_PUBLIC_URL", "https://games.example.com", "GAMES_OPERATOR_BROWSER_URL", "https://play.example.com")))
	if err != nil || c.BasePath != "" || c.BrowserURL != "https://play.example.com" {
		t.Fatalf("root: %v base %q browser %q", err, c.BasePath, c.BrowserURL)
	}
	_, err = load(env(with("GAMES_OPERATOR_PUBLIC_URL", "https://games.example.com", "GAMES_OPERATOR_BROWSER_UPSTREAM", "https://mw:8443")))
	if err == nil || !strings.Contains(err.Error(), "BROWSER_SECRET is required") {
		t.Fatalf("upstream without a secret must be rejected: %v", err)
	}
	_, err = load(env(with("GAMES_OPERATOR_PUBLIC_URL", "https://games.example.com", "GAMES_OPERATOR_BROWSER_UPSTREAM", "https://mw:8443", "GAMES_OPERATOR_BROWSER_SECRET", "x", "GAMES_OPERATOR_BROWSER_PATH", "/")))
	if err == nil || !strings.Contains(err.Error(), "BROWSER_PATH") {
		t.Fatalf("player at the root must be rejected: %v", err)
	}
}
