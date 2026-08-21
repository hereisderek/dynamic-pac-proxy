// Package edge is the daemon's presence at the public network edge: a
// standalone HTTPS reverse-proxy server per "serve"-enabled site, fronted
// by a real ACME (Let's Encrypt) certificate obtained via DNS-01, entirely
// independent of the existing CONNECT/intercept_ssl forward-proxy machinery
// in internal/proxy — no client here is this daemon's own PAC-configured
// device, and no local self-signed CA is involved. The two packages share
// only internal/addon, the Starlark addon runtime.
package edge

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/derekhud/dynamic-pac-proxy/internal/addon"
	"github.com/derekhud/dynamic-pac-proxy/internal/config"
)

// acmeTimeout bounds how long a site's first certificate issuance (or a
// later renewal) may block that site's own startup goroutine. A public
// listener should never accept a connection it can't actually terminate
// TLS for, but one slow/failing site must never hold up any other site or
// the rest of the daemon.
const acmeTimeout = 2 * time.Minute

// CertFunc obtains a TLS config that presents a valid certificate for
// serve.Domain, blocking until one is issued/renewed as needed. The real
// implementation (certmagicSource.TLSConfig, wired in via getCertFunc)
// goes through ACME/DNS-01; NewManagerForTesting lets tests substitute a
// fast fake instead, to exercise the reverse-proxy/addon pipeline with
// zero ACME/network involvement — mirroring how internal/proxy.NewHostHandler
// takes a caGetter func parameter for the same reason.
type CertFunc func(ctx context.Context, serve *config.SiteServe) (*tls.Config, error)

// runningSite is one serve-enabled site's live listener.
type runningSite struct {
	listenAddr string
	domain     string
	cancel     context.CancelFunc
	server     *http.Server // nil until the listener is actually up
}

// Manager owns the public HTTPS listener for every serve-enabled site,
// starting/stopping/rebinding them as the config changes — structurally
// like proxy.Manager, but fully independent: no shared state or lifecycle
// coupling with it.
type Manager struct {
	cfgStore *config.Store
	addons   *addon.Runtime

	certFuncMu sync.Mutex
	certFunc   CertFunc // nil means "not built yet"; see getCertFunc

	mu      sync.Mutex
	running map[string]*runningSite // by site name
}

// NewManager builds a Manager that obtains real ACME certificates via
// certmagic/DNS-01 (see certmagic.go).
func NewManager(cfgStore *config.Store, addons *addon.Runtime) *Manager {
	return &Manager{cfgStore: cfgStore, addons: addons, running: make(map[string]*runningSite)}
}

// NewManagerForTesting builds a Manager using certFunc instead of real
// ACME issuance, so tests can exercise Reconcile()'s start/stop/rebind
// lifecycle and the reverse-proxy/addon pipeline against a throwaway
// self-signed certificate.
func NewManagerForTesting(cfgStore *config.Store, addons *addon.Runtime, certFunc CertFunc) *Manager {
	return &Manager{cfgStore: cfgStore, addons: addons, certFunc: certFunc, running: make(map[string]*runningSite)}
}

// getCertFunc lazily builds the shared certmagicSource the first time any
// site actually needs it — nothing touches disk before then, same spirit
// as proxy.Manager.getCA never touching disk until intercept_ssl is
// actually used.
func (m *Manager) getCertFunc() CertFunc {
	m.certFuncMu.Lock()
	defer m.certFuncMu.Unlock()
	if m.certFunc == nil {
		cacheDir := filepath.Join(m.cfgStore.ConfigDir(), "acme-cache")
		m.certFunc = newCertmagicSource(cacheDir).TLSConfig
	}
	return m.certFunc
}

