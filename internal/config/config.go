// Package config owns the YAML config schema, hot-reload, and the derived
// filesystem paths (the config file itself, and the certs directory served
// at /certs) that the rest of the daemon is built around.
package config

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// EtcConfigPath is the default, well-known config location used when
// nothing else is specified — see ResolveConfigPath.
const EtcConfigPath = "/etc/dynamic-pac-proxy/config.yaml"

// Duration parses as a Go duration string ("15s") in YAML instead of
// yaml.v3's default nanosecond integer, matching what a human editing the
// file expects.
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

var hostNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// domainPattern matches a dotted FQDN like "netflix.mydomain.com" — looser
// than hostNamePattern (which forbids dots) since a site's serve.domain is
// a real public hostname, not an internal identifier used in URL paths.
var domainPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)+$`)

// emailPattern is a deliberately loose "looks like an email" check — Let's
// Encrypt itself barely validates this field (it's only used for expiry/
// account notices), so a full RFC 5322 validator would be disproportionate.
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// HostConfig is one entry in the hosts list. The timeout/interval fields are
// pointers so we can tell "not set, inherit the global default" apart from
// an explicit (if silly) zero value.
type HostConfig struct {
	Name string `yaml:"name"`
	// Exactly one of HostName or HostIP must be set — see ValidateConfig.
	// HostIP skips mDNS resolution entirely, for hosts that already have
	// a fixed address (e.g. a mitmproxy instance at a static LAN IP) or
	// that don't answer mDNS at all.
	HostName        string    `yaml:"host_name,omitempty"`
	HostIP          string    `yaml:"host_ip,omitempty"`
	HostPort        int       `yaml:"host_port"`
	ServerPort      int       `yaml:"server_port"`
	RefreshInterval *Duration `yaml:"refresh_interval,omitempty"`
	MDNSTimeout     *Duration `yaml:"mdns_timeout,omitempty"`
	DialTimeout     *Duration `yaml:"dial_timeout,omitempty"`
	FailureCooldown *Duration `yaml:"failure_cooldown,omitempty"`
	// InterceptSSL, when true, terminates HTTPS for this host at this box
	// instead of tunneling opaque bytes: it presents a certificate signed
	// by this proxy's own local CA (see internal/mitm) to the client,
	// decrypts the request, then re-encrypts before forwarding onward
	// (chained through the upstream if reachable, or straight to the real
	// destination otherwise) — see the "SSL interception" README section.
	InterceptSSL bool `yaml:"intercept_ssl,omitempty"`
}

// Target returns whichever of HostName or HostIP is set — the address
// this host is actually resolved/dialed at, for display purposes (logs,
// /status). ValidateConfig guarantees exactly one of them is set.
func (h HostConfig) Target() string {
	if h.HostIP != "" {
		return h.HostIP
	}
	return h.HostName
}

// EffectiveHost is a HostConfig with all the global defaults resolved in.
type EffectiveHost struct {
	Name            string
	HostName        string
	HostIP          string
	HostPort        int
	ServerPort      int
	RefreshInterval time.Duration
	MDNSTimeout     time.Duration
	DialTimeout     time.Duration
	FailureCooldown time.Duration
}

// SiteConfig is one entry under the top-level sites: list — a named addon
// bundle. Today the only way a site does anything is via Serve (a
// standalone public reverse-proxy mirror of a domain the user owns); a
// site with neither Serve set nor (in a later iteration) a host linking it
// has no effect at all. Addons is intentionally the only thing shared with
// the future intercept-based use of sites, so it lives on SiteConfig
// itself rather than under Serve.
type SiteConfig struct {
	Name string `yaml:"name"`

	// Addons run in order for requests and reverse order for responses
	// (mitmproxy convention), relative to Store.AddonsDir. A missing/
	// broken script file is a runtime concern (internal/addon), not a
	// config-load-time one — see ValidateConfig's own comment on this.
	Addons []string `yaml:"addons,omitempty"`

	// Serve, when set, makes this site a standalone publicly-reachable
	// HTTPS reverse-proxy server fronted by a real ACME certificate — see
	// internal/edge. Unset means this site currently has no effect.
	Serve *SiteServe `yaml:"serve,omitempty"`
}

// SiteServe configures a site as its own HTTPS server: a real ACME
// certificate for Domain (obtained via DNS-01, since a public CA can only
// issue for a domain the operator actually controls), reverse-proxied to
// Backend. ListenAddr is where this daemon itself binds — the operator is
// expected to already have their own reverse proxy in front doing TCP/SNI
// passthrough for Domain to here, since terminating TLS anywhere else
// would defeat the point of this daemon holding its own ACME certificate.
type SiteServe struct {
	Domain     string `yaml:"domain"`
	ListenAddr string `yaml:"listen_addr"`
	Backend    string `yaml:"backend"`
	ACMEEmail  string `yaml:"acme_email"`

	// DNSProvider selects which DNSXxxConfig field below is used to solve
	// the ACME DNS-01 challenge. It's a string switch key, not a nested
	// struct-per-provider registry, on purpose — only Cloudflare exists
	// today, and this keeps adding a second provider a small, additive
	// change (one new struct, one new field, one new switch case) instead
	// of building a generic plugin system nobody's asked for yet.
	DNSProvider string               `yaml:"dns_provider"`
	Cloudflare  *CloudflareDNSConfig `yaml:"cloudflare,omitempty"`
}

// CloudflareDNSConfig names the environment variable holding a scoped
// Cloudflare API Token (Zone:DNS:Edit) — never the token itself, since
// config.yaml is hot-reloaded and has no precedent anywhere in this repo
// for holding a secret (unlike, say, internal/mitm's CA key, which is
// deliberately kept out of anything served but still lives on disk next
// to config.yaml — a credential is worse to leave sitting in a config
// file that already gets read/logged/reloaded routinely).
type CloudflareDNSConfig struct {
	APITokenEnv string `yaml:"api_token_env"`
}

type FileConfig struct {
	ListenAddr      string       `yaml:"listen_addr"`
	AdvertiseHost   string       `yaml:"advertise_host"`
	RefreshInterval Duration     `yaml:"refresh_interval"`
	MDNSTimeout     Duration     `yaml:"mdns_timeout"`
	DialTimeout     Duration     `yaml:"dial_timeout"`
	FailureCooldown Duration     `yaml:"failure_cooldown"`
	CertsDir        string       `yaml:"certs_dir,omitempty"`
	AddonsDir       string       `yaml:"addons_dir,omitempty"`
	Hosts           []HostConfig `yaml:"hosts"`
	Sites           []SiteConfig `yaml:"sites,omitempty"`
}

func (c FileConfig) Effective(h HostConfig) EffectiveHost {
	ri, mt, dt, fc := c.RefreshInterval, c.MDNSTimeout, c.DialTimeout, c.FailureCooldown
	if h.RefreshInterval != nil {
		ri = *h.RefreshInterval
	}
	if h.MDNSTimeout != nil {
		mt = *h.MDNSTimeout
	}
	if h.DialTimeout != nil {
		dt = *h.DialTimeout
	}
	if h.FailureCooldown != nil {
		fc = *h.FailureCooldown
	}
	return EffectiveHost{
		Name:            h.Name,
		HostName:        h.HostName,
		HostIP:          h.HostIP,
		HostPort:        h.HostPort,
		ServerPort:      h.ServerPort,
		RefreshInterval: ri.Duration(),
		MDNSTimeout:     mt.Duration(),
		DialTimeout:     dt.Duration(),
		FailureCooldown: fc.Duration(),
	}
}

func (c FileConfig) FindHost(name string) (HostConfig, bool) {
	for _, h := range c.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return HostConfig{}, false
}

func (c FileConfig) FindSite(name string) (SiteConfig, bool) {
	for _, s := range c.Sites {
		if s.Name == name {
			return s, true
		}
	}
	return SiteConfig{}, false
}

func DefaultConfig() FileConfig {
	return FileConfig{
		ListenAddr:      ":8080",
		AdvertiseHost:   detectLANAddress(),
		RefreshInterval: Duration(15 * time.Second),
		MDNSTimeout:     Duration(2 * time.Second),
		DialTimeout:     Duration(1 * time.Second),
		FailureCooldown: Duration(5 * time.Second),
		Hosts: []HostConfig{
			{Name: "dereks-macbook", HostName: "dereks-MacBook-Pro.local", HostPort: 8888, ServerPort: 8081},
		},
	}
}

// detectLANAddress makes a best-effort guess at this box's own LAN address,
// used as a convenience default for advertise_host so PAC files work out of
// the box. It's only ever a fallback — set advertise_host explicitly on a
// multi-homed box or if this guess doesn't match what client devices can
// actually reach.
func detectLANAddress() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipNet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return ""
}

// PortFromAddr extracts the numeric port from a "host:port" listen address
// — e.g. turning listen_addr into the port PAC URLs should point at.
func PortFromAddr(addr string) (int, bool) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return 0, false
	}
	return port, true
}

func ValidateConfig(cfg FileConfig) error {
	if len(cfg.Hosts) == 0 {
		return fmt.Errorf("hosts: at least one host must be configured")
	}

	listenAddrPort, _ := PortFromAddr(cfg.ListenAddr)

	// seenPorts is shared across hosts[].server_port and sites[].serve's
	// listen_addr port, below — a site's serve listener and a host's
	// forward-proxy listener are both real TCP binds on this same
	// process, so they must not collide with each other any more than two
	// hosts may collide with each other.
	seenNames := make(map[string]bool, len(cfg.Hosts))
	seenPorts := make(map[int]string, len(cfg.Hosts))
	for i, h := range cfg.Hosts {
		if h.Name == "" {
			return fmt.Errorf("hosts[%d]: name is required", i)
		}
		if !hostNamePattern.MatchString(h.Name) {
			return fmt.Errorf("hosts[%d]: name %q must match %s (it's used in URL paths)", i, h.Name, hostNamePattern.String())
		}
		if seenNames[h.Name] {
			return fmt.Errorf("hosts[%d]: duplicate name %q", i, h.Name)
		}
		seenNames[h.Name] = true
		if h.HostName == "" && h.HostIP == "" {
			return fmt.Errorf("hosts[%d] (%s): either host_name or host_ip is required", i, h.Name)
		}
		if h.HostName != "" && h.HostIP != "" {
			return fmt.Errorf("hosts[%d] (%s): host_name and host_ip are mutually exclusive — set only one", i, h.Name)
		}
		if h.HostIP != "" && net.ParseIP(h.HostIP) == nil {
			return fmt.Errorf("hosts[%d] (%s): host_ip %q is not a valid IP address", i, h.Name, h.HostIP)
		}
		if h.HostPort < 1 || h.HostPort > 65535 {
			return fmt.Errorf("hosts[%d] (%s): host_port must be between 1 and 65535, got %d", i, h.Name, h.HostPort)
		}
		if h.ServerPort < 1 || h.ServerPort > 65535 {
			return fmt.Errorf("hosts[%d] (%s): server_port must be between 1 and 65535, got %d", i, h.Name, h.ServerPort)
		}
		if h.ServerPort == listenAddrPort {
			return fmt.Errorf("hosts[%d] (%s): server_port %d collides with listen_addr's port", i, h.Name, h.ServerPort)
		}
		if other, ok := seenPorts[h.ServerPort]; ok {
			return fmt.Errorf("hosts[%d] (%s): server_port %d is already used by host %q", i, h.Name, h.ServerPort, other)
		}
		seenPorts[h.ServerPort] = h.Name
	}

	seenSiteNames := make(map[string]bool, len(cfg.Sites))
	seenServeDomains := make(map[string]bool, len(cfg.Sites))
	for i, s := range cfg.Sites {
		if s.Name == "" {
			return fmt.Errorf("sites[%d]: name is required", i)
		}
		if !hostNamePattern.MatchString(s.Name) {
			return fmt.Errorf("sites[%d]: name %q must match %s", i, s.Name, hostNamePattern.String())
		}
		if seenSiteNames[s.Name] {
			return fmt.Errorf("sites[%d]: duplicate name %q", i, s.Name)
		}
		seenSiteNames[s.Name] = true

		// Whether the addon script files named here actually exist or
		// compile is deliberately NOT checked here — see internal/addon.
		// Scripts are edited far more often than sites/hosts structure,
		// and a broken .star edit must never block the whole daemon's
		// config from loading, unlike a bad port number.

		if s.Serve == nil {
			continue
		}
		serve := s.Serve
		if serve.Domain == "" {
			return fmt.Errorf("sites[%d] (%s): serve.domain is required", i, s.Name)
		}
		if !domainPattern.MatchString(serve.Domain) {
			return fmt.Errorf("sites[%d] (%s): serve.domain %q does not look like a valid hostname", i, s.Name, serve.Domain)
		}
		if seenServeDomains[serve.Domain] {
			return fmt.Errorf("sites[%d] (%s): serve.domain %q is already used by another site", i, s.Name, serve.Domain)
		}
		seenServeDomains[serve.Domain] = true

		if serve.ListenAddr == "" {
			return fmt.Errorf("sites[%d] (%s): serve.listen_addr is required", i, s.Name)
		}
		servePort, ok := PortFromAddr(serve.ListenAddr)
		if !ok {
			return fmt.Errorf("sites[%d] (%s): serve.listen_addr %q is not a valid host:port address", i, s.Name, serve.ListenAddr)
		}
		if servePort < 1 || servePort > 65535 {
			return fmt.Errorf("sites[%d] (%s): serve.listen_addr port must be between 1 and 65535, got %d", i, s.Name, servePort)
		}
		if servePort == listenAddrPort {
			return fmt.Errorf("sites[%d] (%s): serve.listen_addr port %d collides with listen_addr's port", i, s.Name, servePort)
		}
		if other, ok := seenPorts[servePort]; ok {
			return fmt.Errorf("sites[%d] (%s): serve.listen_addr port %d is already used by %q", i, s.Name, servePort, other)
		}
		seenPorts[servePort] = "site:" + s.Name

		backendURL, err := url.Parse(serve.Backend)
		if serve.Backend == "" || err != nil || !backendURL.IsAbs() || (backendURL.Scheme != "http" && backendURL.Scheme != "https") {
			return fmt.Errorf("sites[%d] (%s): serve.backend must be an absolute http(s) URL, got %q", i, s.Name, serve.Backend)
		}

		if serve.ACMEEmail == "" {
			return fmt.Errorf("sites[%d] (%s): serve.acme_email is required", i, s.Name)
		}
		if !emailPattern.MatchString(serve.ACMEEmail) {
			return fmt.Errorf("sites[%d] (%s): serve.acme_email %q does not look like a valid email address", i, s.Name, serve.ACMEEmail)
		}

		switch serve.DNSProvider {
		case "cloudflare":
			if serve.Cloudflare == nil || serve.Cloudflare.APITokenEnv == "" {
				return fmt.Errorf("sites[%d] (%s): serve.cloudflare.api_token_env is required when dns_provider is \"cloudflare\"", i, s.Name)
			}
			if os.Getenv(serve.Cloudflare.APITokenEnv) == "" {
				return fmt.Errorf("sites[%d] (%s): environment variable %q (serve.cloudflare.api_token_env) is not set", i, s.Name, serve.Cloudflare.APITokenEnv)
			}
		case "":
			return fmt.Errorf("sites[%d] (%s): serve.dns_provider is required (supported: \"cloudflare\")", i, s.Name)
		default:
			return fmt.Errorf("sites[%d] (%s): serve.dns_provider %q is not supported (supported: \"cloudflare\")", i, s.Name, serve.DNSProvider)
		}
	}
	return nil
}

// ParseConfig overlays YAML onto the defaults, so a config file only needs
// to mention the fields it wants to override — except hosts, which fully
// replaces the default host list as soon as the file specifies any.
func ParseConfig(data []byte) (FileConfig, error) {
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, err
	}
	if err := ValidateConfig(cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// Store holds the current config plus the mtime it was loaded from, and is
// safe for concurrent reads from the HTTP handlers and refresh loops while
// a background goroutine polls for edits.
type Store struct {
	path string

	mu      sync.RWMutex
	cfg     FileConfig
	modTime time.Time
}

// NewStore builds a Store directly from an already-parsed config, without
// reading a file — useful for embedding this package as a library, or for
// tests that want to control the config precisely. path only affects
// derived locations (see CertsDir); it need not exist on disk.
func NewStore(cfg FileConfig, path string) *Store {
	return &Store{cfg: cfg, path: path}
}

// LoadInitial loads the config file at startup. A missing file falls back
// to built-in defaults (so the service is usable with no setup); a present
// but invalid file fails fast rather than starting up misconfigured.
func LoadInitial(path string) (*Store, error) {
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		log.Printf("no config file at %s, using built-in defaults", path)
		s.cfg = DefaultConfig()
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg, err := ParseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	s.cfg = cfg
	if info, statErr := os.Stat(path); statErr == nil {
		s.modTime = info.ModTime()
	}
	return s, nil
}

// ReloadIfChanged re-reads the config file if its mtime moved since the
// last successful load. A parse/validation error is logged and the
// previous known-good config is kept, so a mid-edit typo can't take the
// service down.
func (s *Store) ReloadIfChanged() {
	info, err := os.Stat(s.path)
	if err != nil {
		return
	}

	s.mu.RLock()
	unchanged := info.ModTime().Equal(s.modTime)
	s.mu.RUnlock()
	if unchanged {
		return
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		log.Printf("config reload: read %s: %v", s.path, err)
		return
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		log.Printf("config reload: %s is invalid, keeping previous config: %v", s.path, err)
		return
	}

	s.mu.Lock()
	s.cfg = cfg
	s.modTime = info.ModTime()
	s.mu.Unlock()

	names := make([]string, len(cfg.Hosts))
	for i, h := range cfg.Hosts {
		names[i] = h.Name
	}
	log.Printf("config reloaded from %s: hosts=%v", s.path, names)
}

func (s *Store) Snapshot() FileConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// ResolveConfigPath picks the config file location: an explicit --config
// flag wins outright; otherwise /etc/dynamic-pac-proxy/config.yaml if it
// exists, then config.yaml next to the binary if that exists, falling back
// to the /etc path (which LoadInitial will treat as "use defaults" if
// nothing is actually there).
func ResolveConfigPath(flagValue string) (path string, source string) {
	if flagValue != "" {
		return flagValue, "--config flag"
	}
	if _, err := os.Stat(EtcConfigPath); err == nil {
		return EtcConfigPath, "default location"
	}
	if exe, err := os.Executable(); err == nil {
		besideBinary := filepath.Join(filepath.Dir(exe), "config.yaml")
		if _, err := os.Stat(besideBinary); err == nil {
			return besideBinary, "next to binary"
		}
	}
	return EtcConfigPath, "none found, falling back to default location"
}

// ResolveCertsDir picks the directory served at /certs. An explicit
// certs_dir in config.yaml wins outright (resolved relative to the config
// file's own directory if it isn't already absolute); otherwise it defaults
// to a "certs" directory next to the config file, mirroring how the config
// file and binary already live side by side in deployment.
func ResolveCertsDir(cfg FileConfig, configPath string) string {
	base := filepath.Dir(configPath)
	if cfg.CertsDir == "" {
		return filepath.Join(base, "certs")
	}
	if filepath.IsAbs(cfg.CertsDir) {
		return cfg.CertsDir
	}
	return filepath.Join(base, cfg.CertsDir)
}

// CertsDir returns the currently-configured certs directory, re-resolved
// against whatever config is live right now so a hot-reloaded certs_dir
// takes effect on the next request without a restart.
func (s *Store) CertsDir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return ResolveCertsDir(s.cfg, s.path)
}

// ConfigDir returns the directory containing the config file — used to
// place files that must live *next to* config.yaml but, unlike CertsDir,
// must never be served over HTTP (e.g. the local CA's private key; see
// internal/mitm).
func (s *Store) ConfigDir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return filepath.Dir(s.path)
}

// ResolveAddonsDir picks the directory addon script paths (sites[].addons)
// are resolved relative to — mirrors ResolveCertsDir exactly, defaulting
// to an "addons" directory next to the config file.
func ResolveAddonsDir(cfg FileConfig, configPath string) string {
	base := filepath.Dir(configPath)
	if cfg.AddonsDir == "" {
		return filepath.Join(base, "addons")
	}
	if filepath.IsAbs(cfg.AddonsDir) {
		return cfg.AddonsDir
	}
	return filepath.Join(base, cfg.AddonsDir)
}

// AddonsDir returns the currently-configured addons directory, re-resolved
// against whatever config is live right now so a hot-reloaded addons_dir
// takes effect on the next request without a restart.
func (s *Store) AddonsDir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return ResolveAddonsDir(s.cfg, s.path)
}
