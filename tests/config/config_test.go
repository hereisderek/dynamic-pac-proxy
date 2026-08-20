package config_test

import (
	"testing"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
)

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
