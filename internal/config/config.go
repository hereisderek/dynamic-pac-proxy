// Package config owns the YAML config schema, hot-reload, and the derived
// filesystem paths (the config file itself, and the certs directory served
// at /certs) that the rest of the daemon is built around.
package config

import (
	"fmt"
	"log"
	"net"
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

type FileConfig struct {
	ListenAddr      string       `yaml:"listen_addr"`
	AdvertiseHost   string       `yaml:"advertise_host"`
	RefreshInterval Duration     `yaml:"refresh_interval"`
	MDNSTimeout     Duration     `yaml:"mdns_timeout"`
	DialTimeout     Duration     `yaml:"dial_timeout"`
	FailureCooldown Duration     `yaml:"failure_cooldown"`
	CertsDir        string       `yaml:"certs_dir,omitempty"`
	Hosts           []HostConfig `yaml:"hosts"`
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
