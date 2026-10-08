// Package moonlight implements the GameStream (Moonlight) HTTP/HTTPS
// protocol: server info, the four-phase PIN pairing, the app list, assets
// and launch/resume/cancel. It is ported from games-on-whales/fenrir (MIT)
// and backed by the hub's store instead of CRDs.
package moonlight

import "encoding/xml"

// XMLResponse is any XML response carrying a status code.
type XMLResponse interface {
	GetStatusCode() int
}

// Response is the common <root status_code="..."> envelope.
type Response struct {
	XMLName       xml.Name `xml:"root"`
	StatusCode    int      `xml:"status_code,attr"`
	StatusMessage string   `xml:"status_message,attr,omitempty"`
}

// GetStatusCode implements XMLResponse.
func (r Response) GetStatusCode() int { return r.StatusCode }

// ServerInfoResponse answers /serverinfo.
type ServerInfoResponse struct {
	Response
	ServerInfo
}

// ServerInfo is what Moonlight shows about a host.
type ServerInfo struct {
	Hostname               string       `xml:"hostname"`
	AppVersion             string       `xml:"appversion"`
	GfeVersion             string       `xml:"GfeVersion"`
	UniqueID               string       `xml:"uniqueid"`
	MaxLumaPixelsHEVC      int64        `xml:"MaxLumaPixelsHEVC"`
	ServerCodecModeSupport int          `xml:"ServerCodecModeSupport"`
	HTTPSPort              int          `xml:"HttpsPort"`
	ExternalPort           int          `xml:"ExternalPort"`
	MAC                    string       `xml:"mac"`
	LocalIP                string       `xml:"LocalIP"`
	SupportedDisplayModes  DisplayModes `xml:"SupportedDisplayMode"`
	PairStatus             int          `xml:"PairStatus"`
	CurrentGame            string       `xml:"currentgame"`
	State                  string       `xml:"state"`
}

// DisplayModes wraps the list of display modes.
type DisplayModes struct {
	Modes []DisplayMode `xml:"DisplayMode"`
}

// DisplayMode is one resolution/refresh rate pair.
type DisplayMode struct {
	Width       int `xml:"Width"`
	Height      int `xml:"Height"`
	RefreshRate int `xml:"RefreshRate"`
}

// AppListResponse answers /applist.
type AppListResponse struct {
	Response
	Apps []AppEntry `xml:"App"`
}

// AppEntry is one app in the list.
type AppEntry struct {
	XMLName        xml.Name `xml:"App"`
	Title          string   `xml:"AppTitle"`
	ID             int32    `xml:"ID"`
	IsHDRSupported int      `xml:"IsHdrSupported"`
}

// LaunchResponse answers /launch and /resume.
type LaunchResponse struct {
	Response
	RTSPSessionURL string `xml:"sessionUrl0"`
	GameSession    int    `xml:"gamesession"`
}

// PairingResponse answers the /pair phases.
type PairingResponse struct {
	Response
	Paired            int    `xml:"paired"`
	PlainCert         string `xml:"plaincert,omitempty"`
	ChallengeResponse string `xml:"challengeresponse,omitempty"`
	PairingSecret     string `xml:"pairingsecret,omitempty"`
}
