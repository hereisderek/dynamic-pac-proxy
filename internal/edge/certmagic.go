package edge

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"

	"github.com/caddyserver/certmagic"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
)

// certmagicSource manages real ACME certificates for every serve-enabled
// site in this process, via DNS-01 only (see buildDNSProvider) — no port
// 80/443 exposure is needed for issuance itself, only for actually serving
// the site afterward.
//
// It shares one on-disk cache directory and one certmagic.Cache across all
// sites, so certmagic's own background renewal maintenance (started once
// per Cache) covers every site, while giving each site its own
// certmagic.Config. That per-site Config is required, not just tidy:
// certmagic's cache doesn't keep a static pointer to "the" config for a
// certificate — it calls GetConfigForCert again at every maintenance pass
// (renewal can happen ~60-90 days after the site was first started, long
// after this process's initial in-memory state), specifically because a
// site's DNS provider/credentials need to be available again at that
// point. The per-domain map below is what makes that lookup return the
// right Config instead of a bare one with no DNS solver configured.
type certmagicSource struct {
	storage certmagic.Storage

	mu      sync.Mutex
	cache   *certmagic.Cache
	configs map[string]*certmagic.Config // by domain
}

// newCertmagicSource stores everything under cacheDir: certmagic's own
// generated ACME account private key alongside issued certs/keys,
// comingled by certmagic's own storage layout with no supported way to
// split them. Treat the whole directory like internal/mitm treats its CA
// private key — living next to config.yaml (Store.ConfigDir()), never
// under the served /certs directory.
func newCertmagicSource(cacheDir string) *certmagicSource {
	s := &certmagicSource{
		storage: &certmagic.FileStorage{Path: cacheDir},
		configs: make(map[string]*certmagic.Config),
	}
	s.cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(cert certmagic.Certificate) (*certmagic.Config, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, name := range cert.Names {
				if cfg, ok := s.configs[name]; ok {
					return cfg, nil
				}
			}
			return nil, fmt.Errorf("edge: no ACME config registered for certificate names %v", cert.Names)
		},
	})
	return s
}

// TLSConfig obtains (issuing or loading from storage as needed) a
// certificate for serve.Domain and returns a ready *tls.Config presenting
// it — blocks until issuance/renewal succeeds or ctx is done, per
// certmagic.Config.ManageSync's own documented behavior.
func (s *certmagicSource) TLSConfig(ctx context.Context, serve *config.SiteServe) (*tls.Config, error) {
	provider, err := buildDNSProvider(serve)
	if err != nil {
		return nil, err
	}

	ca := certmagic.LetsEncryptProductionCA
	if serve.ACMEStaging {
		ca = certmagic.LetsEncryptStagingCA
	}

	// certmagic's own documented pattern: build the Config first, then
	// NewACMEIssuer(cfg, ...) (which stores a reference back to cfg for
	// its own storage access during challenges), then assign the issuer
	// onto cfg.Issuers — not the other way around.
	cfg := certmagic.New(s.cache, certmagic.Config{Storage: s.storage})
	issuer := certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
		CA:                      ca,
		Email:                   serve.ACMEEmail,
		Agreed:                  true,
		DisableHTTPChallenge:    true, // DNS-01 only — no port 80 needed for issuance
		DisableTLSALPNChallenge: true,
		DNS01Solver: &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{DNSProvider: provider},
		},
	})
	cfg.Issuers = []certmagic.Issuer{issuer}

	s.mu.Lock()
	s.configs[serve.Domain] = cfg
	s.mu.Unlock()

	if err := cfg.ManageSync(ctx, []string{serve.Domain}); err != nil {
		return nil, fmt.Errorf("edge: obtain certificate for %s: %w", serve.Domain, err)
	}
	return cfg.TLSConfig(), nil
}

// unmanage undoes TLSConfig's registration for domain: it drops the
// per-domain Config (so a future GetConfigForCert lookup for a stale
// certificate that lingers in the cache fails loudly instead of silently
// reusing a removed site's DNS credentials) and tells the shared Cache to
// stop maintaining/renewing that domain's certificate outright. Without
// this, a removed or renamed site's certificate would keep renewing with
// its old credentials for the rest of the process's life.
func (s *certmagicSource) unmanage(domain string) {
	s.mu.Lock()
	delete(s.configs, domain)
	s.mu.Unlock()
	s.cache.RemoveManaged([]certmagic.SubjectIssuer{{Subject: domain}})
}
