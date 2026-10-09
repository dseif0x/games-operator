package wolf

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Moonlight talks to one Wolf's own Moonlight HTTPS endpoints (/launch,
// /resume, /cancel) as a paired client: the hub's client certificate is
// listed in that Wolf's config (ConfigOptions.ClientCertPEM).
//
// This is how streams are started instead of the sessions/add API: Wolf's
// HTTPS resume creates the new session with the client's new keys but
// carries the compositor and the input devices over, so the app keeps
// running across a re-key (a client reconnecting, a browser reload, a
// transport or codec fallback). The API can only add and stop sessions,
// and a stopped session takes the compositor, and with it the app, down.
type Moonlight struct {
	// BaseURL is https://<pod ip>:<https port>.
	BaseURL string
	// UniqueID is the hub's Moonlight client id (any stable string).
	UniqueID string
	HTTP     *http.Client
}

// NewMoonlight returns a client presenting cert; Wolf's certificate is
// self-signed and not verified (the hop stays inside the cluster).
func NewMoonlight(baseURL, uniqueID string, cert tls.Certificate) *Moonlight {
	return &Moonlight{
		BaseURL: baseURL, UniqueID: uniqueID,
		HTTP: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext:     (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // Wolf's self-signed listener inside the pod
			},
		},
	}
}

// LaunchParams are the client's stream parameters, passed through to Wolf
// the way Moonlight sends them.
type LaunchParams struct {
	AppID    string
	AESKey   string // rikey
	AESIV    string // rikeyid
	Width    int
	Height   int
	FPS      int
	Surround int // surroundAudioInfo; 0 = stereo default
}

type launchXML struct {
	XMLName       xml.Name `xml:"root"`
	StatusCode    int      `xml:"status_code,attr"`
	StatusMessage string   `xml:"status_message,attr"`
	SessionURL    string   `xml:"sessionUrl0"`
}

// Launch starts a stream for the app, or resumes the running one with the
// new keys (Wolf decides: a client with a session is resumed). It returns
// the RTSP URL Moonlight is to connect to; its host is Wolf's per-session
// marker, which the client sends back in every RTSP request.
func (m *Moonlight) Launch(ctx context.Context, p LaunchParams) (string, error) {
	q := url.Values{}
	q.Set("uniqueid", m.UniqueID)
	q.Set("uuid", m.UniqueID)
	q.Set("appid", p.AppID)
	q.Set("rikey", p.AESKey)
	q.Set("rikeyid", p.AESIV)
	w, h, fps := p.Width, p.Height, p.FPS
	if w <= 0 || h <= 0 {
		w, h = 1920, 1080
	}
	if fps <= 0 {
		fps = 60
	}
	q.Set("mode", fmt.Sprintf("%dx%dx%d", w, h, fps))
	surround := p.Surround
	if surround == 0 {
		surround = 196610
	}
	q.Set("surroundAudioInfo", strconv.Itoa(surround))
	q.Set("localAudioPlayMode", "0")
	q.Set("corever", "1")
	out, err := m.get(ctx, "/launch", q)
	if err != nil {
		return "", err
	}
	if out.SessionURL == "" {
		return "", errors.New("wolf launch: no sessionUrl0 in the answer")
	}
	return out.SessionURL, nil
}

// Cancel ends the hub's stream on this Wolf (the session and the app's
// compositor).
func (m *Moonlight) Cancel(ctx context.Context) error {
	q := url.Values{}
	q.Set("uniqueid", m.UniqueID)
	q.Set("uuid", m.UniqueID)
	_, err := m.get(ctx, "/cancel", q)
	return err
}

func (m *Moonlight) get(ctx context.Context, path string, q url.Values) (*launchXML, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.BaseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	res, err := m.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wolf %s: %w", path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out launchXML
	if err := xml.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("wolf %s: status %d: %s", path, res.StatusCode, string(raw))
	}
	if res.StatusCode != http.StatusOK || (out.StatusCode != 0 && out.StatusCode != http.StatusOK) {
		msg := out.StatusMessage
		if msg == "" {
			msg = res.Status
		}
		return nil, fmt.Errorf("wolf %s: %s", path, msg)
	}
	return &out, nil
}
