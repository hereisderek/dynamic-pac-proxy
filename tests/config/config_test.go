package config_test

import (
	"testing"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
)

func baseFileConfig(h config.HostConfig) config.FileConfig {
	return config.FileConfig{
		ListenAddr:      ":8080",
		RefreshInterval: config.Duration(1),
		MDNSTimeout:     config.Duration(1),
		DialTimeout:     config.Duration(1),
		FailureCooldown: config.Duration(1),
		Hosts:           []config.HostConfig{h},
	}
}

func TestValidateConfigHostIdentity(t *testing.T) {
	valid := config.HostConfig{Name: "host", HostPort: 8888, ServerPort: 8081}

	cases := []struct {
		name    string
		host    config.HostConfig
		wantErr bool
	}{
		{
			name: "host_name alone is valid",
			host: func() config.HostConfig { h := valid; h.HostName = "some-machine.local"; return h }(),
		},
		{
			name: "host_ip alone is valid",
			host: func() config.HostConfig { h := valid; h.HostIP = "172.16.2.23"; return h }(),
		},
		{
			name:    "neither set is invalid",
			host:    valid,
			wantErr: true,
		},
		{
			name: "both set is invalid",
			host: func() config.HostConfig {
				h := valid
				h.HostName = "some-machine.local"
				h.HostIP = "172.16.2.23"
				return h
			}(),
			wantErr: true,
		},
		{
			name:    "invalid host_ip is rejected",
			host:    func() config.HostConfig { h := valid; h.HostIP = "not-an-ip"; return h }(),
			wantErr: true,
		},
		{
			name:    "host_ip accepts IPv6",
			host:    func() config.HostConfig { h := valid; h.HostIP = "::1"; return h }(),
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := config.ValidateConfig(baseFileConfig(tc.host))
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

func validServeSite(t *testing.T) config.SiteConfig {
	t.Helper()
	t.Setenv("TEST_CF_TOKEN", "some-token")
	return config.SiteConfig{
		Name: "netflix",
		Serve: &config.SiteServe{
			Domain:      "netflix.mydomain.com",
			ListenAddr:  ":8443",
			Backend:     "https://www.netflix.com",
			ACMEEmail:   "you@mydomain.com",
			DNSProvider: "cloudflare",
			Cloudflare:  &config.CloudflareDNSConfig{APITokenEnv: "TEST_CF_TOKEN"},
		},
	}
}

func TestValidateConfigSites(t *testing.T) {
	validHost := config.HostConfig{Name: "host", HostName: "some-machine.local", HostPort: 8888, ServerPort: 8081}

	cases := []struct {
		name    string
		mutate  func(s config.SiteConfig) config.SiteConfig
		wantErr bool
	}{
		{name: "valid site as-is"},
		{
			name:    "missing name",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Name = ""; return s },
			wantErr: true,
		},
		{
			name:    "name with invalid characters",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Name = "not valid!"; return s },
			wantErr: true,
		},
		{
			name:    "no serve block is valid (site currently inert)",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve = nil; return s },
			wantErr: false,
		},
		{
			name:    "missing serve.domain",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.Domain = ""; return s },
			wantErr: true,
		},
		{
			name:    "serve.domain without a dot is rejected",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.Domain = "netflix"; return s },
			wantErr: true,
		},
		{
			name:    "missing serve.listen_addr",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.ListenAddr = ""; return s },
			wantErr: true,
		},
		{
			name:    "serve.listen_addr without a port",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.ListenAddr = "example.com"; return s },
			wantErr: true,
		},
		{
			name:    "serve.listen_addr colliding with a host's server_port",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.ListenAddr = ":8081"; return s },
			wantErr: true,
		},
		{
			name:    "serve.listen_addr colliding with listen_addr's port",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.ListenAddr = ":8080"; return s },
			wantErr: true,
		},
		{
			name:    "missing serve.backend",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.Backend = ""; return s },
			wantErr: true,
		},
		{
			name:    "serve.backend without a scheme is rejected",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.Backend = "www.netflix.com"; return s },
			wantErr: true,
		},
		{
			name:    "missing serve.acme_email",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.ACMEEmail = ""; return s },
			wantErr: true,
		},
		{
			name:    "serve.acme_email that doesn't look like an email",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.ACMEEmail = "not-an-email"; return s },
			wantErr: true,
		},
		{
			name:    "missing serve.dns_provider",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.DNSProvider = ""; return s },
			wantErr: true,
		},
		{
			name:    "unknown serve.dns_provider",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.DNSProvider = "route53"; return s },
			wantErr: true,
		},
		{
			name:    "cloudflare provider missing cloudflare block",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.Cloudflare = nil; return s },
			wantErr: true,
		},
		{
			name:    "cloudflare provider missing api_token_env",
			mutate:  func(s config.SiteConfig) config.SiteConfig { s.Serve.Cloudflare.APITokenEnv = ""; return s },
			wantErr: true,
		},
		{
			name: "cloudflare api_token_env names an unset env var",
			mutate: func(s config.SiteConfig) config.SiteConfig {
				s.Serve.Cloudflare.APITokenEnv = "TEST_CF_TOKEN_DEFINITELY_UNSET"
				return s
			},
			wantErr: true,
		},
		{
			name: "cloudflare literal api_token is accepted in place of api_token_env",
			mutate: func(s config.SiteConfig) config.SiteConfig {
				s.Serve.Cloudflare = &config.CloudflareDNSConfig{APIToken: "a-real-token"}
				return s
			},
			wantErr: false,
		},
		{
			name: "cloudflare api_token and api_token_env are mutually exclusive",
			mutate: func(s config.SiteConfig) config.SiteConfig {
				s.Serve.Cloudflare.APIToken = "a-real-token"
				return s
			},
			wantErr: true,
		},
		{
			name: "acme_staging is accepted",
			mutate: func(s config.SiteConfig) config.SiteConfig {
				s.Serve.ACMEStaging = true
				return s
			},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			site := validServeSite(t)
			if tc.mutate != nil {
				site = tc.mutate(site)
			}
			cfg := baseFileConfig(validHost)
			cfg.Sites = []config.SiteConfig{site}
			err := config.ValidateConfig(cfg)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

func TestValidateConfigSiteNameUniqueness(t *testing.T) {
	validHost := config.HostConfig{Name: "host", HostName: "some-machine.local", HostPort: 8888, ServerPort: 8081}
	site := validServeSite(t)
	site2 := site
	site2.Serve = &config.SiteServe{
		Domain: "other.mydomain.com", ListenAddr: ":8444", Backend: "https://example.com",
		ACMEEmail: "you@mydomain.com", DNSProvider: "cloudflare",
		Cloudflare: &config.CloudflareDNSConfig{APITokenEnv: "TEST_CF_TOKEN"},
	}

	cfg := baseFileConfig(validHost)
	cfg.Sites = []config.SiteConfig{site, site2}
	if err := config.ValidateConfig(cfg); err == nil {
		t.Fatal("expected duplicate site name to be rejected")
	}

	site2.Name = "other-site"
	cfg.Sites = []config.SiteConfig{site, site2}
	if err := config.ValidateConfig(cfg); err != nil {
		t.Fatalf("expected distinct site names to be valid, got: %v", err)
	}

	// distinct names, same serve.domain, must still be rejected
	site2.Serve.Domain = site.Serve.Domain
	cfg.Sites = []config.SiteConfig{site, site2}
	if err := config.ValidateConfig(cfg); err == nil {
		t.Fatal("expected duplicate serve.domain to be rejected")
	}
}

func TestResolveAddonsDir(t *testing.T) {
	cases := []struct {
		name       string
		cfg        config.FileConfig
		configPath string
		want       string
	}{
		{
			name:       "default next to config file",
			cfg:        config.FileConfig{},
			configPath: "/opt/dynamic-pac-proxy/config.yaml",
			want:       "/opt/dynamic-pac-proxy/addons",
		},
		{
			name:       "relative override resolved against config dir",
			cfg:        config.FileConfig{AddonsDir: "my-addons"},
			configPath: "/opt/dynamic-pac-proxy/config.yaml",
			want:       "/opt/dynamic-pac-proxy/my-addons",
		},
		{
			name:       "absolute override used as-is",
			cfg:        config.FileConfig{AddonsDir: "/srv/addons"},
			configPath: "/opt/dynamic-pac-proxy/config.yaml",
			want:       "/srv/addons",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := config.ResolveAddonsDir(tc.cfg, tc.configPath); got != tc.want {
				t.Fatalf("ResolveAddonsDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHostConfigTarget(t *testing.T) {
	if got := (config.HostConfig{HostName: "some-machine.local"}).Target(); got != "some-machine.local" {
		t.Fatalf("Target() = %q, want host_name value", got)
	}
	if got := (config.HostConfig{HostIP: "172.16.2.23"}).Target(); got != "172.16.2.23" {
		t.Fatalf("Target() = %q, want host_ip value", got)
	}
}

func TestResolveCertsDir(t *testing.T) {
	cases := []struct {
		name       string
		cfg        config.FileConfig
		configPath string
		want       string
	}{
		{
			name:       "default next to config file",
			cfg:        config.FileConfig{},
			configPath: "/opt/dynamic-pac-proxy/config.yaml",
			want:       "/opt/dynamic-pac-proxy/certs",
		},
		{
			name:       "relative override resolved against config dir",
			cfg:        config.FileConfig{CertsDir: "my-certs"},
			configPath: "/opt/dynamic-pac-proxy/config.yaml",
			want:       "/opt/dynamic-pac-proxy/my-certs",
		},
		{
			name:       "absolute override used as-is",
			cfg:        config.FileConfig{CertsDir: "/srv/certs"},
			configPath: "/opt/dynamic-pac-proxy/config.yaml",
			want:       "/srv/certs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := config.ResolveCertsDir(tc.cfg, tc.configPath); got != tc.want {
				t.Fatalf("ResolveCertsDir() = %q, want %q", got, tc.want)
			}
		})
	}
}
