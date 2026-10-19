package moonlight

import (
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dseif0x/games-operator/internal/store"
)

// fakeLauncher is the user's single app, running or not, streaming or not.
type fakeLauncher struct {
	app       *store.App
	resumeArg []bool
}

func (f *fakeLauncher) Apps(context.Context, string) ([]*store.App, error) {
	return []*store.App{f.app}, nil
}

func (f *fakeLauncher) Current(context.Context, string) (*store.App, error) {
	if f.app.State == store.StateRunning || f.app.State == store.StateStarting {
		return f.app, nil
	}
	return nil, nil
}

func (f *fakeLauncher) Launch(_ context.Context, _ *store.User, _ *store.Pairing, _ int32, s store.Stream, resume bool) (string, error) {
	f.resumeArg = append(f.resumeArg, resume)
	f.app.State, f.app.Stream = store.StateRunning, &s
	return "rtsp://10.0.0.1:48010", nil
}

func (f *fakeLauncher) Cancel(context.Context, *store.User) error {
	f.app.Stream = nil
	return nil
}

func newTestServer(l Launcher) *Server {
	return NewServer(Options{Hostname: "games", UniqueID: "u"}, nil, nil, nil, l, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// call runs a handler as an authenticated (paired) client.
func call(t *testing.T, h http.HandlerFunc, target string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	ctx := context.WithValue(r.Context(), userKey, &store.User{ID: "u1"})
	ctx = context.WithValue(ctx, pairingKey, &store.Pairing{ID: "p1", Name: "Apple TV"})
	w := httptest.NewRecorder()
	h(w, r.WithContext(ctx))
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", target, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func serverState(t *testing.T, s *Server) (state, game string) {
	t.Helper()
	var resp ServerInfoResponse
	if err := xml.Unmarshal([]byte(call(t, s.serverInfo, "/serverinfo")), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.State, resp.CurrentGame
}

const launchQuery = "?appid=7&rikey=00112233445566778899aabbccddeeff&rikeyid=1&mode=1920x1080x60"

// The Apple TV flow: a launch the client gave up on (the node was waking),
// then Resume, then Quit, which Moonlight checks by re-reading serverinfo.
func TestResumeAndQuitAsMoonlightChecksThem(t *testing.T) {
	l := &fakeLauncher{app: &store.App{ID: "a", Name: "Steam", MoonlightID: 7, State: store.StateStarting}}
	s := newTestServer(l)

	// Starting without a client's keys (the launch timed out, /cancel came):
	// the host is free, so the next tap launches.
	if state, game := serverState(t, s); state != "SUNSHINE_SERVER_FREE" || game != "0" {
		t.Fatalf("starting without a stream: %s %s", state, game)
	}

	// /launch answers <gamesession>1</gamesession> and no <resume>.
	body := call(t, s.launch, "/launch"+launchQuery)
	if !strings.Contains(body, "<gamesession>1</gamesession>") || strings.Contains(body, "<resume>") || !strings.Contains(body, "<sessionUrl0>rtsp://") {
		t.Fatalf("launch response: %s", body)
	}
	if state, game := serverState(t, s); state != "SUNSHINE_SERVER_BUSY" || game != "7" {
		t.Fatalf("streaming: %s %s", state, game)
	}

	// /resume (no appid) answers <resume>1</resume>; Moonlight's iOS/tvOS
	// client fails with "Failed to resume app" without it.
	body = call(t, s.resume, "/resume"+strings.Replace(launchQuery, "appid=7&", "", 1))
	if !strings.Contains(body, "<resume>1</resume>") || !strings.Contains(body, "<sessionUrl0>rtsp://") {
		t.Fatalf("resume response: %s", body)
	}
	if len(l.resumeArg) != 2 || l.resumeArg[0] || !l.resumeArg[1] {
		t.Fatalf("launcher resume flags = %v", l.resumeArg)
	}

	// Quit: /cancel ends the stream (the app keeps running), and the host
	// reads as free, which is what the client checks for success.
	call(t, s.cancel, "/cancel")
	if l.app.State != store.StateRunning {
		t.Fatalf("cancel stopped the app: %s", l.app.State)
	}
	if state, game := serverState(t, s); state != "SUNSHINE_SERVER_FREE" || game != "0" {
		t.Fatalf("after quit: %s %s", state, game)
	}
}
