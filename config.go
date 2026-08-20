package main

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// duration parses as a Go duration string ("15s") in YAML instead of yaml.v3's
// default nanosecond integer, matching what a human editing the file expects.
type duration time.Duration

func (d duration) Duration() time.Duration { return time.Duration(d) }

func (d duration) String() string { return time.Duration(d).String() }

func (d *duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = duration(parsed)
	return nil
}

var hostNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// hostConfig is one entry in the hosts list. The timeout/interval fields are
// pointers so we can tell "not set, inherit the global default" apart from
// an explicit (if silly) zero value.
type hostConfig struct {
	Name            string    `yaml:"name"`
	MDNSHostname    string    `yaml:"mdns_hostname"`
	Port            int       `yaml:"port"`
	RefreshInterval *duration `yaml:"refresh_interval,omitempty"`
	MDNSTimeout     *duration `yaml:"mdns_timeout,omitempty"`
	DialTimeout     *duration `yaml:"dial_timeout,omitempty"`
}

// effectiveHost is a hostConfig with all the global defaults resolved in.
type effectiveHost struct {
	Name            string
	MDNSHostname    string
	Port            int
	RefreshInterval time.Duration
	MDNSTimeout     time.Duration
	DialTimeout     time.Duration
}

type fileConfig struct {
	ListenAddr      string       `yaml:"listen_addr"`
	RefreshInterval duration     `yaml:"refresh_interval"`
	MDNSTimeout     duration     `yaml:"mdns_timeout"`
	DialTimeout     duration     `yaml:"dial_timeout"`
	Hosts           []hostConfig `yaml:"hosts"`
}

func (c fileConfig) effective(h hostConfig) effectiveHost {
	ri, mt, dt := c.RefreshInterval, c.MDNSTimeout, c.DialTimeout
	if h.RefreshInterval != nil {
		ri = *h.RefreshInterval
	}
	if h.MDNSTimeout != nil {
		mt = *h.MDNSTimeout
	}
	if h.DialTimeout != nil {
		dt = *h.DialTimeout
	}
	return effectiveHost{
		Name:            h.Name,
		MDNSHostname:    h.MDNSHostname,
		Port:            h.Port,
		RefreshInterval: ri.Duration(),
		MDNSTimeout:     mt.Duration(),
		DialTimeout:     dt.Duration(),
	}
}

func (c fileConfig) findHost(name string) (hostConfig, bool) {
	for _, h := range c.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return hostConfig{}, false
}

func defaultConfig() fileConfig {
	return fileConfig{
		ListenAddr:      ":8080",
		RefreshInterval: duration(15 * time.Second),
		MDNSTimeout:     duration(2 * time.Second),
		DialTimeout:     duration(1 * time.Second),
		Hosts: []hostConfig{
			{Name: "dereks-macbook", MDNSHostname: "dereks-MacBook-Pro.local", Port: 8888},
		},
	}
}

func validateConfig(cfg fileConfig) error {
	if len(cfg.Hosts) == 0 {
		return fmt.Errorf("hosts: at least one host must be configured")
	}
	seen := make(map[string]bool, len(cfg.Hosts))
	for i, h := range cfg.Hosts {
		if h.Name == "" {
			return fmt.Errorf("hosts[%d]: name is required", i)
		}
		if !hostNamePattern.MatchString(h.Name) {
			return fmt.Errorf("hosts[%d]: name %q must match %s (it's used in URL paths)", i, h.Name, hostNamePattern.String())
		}
		if seen[h.Name] {
			return fmt.Errorf("hosts[%d]: duplicate name %q", i, h.Name)
		}
		seen[h.Name] = true
		if h.MDNSHostname == "" {
			return fmt.Errorf("hosts[%d] (%s): mdns_hostname is required", i, h.Name)
		}
		if h.Port < 1 || h.Port > 65535 {
			return fmt.Errorf("hosts[%d] (%s): port must be between 1 and 65535, got %d", i, h.Name, h.Port)
		}
	}
	return nil
}

// parseConfig overlays YAML onto the defaults, so a config file only needs
// to mention the fields it wants to override — except hosts, which fully
// replaces the default host list as soon as the file specifies any.
func parseConfig(data []byte) (fileConfig, error) {
	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fileConfig{}, err
	}
	if err := validateConfig(cfg); err != nil {
		return fileConfig{}, err
	}
	return cfg, nil
}

// configStore holds the current config plus the mtime it was loaded from,
// and is safe for concurrent reads from the HTTP handlers and refresh loops
// while a background goroutine polls for edits.
type configStore struct {
	path string

	mu      sync.RWMutex
	cfg     fileConfig
	modTime time.Time
}

// loadInitial loads the config file at startup. A missing file falls back
// to built-in defaults (so the service is usable with no setup); a present
// but invalid file fails fast rather than starting up misconfigured.
func loadInitial(path string) (*configStore, error) {
	s := &configStore{path: path}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		log.Printf("no config file at %s, using built-in defaults", path)
		s.cfg = defaultConfig()
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg, err := parseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	s.cfg = cfg
	if info, statErr := os.Stat(path); statErr == nil {
		s.modTime = info.ModTime()
	}
	return s, nil
}

// reloadIfChanged re-reads the config file if its mtime moved since the
// last successful load. A parse/validation error is logged and the
// previous known-good config is kept, so a mid-edit typo can't take the
// service down.
func (s *configStore) reloadIfChanged() {
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
	cfg, err := parseConfig(data)
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

func (s *configStore) snapshot() fileConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}
