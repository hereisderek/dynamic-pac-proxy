package edge

import (
	"fmt"
	"os"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
)

// buildDNSProvider constructs the libdns/certmagic DNS provider for one
// site's serve.dns_provider. Adding a second provider later is: one new
// case here, one new *_env credential field on config.SiteServe, and one
// new small libdns/* import — not a rewrite of this switch, matching how
// config.SiteServe.DNSProvider is a plain string switch key rather than a
// generic plugin registry nobody's asked for yet.
func buildDNSProvider(serve *config.SiteServe) (certmagic.DNSProvider, error) {
	switch serve.DNSProvider {
	case "cloudflare":
		token := os.Getenv(serve.Cloudflare.APITokenEnv)
		if token == "" {
			// config.ValidateConfig already checks this at load time; this
			// is a defensive backstop in case the env var was unset again
			// after config load but before this site actually started.
			return nil, fmt.Errorf("edge: environment variable %q is not set", serve.Cloudflare.APITokenEnv)
		}
		return &cloudflare.Provider{APIToken: token}, nil
	default:
		return nil, fmt.Errorf("edge: unsupported dns_provider %q", serve.DNSProvider)
	}
}