// Reconcile starts a listener for any serve-enabled site in the current
// config that doesn't have one, stops listeners for sites no longer
// present or no longer serve-enabled, and rebinds any whose domain or
// listen_addr changed. Safe to call repeatedly.
func (m *Manager) Reconcile() {
	cfg := m.cfgStore.Snapshot()
	wanted := make(map[string]config.SiteConfig, len(cfg.Sites))
	for _, s := range cfg.Sites {
		if s.Serve != nil {
			wanted[s.Name] = s
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for name, rs := range m.running {
		s, ok := wanted[name]
		if !ok {
			m.stopLocked(name, rs, "removed from config or serve disabled")
			continue
		}
		if s.Serve.ListenAddr != rs.listenAddr || s.Serve.Domain != rs.domain {
			m.stopLocked(name, rs, "serve.listen_addr or serve.domain changed")
			m.startLocked(name, s)
		}
	}

	for name, s := range wanted {
		if _, ok := m.running[name]; !ok {
			m.startLocked(name, s)
		}
	}
}

func (m *Manager) stopLocked(name string, rs *runningSite, reason string) {
	rs.cancel()
	if rs.server != nil {
		go rs.server.Close()
	}
	delete(m.running, name)
	log.Printf("site %q: stopped serving %s (%s)", name, rs.domain, reason)
}

// startLocked kicks off a site's startup — including the potentially slow
// or failing ACME issuance — in its own goroutine, so one site's problems
// never delay the webui mux, proxy.Manager's hosts, or any other Shape B
// site from starting. A site that fails to get a certificate or bind its
// listener is simply retried on the next Reconcile() tick (the existing 3s
// config-poll loop in main.go), not through any bespoke retry machinery.
func (m *Manager) startLocked(name string, s config.SiteConfig) {
	ctx, cancel := context.WithCancel(context.Background())
	placeholder := &runningSite{listenAddr: s.Serve.ListenAddr, domain: s.Serve.Domain, cancel: cancel}
	m.running[name] = placeholder

	go func() {
		if err := m.startSite(ctx, name, s, placeholder); err != nil {
			log.Printf("site %q: %v — will retry", name, err)
			m.mu.Lock()
			if m.running[name] == placeholder {
				delete(m.running, name)
			}
			m.mu.Unlock()
		}
	}()
}

func (m *Manager) startSite(ctx context.Context, name string, s config.SiteConfig, placeholder *runningSite) error {
	acmeCtx, acmeCancel := context.WithTimeout(ctx, acmeTimeout)
	defer acmeCancel()

	tlsConfig, err := m.getCertFunc()(acmeCtx, s.Serve)
	if err != nil {
		return fmt.Errorf("could not obtain a certificate for %s: %w", s.Serve.Domain, err)
	}

	tcpLn, err := net.Listen("tcp", s.Serve.ListenAddr)
	if err != nil {
		return fmt.Errorf("could not listen on %s: %w", s.Serve.ListenAddr, err)
	}
	tlsLn := tls.NewListener(tcpLn, tlsConfig)

	backend, err := url.Parse(s.Serve.Backend) // already validated by config.ValidateConfig
	if err != nil {
		tlsLn.Close()
		return fmt.Errorf("invalid backend %q: %w", s.Serve.Backend, err)
	}

	rp := newReverseProxy(name, backend, m.addons, m.cfgStore.AddonsDir(), s.Addons)
	srv := &http.Server{Handler: rp}

	m.mu.Lock()
	if m.running[name] != placeholder {
		// Reconcile() already stopped/replaced this site while we were
		// busy obtaining a certificate — don't bind a listener nobody
		// wants anymore.
		m.mu.Unlock()
		tlsLn.Close()
		return nil
	}
	placeholder.server = srv
	m.mu.Unlock()

	log.Printf("site %q: serving https://%s on %s -> %s", name, s.Serve.Domain, s.Serve.ListenAddr, s.Serve.Backend)
	if err := srv.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
		log.Printf("site %q: server on %s exited: %v", name, s.Serve.ListenAddr, err)
	}
	return nil
}
