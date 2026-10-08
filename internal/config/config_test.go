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
