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
	"reflect"
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

// Edge listeners are directly reachable from the public internet, unlike
// internal/proxy's LAN-only listeners — so, unlike those, they need their
// own defense against a slow client holding a connection (and its
// goroutine) open indefinitely by trickling in request headers.
// ReadTimeout/WriteTimeout are deliberately left at their zero (unlimited)
// default: this is a reverse proxy, and a hard cap on total
// request/response duration would break large or long-lived
// (streaming/SSE) backend responses, not just slow clients.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
)

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
	// site and addonsDir are exactly what this listener/reverse-proxy was
	// built from — kept so Reconcile can tell a hot-reloaded change to
	// anything captured at start time (backend, addons, ACME settings,
	// credentials, the global addons_dir) apart from "nothing changed",
	// not just a domain/listen_addr change.
	site      config.SiteConfig
	addonsDir string
	cancel    context.CancelFunc
	server    *http.Server // nil until the listener is actually up
}

// Manager owns the public HTTPS listener for every serve-enabled site,
// starting/stopping/rebinding them as the config changes — structurally
// like proxy.Manager, but fully independent: no shared state or lifecycle
// coupling with it.
type Manager struct {
	cfgStore *config.Store
	addons   *addon.Runtime

	certFuncMu  sync.Mutex
	certFunc    CertFunc            // nil means "not built yet"; see getCertFunc
	certRelease func(domain string) // nil for a testing certFunc with nothing to release

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
		source := newCertmagicSource(cacheDir)
		m.certFunc = source.TLSConfig
		m.certRelease = source.unmanage
	}
	return m.certFunc
}

// releaseCert tells the real certmagicSource (if one has been built) to
// stop managing/renewing domain — called when a site is stopped or
// rebuilt. A no-op under NewManagerForTesting, which has no real source to
// release anything from.
func (m *Manager) releaseCert(domain string) {
	m.certFuncMu.Lock()
	release := m.certRelease
	m.certFuncMu.Unlock()
	if release != nil {
		release(domain)
	}
}

// Reconcile starts a listener for any serve-enabled site in the current
// config that doesn't have one, stops listeners for sites no longer
// present or no longer serve-enabled, and rebinds any whose domain or
// listen_addr changed. Safe to call repeatedly.
func (m *Manager) Reconcile() {
	cfg := m.cfgStore.Snapshot()
	addonsDir := m.cfgStore.AddonsDir()
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
		if !reflect.DeepEqual(rs.site, s) || rs.addonsDir != addonsDir {
			m.stopLocked(name, rs, "site configuration changed")
			m.startLocked(name, s, addonsDir)
		}
	}

	for name, s := range wanted {
		if _, ok := m.running[name]; !ok {
			m.startLocked(name, s, addonsDir)
		}
	}
}

func (m *Manager) stopLocked(name string, rs *runningSite, reason string) {
	rs.cancel()
	if rs.server != nil {
		go rs.server.Close()
	}
	delete(m.running, name)
	m.releaseCert(rs.domain)
	log.Printf("site %q: stopped serving %s (%s)", name, rs.domain, reason)
}

// startLocked kicks off a site's startup — including the potentially slow
// or failing ACME issuance — in its own goroutine, so one site's problems
// never delay the webui mux, proxy.Manager's hosts, or any other Shape B
// site from starting. A site that fails to get a certificate or bind its
// listener is simply retried on the next Reconcile() tick (the existing 3s
// config-poll loop in main.go), not through any bespoke retry machinery.
func (m *Manager) startLocked(name string, s config.SiteConfig, addonsDir string) {
	ctx, cancel := context.WithCancel(context.Background())
	placeholder := &runningSite{
		listenAddr: s.Serve.ListenAddr,
		domain:     s.Serve.Domain,
		site:       s,
		addonsDir:  addonsDir,
		cancel:     cancel,
	}
	m.running[name] = placeholder

	go func() {
		if err := m.startSite(ctx, name, s, addonsDir, placeholder); err != nil {
			log.Printf("site %q: %v — will retry", name, err)
			m.mu.Lock()
			if m.running[name] == placeholder {
				delete(m.running, name)
			}
			m.mu.Unlock()
		}
	}()
}

func (m *Manager) startSite(ctx context.Context, name string, s config.SiteConfig, addonsDir string, placeholder *runningSite) error {
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

	rp := newReverseProxy(name, backend, m.addons, addonsDir, s.Addons)
	srv := &http.Server{
		Handler:           rp,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}

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
		// An intentional stop (stopLocked) already removed this site from
		// m.running before closing the listener, so this path is only
		// reached by a real, unexpected failure — surface it so the
		// caller drops the now-dead placeholder and Reconcile's next tick
		// restarts the site, instead of leaving a placeholder that looks
		// "running" forever with no listener behind it.
		return fmt.Errorf("server on %s exited unexpectedly: %w", s.Serve.ListenAddr, err)
	}
	return nil
}
