// Package wolf talks to Wolf's HTTP API (through the wolf-bridge sidecar)
// and renders the config.toml a session pod's Wolf starts with.
package wolf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Session is the payload of POST /api/v1/sessions/add. app_id and
// client_id are left out on purpose: Wolf then creates a dummy app (with
// the first profile app's pipelines) and a dummy client, which is exactly
// what a single-app pod needs.
type Session struct {
	ClientIP          string         `json:"client_ip"`
	AESKey            string         `json:"aes_key"`
	AESIV             string         `json:"aes_iv"`
	RTSPFakeIP        string         `json:"rtsp_fake_ip"`
	VideoWidth        int            `json:"video_width"`
	VideoHeight       int            `json:"video_height"`
	VideoRefreshRate  int            `json:"video_refresh_rate"`
	AudioChannelCount int            `json:"audio_channel_count"`
	ClientSettings    ClientSettings `json:"client_settings"`
}

// ClientSettings mirrors Wolf's per-client settings.
type ClientSettings struct {
	RunUID              int      `json:"run_uid"`
	RunGID              int      `json:"run_gid"`
	ControllersOverride []string `json:"controllers_override"`
	MouseAcceleration   float64  `json:"mouse_acceleration"`
	VScrollAcceleration float64  `json:"v_scroll_acceleration"`
	HScrollAcceleration float64  `json:"h_scroll_acceleration"`
}

// DefaultClientSettings is what every stream gets.
func DefaultClientSettings() ClientSettings {
	return ClientSettings{RunUID: 1000, RunGID: 1000, ControllersOverride: []string{}, MouseAcceleration: 1, VScrollAcceleration: 1, HScrollAcceleration: 1}
}

// RunningSession is one entry of GET /api/v1/sessions.
type RunningSession struct {
	ClientIP   string `json:"client_ip"`
	AESKey     string `json:"aes_key"`
	AESIV      string `json:"aes_iv"`
	RTSPFakeIP string `json:"rtsp_fake_ip"`
	AppID      string `json:"app_id"`
	// ClientID is what Wolf calls the session id on the way out.
	ClientID string `json:"client_id"`
	Width    int    `json:"video_width"`
	Height   int    `json:"video_height"`
	FPS      int    `json:"video_refresh_rate"`
}

// Client calls the Wolf API behind a bridge.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient returns a client for the bridge at baseURL (http://ip:port).
func NewClient(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

type response struct {
	Success   bool             `json:"success"`
	Error     string           `json:"error"`
	SessionID string           `json:"session_id"`
	Sessions  []RunningSession `json:"sessions"`
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("wolf %s %s: status %d: %s", method, path, res.StatusCode, bytes.TrimSpace(raw))
	}
	if !out.Success {
		if out.Error == "" {
			out.Error = fmt.Sprintf("status %d", res.StatusCode)
		}
		return nil, fmt.Errorf("wolf %s %s: %s", method, path, out.Error)
	}
	return &out, nil
}

// AddSession registers a stream and returns Wolf's session id.
func (c *Client) AddSession(ctx context.Context, s Session) (string, error) {
	out, err := c.do(ctx, http.MethodPost, "/api/v1/sessions/add", s)
	if err != nil {
		return "", err
	}
	return out.SessionID, nil
}

// ListSessions returns the sessions Wolf knows about.
func (c *Client) ListSessions(ctx context.Context) ([]RunningSession, error) {
	out, err := c.do(ctx, http.MethodGet, "/api/v1/sessions", nil)
	if err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

// StopSession ends a stream (and the app Wolf started for it).
func (c *Client) StopSession(ctx context.Context, sessionID string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/sessions/stop", map[string]string{"session_id": sessionID})
	return err
}
