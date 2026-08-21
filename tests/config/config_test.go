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
	valid := config.HostConfig{Name: "host", Port: 8888, ListenPort: 8081}

	cases := []struct {
		name    string
		host    config.HostConfig
		wantErr bool
	}{
		{
			name: "mdns_hostname alone is valid",
			host: func() config.HostConfig { h := valid; h.MDNSHostname = "some-machine.local"; return h }(),
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
				h.MDNSHostname = "some-machine.local"
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

func TestHostConfigTarget(t *testing.T) {
	if got := (config.HostConfig{MDNSHostname: "some-machine.local"}).Target(); got != "some-machine.local" {
		t.Fatalf("Target() = %q, want mdns_hostname value", got)
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
