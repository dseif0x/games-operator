// Package config loads the hub configuration from GAMES_OPERATOR_*
// environment variables and fails fast on anything missing or malformed.
package config

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Prefix is prepended to every environment variable name.
const Prefix = "GAMES_OPERATOR_"

// Resources is a CPU/memory request and limit pair in Kubernetes quantity
// syntax. Kept free of k8s types so it can live in the store too.
type Resources struct {
	Requests ResourceList `json:"requests"`
	Limits   ResourceList `json:"limits"`
}

// ResourceList is one side of Resources. On the wire it is a flat map like
// a Kubernetes resource list: cpu and memory plus any extended resource
// (nvidia.com/gpu, squat.ai/uinput …), which land in Extended.
type ResourceList struct {
	CPU      string
	Memory   string
	Extended map[string]string
}

// MarshalJSON writes the flat map form.
func (r ResourceList) MarshalJSON() ([]byte, error) {
	m := make(map[string]string, len(r.Extended)+2)
	for k, v := range r.Extended {
		m[k] = v
	}
	if r.CPU != "" {
		m["cpu"] = r.CPU
	}
	if r.Memory != "" {
		m["memory"] = r.Memory
	}
	return json.Marshal(m)
}

// UnmarshalJSON reads the flat map form; numbers (a chart value such as
// `cpu: 2`) are accepted and kept as written.
func (r *ResourceList) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return err
	}
	*r = ResourceList{}
	for k, v := range m {
		var s string
		switch t := v.(type) {
		case string:
			s = t
		case json.Number:
			s = t.String()
		case nil:
			continue
		default:
			return fmt.Errorf("resource %s: expected a quantity string, got %T", k, v)
		}
		switch k {
		case "cpu":
			r.CPU = s
		case "memory":
			r.Memory = s
		default:
			if r.Extended == nil {
				r.Extended = map[string]string{}
			}
			r.Extended[k] = s
		}
	}
	return nil
}

// Toleration mirrors corev1.Toleration without importing it.
type Toleration struct {
	Key               string `json:"key,omitempty"`
	Operator          string `json:"operator,omitempty"`
	Value             string `json:"value,omitempty"`
	Effect            string `json:"effect,omitempty"`
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
}

// Config is the fully parsed hub configuration.
type Config struct {
	ListenAddr  string
	PublicURL   *url.URL
	DatabaseURL string
	Namespace   string
	// Kubeconfig is only used outside the cluster (make dev).
	Kubeconfig string

	// Moonlight is the GameStream-compatible front door.
	MoonlightHTTPPort  int
	MoonlightHTTPSPort int
	// MoonlightHostname is what Moonlight clients see in their host list.
	MoonlightHostname string
	// LBSharingKey lets the per-app stream Services share the Moonlight
	// Service's LoadBalancer IP (MetalLB allow-shared-ip / Cilium sharing-key).
	LBSharingKey string
	// LBIP pins every LoadBalancer Service to one address; empty lets the
	// LoadBalancer pick (the shared key then still keeps them together).
	LBIP string
	// StreamPortBase and MaxConcurrent define the per-app port sets:
	// app slot i uses StreamPortBase+10*i .. +3 (RTSP, control, video, audio).
	StreamPortBase int
	MaxConcurrent  int
	// CertDir holds the Moonlight server certificate (key.pem/cert.pem). It
	// must persist: Moonlight clients pin it when they pair.
	CertDir string

	WolfImage       string
	InitImage       string
	BridgeImage     string
	BridgeImageTag  string
	ImagePullPolicy string
	RuntimeClass    string
	// WolfGPURequest also asks for one nvidia.com/gpu for the wolf container
	// (needed when the device plugin does not honour NVIDIA_VISIBLE_DEVICES).
	WolfGPURequest bool
	// UinputResource is the extended resource that hands /dev/uinput to the
	// app container (generic-device-plugin); empty = hostPath /dev/uinput.
	UinputResource string
	// RenderNode pins Wolf to one DRI render node (empty = Wolf picks).
	RenderNode string
	TimeZone   string

	DefaultStorageClass string
	DefaultPVCSize      string
	DefaultResources    Resources
	// MaxResources caps per-app resources; empty sides fall back to the
	// defaults (which then double as the cap).
	MaxResources Resources
	NodeSelector map[string]string
	Tolerations  []Toleration
	ExtraEnv     map[string]string

	// BasePath is the path of PublicURL without a trailing slash ("" at
	// the root). The UI and API live under it, so the root of the host can
	// belong to moonlight-web.
	BasePath string
	// BrowserUpstream is the in-cluster URL of the embedded moonlight-web
	// (the games-operator fork). When set the hub reverse-proxies BrowserPath
	// to it, so the browser client shares the hub's host name, Ingress, TLS
	// and tunnel, and the hub's login decides who may use it.
	BrowserUpstream *url.URL
	// BrowserPath is the prefix the embedded moonlight-web is served under
	// ("/play"); no trailing slash.
	BrowserPath string
	// BrowserSecret is what the proxy puts in X-MW-Embedded so moonlight-web
	// trusts the forwarded requests (its MW_EMBEDDED_SECRET).
	BrowserSecret string
	// BrowserURL is the public URL of the moonlight-web instance that
	// streams into the browser; empty hides "Play in browser". Derived from
	// PublicURL and BrowserPath when BrowserUpstream is set, otherwise
	// BROWSER_URL (a moonlight-web run elsewhere).
	BrowserURL string

	CookieSecret      []byte
	AdminUsername     string
	AdminPasswordHash string
	// AdminPassword is a plaintext alternative to AdminPasswordHash, hashed
	// at startup. The chart uses it for the generated password.
	AdminPassword string
	// DefaultMaxApps caps the apps (instances) a user may have; 0 means
	// unlimited. An admin can override it per user.
	DefaultMaxApps int
	// DefaultMaxStorage caps the sum of a user's home volumes (a quantity
	// such as 500Gi); empty means unlimited.
	DefaultMaxStorage string
	// IdleStopAfter stops a running app this long after its last active
	// stream ended (0 = never).
	IdleStopAfter time.Duration
	AllowedHosts  []string
	LogLevel      string

	ReconcileInterval time.Duration
	StatusPollEvery   time.Duration
}

