package wolf

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dseif0x/games-operator/internal/moonlight"
)

func TestMoonlightLaunchAndCancel(t *testing.T) {
	cert, err := moonlight.LoadOrCreateClientCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "no client certificate", http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		switch r.URL.Path {
		case "/launch":
			if q.Get("appid") != "app-1" || q.Get("rikey") != "k" || q.Get("rikeyid") != "iv" || q.Get("mode") != "1280x720x60" || q.Get("surroundAudioInfo") != "196610" {
				http.Error(w, "bad query "+r.URL.RawQuery, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?><root status_code="200"><sessionUrl0>rtsp://149.3.9.77:48100</sessionUrl0><gamesession>1</gamesession></root>`))
		case "/cancel":
			_, _ = w.Write([]byte(`<root status_code="200"><cancel>1</cancel></root>`))
		default:
			http.NotFound(w, r)
		}
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	m := NewMoonlight(srv.URL, "games-operator", cert)
	url, err := m.Launch(context.Background(), LaunchParams{AppID: "app-1", AESKey: "k", AESIV: "iv", Width: 1280, Height: 720, FPS: 60})
	if err != nil || url != "rtsp://149.3.9.77:48100" {
		t.Fatalf("launch: %v %q", err, url)
	}
	if err := m.Cancel(context.Background()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(seen) != 2 || seen[0] != "/launch" || seen[1] != "/cancel" {
		t.Fatalf("requests: %v", seen)
	}
	// Wolf answers errors as XML with a status attribute.
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<root status_code="400" status_message="no such app"></root>`))
	}))
	defer bad.Close()
	if _, err := NewMoonlight(bad.URL, "x", cert).Launch(context.Background(), LaunchParams{AppID: "nope"}); err == nil {
		t.Fatal("an error status must be an error")
	}
}