// BridgeImageRef returns image:tag.
func (c *Config) BridgeImageRef() string { return c.BridgeImage + ":" + c.BridgeImageTag }

// PublicHost is the host[:port] part of PublicURL.
func (c *Config) PublicHost() string { return c.PublicURL.Host }

// Secure reports whether cookies must carry the Secure flag.
func (c *Config) Secure() bool { return c.PublicURL.Scheme == "https" }

type lookup func(string) (string, bool)

// Load reads the configuration from the environment.
func Load() (*Config, error) {
	return load(os.LookupEnv)
}

func load(get lookup) (*Config, error) {
	var errs []error
	str := func(key, def string) string {
		if v, ok := get(Prefix + key); ok && v != "" {
			return v
		}
		return def
	}
	required := func(key string) string {
		v := str(key, "")
		if v == "" {
			errs = append(errs, fmt.Errorf("%s%s is required", Prefix, key))
		}
		return v
	}
	dur := func(key string, def time.Duration) time.Duration {
		v := str(key, "")
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s%s: %w", Prefix, key, err))
		}
		return d
	}
	integer := func(key string, def int) int {
		v := str(key, "")
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s%s: %w", Prefix, key, err))
		}
		return n
	}
	boolean := func(key string, def bool) bool {
		v := str(key, "")
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s%s: %w", Prefix, key, err))
		}
		return b
	}
	jsonInto := func(key string, dst any) {
		v := str(key, "")
		if v == "" {
			return
		}
		if err := json.Unmarshal([]byte(v), dst); err != nil {
			errs = append(errs, fmt.Errorf("%s%s: invalid JSON: %w", Prefix, key, err))
		}
	}

	c := &Config{
		ListenAddr:          str("LISTEN_ADDR", ":8080"),
		DatabaseURL:         required("DATABASE_URL"),
		Kubeconfig:          str("KUBECONFIG", ""),
		MoonlightHTTPPort:   integer("MOONLIGHT_HTTP_PORT", 47989),
		MoonlightHTTPSPort:  integer("MOONLIGHT_HTTPS_PORT", 47984),
		MoonlightHostname:   str("MOONLIGHT_HOSTNAME", "games-operator"),
		LBSharingKey:        str("LB_SHARING_KEY", "games-operator"),
		LBIP:                str("LB_IP", ""),
		StreamPortBase:      integer("STREAM_PORT_BASE", 48100),
		MaxConcurrent:       integer("MAX_CONCURRENT", 10),
		CertDir:             str("CERT_DIR", "/data/certs"),
		WolfImage:           str("WOLF_IMAGE", "ghcr.io/games-on-whales/wolf:stable"),
		InitImage:           str("INIT_IMAGE", "ghcr.io/games-on-whales/base:edge"),
		BridgeImage:         str("BRIDGE_IMAGE", "ghcr.io/dseif0x/games-operator-bridge"),
		BridgeImageTag:      str("BRIDGE_IMAGE_TAG", "latest"),
		ImagePullPolicy:     str("IMAGE_PULL_POLICY", "IfNotPresent"),
		RuntimeClass:        str("RUNTIME_CLASS", ""),
		WolfGPURequest:      boolean("WOLF_GPU_REQUEST", false),
		UinputResource:      str("UINPUT_RESOURCE", ""),
		RenderNode:          str("RENDER_NODE", ""),
		TimeZone:            str("TIMEZONE", "Etc/UTC"),
		DefaultStorageClass: str("DEFAULT_STORAGE_CLASS", ""),
		DefaultPVCSize:      str("DEFAULT_PVC_SIZE", "50Gi"),
		BrowserURL:          str("BROWSER_URL", ""),
		AdminUsername:       str("ADMIN_USERNAME", "admin"),
		AdminPasswordHash:   str("ADMIN_PASSWORD_HASH", ""),
		AdminPassword:       str("ADMIN_PASSWORD", ""),
		IdleStopAfter:       dur("IDLE_STOP_AFTER", 15*time.Minute),
		DefaultMaxApps:      integer("DEFAULT_MAX_APPS", 0),
		DefaultMaxStorage:   str("DEFAULT_MAX_STORAGE", ""),
		LogLevel:            str("LOG_LEVEL", "info"),
		ReconcileInterval:   dur("RECONCILE_INTERVAL", 30*time.Second),
		StatusPollEvery:     dur("STATUS_POLL_INTERVAL", 10*time.Second),
		DefaultResources: Resources{
			Requests: ResourceList{CPU: str("DEFAULT_CPU_REQUEST", "2"), Memory: str("DEFAULT_MEMORY_REQUEST", "4Gi")},
			Limits:   ResourceList{CPU: str("DEFAULT_CPU_LIMIT", "8"), Memory: str("DEFAULT_MEMORY_LIMIT", "16Gi"), Extended: map[string]string{"nvidia.com/gpu": "1"}},
		},
	}
	jsonInto("DEFAULT_RESOURCES", &c.DefaultResources)
	jsonInto("MAX_RESOURCES", &c.MaxResources)
	jsonInto("NODE_SELECTOR", &c.NodeSelector)
	jsonInto("TOLERATIONS", &c.Tolerations)
	jsonInto("EXTRA_ENV", &c.ExtraEnv)

	if raw := required("PUBLIC_URL"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, fmt.Errorf("%sPUBLIC_URL must be an absolute http(s) URL", Prefix))
		} else {
			c.PublicURL = u
			c.BasePath = strings.TrimSuffix(path.Clean("/"+u.Path), "/")
			if c.BasePath == "/" {
				c.BasePath = ""
			}
		}
	}
	c.BrowserPath = strings.TrimSuffix(path.Clean("/"+str("BROWSER_PATH", "/play")), "/")
	c.BrowserSecret = str("BROWSER_SECRET", "")
	if raw := str("BROWSER_UPSTREAM", ""); raw != "" {
		u, err := url.Parse(raw)
		switch {
		case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
			errs = append(errs, fmt.Errorf("%sBROWSER_UPSTREAM must be an absolute http(s) URL", Prefix))
		case c.BrowserPath == "" || c.BrowserPath == c.BasePath:
			errs = append(errs, fmt.Errorf("%sBROWSER_PATH must be a path of its own (not the root, not PUBLIC_URL's path)", Prefix))
		case c.BrowserSecret == "":
			errs = append(errs, fmt.Errorf("%sBROWSER_SECRET is required with BROWSER_UPSTREAM (moonlight-web's MW_EMBEDDED_SECRET)", Prefix))
		default:
			c.BrowserUpstream = u
			if c.PublicURL != nil {
				c.BrowserURL = c.PublicURL.Scheme + "://" + c.PublicURL.Host + c.BrowserPath + "/"
			}
		}
	}
	if raw := required("COOKIE_SECRET"); raw != "" {
		b, err := hex.DecodeString(raw)
		if err != nil || len(b) < 32 {
			errs = append(errs, fmt.Errorf("%sCOOKIE_SECRET must be at least 32 bytes hex", Prefix))
		}
		c.CookieSecret = b
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > 100 {
		errs = append(errs, fmt.Errorf("%sMAX_CONCURRENT must be between 1 and 100", Prefix))
	}
	if c.StreamPortBase < 1024 || c.StreamPortBase+10*c.MaxConcurrent > 65535 {
		errs = append(errs, fmt.Errorf("%sSTREAM_PORT_BASE leaves no room for %d port sets", Prefix, c.MaxConcurrent))
	}

	c.Namespace = str("NAMESPACE", "")
	if c.Namespace == "" {
		if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			c.Namespace = strings.TrimSpace(string(b))
		}
	}
	if c.Namespace == "" {
		c.Namespace = "default"
	}

	if v := str("ALLOWED_HOSTS", ""); v != "" {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				c.AllowedHosts = append(c.AllowedHosts, strings.ToLower(h))
			}
		}
	}
	if c.PublicURL != nil {
		c.AllowedHosts = appendUnique(c.AllowedHosts, strings.ToLower(c.PublicURL.Host))
		if host := c.PublicURL.Hostname(); host != c.PublicURL.Host {
			c.AllowedHosts = appendUnique(c.AllowedHosts, strings.ToLower(host))
		}
	}
	if _, _, err := splitAddr(c.ListenAddr); err != nil {
		errs = append(errs, fmt.Errorf("%sLISTEN_ADDR: %w", Prefix, err))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func splitAddr(addr string) (string, int, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", 0, errors.New("missing port")
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("bad port: %w", err)
	}
	return addr[:i], port, nil
}
