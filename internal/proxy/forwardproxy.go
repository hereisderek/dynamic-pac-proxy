// Package proxy is the actual forward-proxying engine: one HTTP handler per
// configured host that chains to the upstream (Charles) when it's reachable
// or falls back to DIRECT when it's not, plus the Manager that owns each
// host's dedicated listener and keeps them in sync with the live config.
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
	"github.com/derekhud/dynamic-pac-proxy/internal/health"
	"github.com/derekhud/dynamic-pac-proxy/internal/mitm"
)

type upstreamDecisionKey struct{}

// upstreamDecision records, for one in-flight request, whether it was sent
// chained through the upstream (Charles) or DIRECT — so the error handler
// can tell which one actually failed and only invalidate Charles's health
// cache if the failure was on the chained path.
type upstreamDecision struct {
	proxyURL *url.URL // nil means DIRECT
}

// NewHostHandler builds the forward-proxy handler for one configured host.
// On every request it consults that host's lazily-refreshed health cache
// (see the health package) and either chains through the upstream
// (Charles) if it's reachable, or goes DIRECT to the real destination if
// not. If a chained attempt actually fails, the cache is invalidated so
// the next request re-checks right away instead of waiting out
// refresh_interval — see health.State.ReportFailure.
//
// caGetter lazily loads/creates the shared local CA used for hosts with
// intercept_ssl enabled (see internal/mitm) — it's only ever called if a
// CONNECT actually needs it, never eagerly.
func NewHostHandler(cfgStore *config.Store, states *health.StateStore, hostName string, caGetter func() (*mitm.CA, error)) http.Handler {
	proxyHandler := newProxyHandler(cfgStore, states, hostName)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			handleConnect(w, r, cfgStore, states, hostName, caGetter, proxyHandler)
			return
		}
		proxyHandler(w, r)
	})
}

// newProxyHandler builds the shared "decide chained-vs-DIRECT, log, then
// forward" handler used both for plain HTTP proxy requests and — once a
// CONNECT has been TLS-terminated under intercept_ssl — for the decrypted
// HTTPS requests read off that connection. In the latter case req.URL is
// origin-form (no scheme/host) until handleConnectIntercept fills it in;
// this handler itself doesn't care which path a request arrived by.
func newProxyHandler(cfgStore *config.Store, states *health.StateStore, hostName string) http.HandlerFunc {
	rp := &httputil.ReverseProxy{
		Director: func(r *http.Request) {},
		Transport: &http.Transport{
			Proxy: func(r *http.Request) (*url.URL, error) {
				dec, _ := r.Context().Value(upstreamDecisionKey{}).(*upstreamDecision)
				if dec == nil {
					return nil, fmt.Errorf("host %q: internal error: no upstream decision on request", hostName)
				}
				return dec.proxyURL, nil
			},
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if dec, _ := r.Context().Value(upstreamDecisionKey{}).(*upstreamDecision); dec != nil && dec.proxyURL != nil {
				reportUpstreamFailure(cfgStore, states, hostName)
			}
			log.Printf("host %q: proxy error for %s: %v", hostName, r.URL, err)
			http.Error(w, "proxy error: "+err.Error(), http.StatusBadGateway)
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		cfg := cfgStore.Snapshot()
		h, ok := cfg.FindHost(hostName)
		if !ok {
			http.Error(w, "host no longer configured", http.StatusBadGateway)
			return
		}
		eff := cfg.Effective(h)
		snap := states.Get(hostName).GetFresh(eff)

		dec := &upstreamDecision{}
		if snap.Reachable {
			dec.proxyURL = &url.URL{Scheme: "http", Host: net.JoinHostPort(snap.IP.String(), strconv.Itoa(snap.Port))}
		}
		log.Printf("host %q: %s %s from %s -> %s", hostName, r.Method, r.URL, clientIP(r), pathLabel(dec.proxyURL))
		r = r.WithContext(context.WithValue(r.Context(), upstreamDecisionKey{}, dec))
		rp.ServeHTTP(w, r)
	}
}

// reportUpstreamFailure invalidates a host's health cache after a chained
// request actually failed to reach it, rate-limited by that host's
// failure_cooldown so a burst of failing requests doesn't force a real
// mDNS/dial check on every single one — just the first in each window.
func reportUpstreamFailure(cfgStore *config.Store, states *health.StateStore, hostName string) {
	cfg := cfgStore.Snapshot()
	h, ok := cfg.FindHost(hostName)
	if !ok {
		return
	}
	eff := cfg.Effective(h)
	if states.Get(hostName).ReportFailure(eff.FailureCooldown) {
		log.Printf("host %q: chained request failed, invalidating cached health for early recheck", hostName)
	}
}

// handleConnect dispatches a CONNECT to either the opaque byte-splicing
// tunnel (the default) or, for a host with intercept_ssl enabled, full TLS
// termination — falling back to the plain tunnel if the local CA isn't
// available for some reason (e.g. failed to load/generate) rather than
// breaking the connection outright.
func handleConnect(w http.ResponseWriter, r *http.Request, cfgStore *config.Store, states *health.StateStore, hostName string, caGetter func() (*mitm.CA, error), proxyHandler http.HandlerFunc) {
	cfg := cfgStore.Snapshot()
	h, ok := cfg.FindHost(hostName)
	if !ok {
		http.Error(w, "host no longer configured", http.StatusBadGateway)
		return
	}

	if h.InterceptSSL {
		ca, err := caGetter()
		if err != nil {
			log.Printf("host %q: intercept_ssl enabled but the local CA isn't available, falling back to a plain tunnel: %v", hostName, err)
		} else {
			handleConnectIntercept(w, r, hostName, ca, proxyHandler)
			return
		}
	}

	handleConnectTunnel(w, r, cfgStore, states, hostName, h)
}

// handleConnectTunnel implements opaque HTTPS tunneling: hijack the client
// connection, establish an upstream tunnel (chained through Charles if
// reachable, else straight to the target), and splice bytes between them.
// TLS terminates at the client and the real destination, unmodified — this
// box never sees plaintext.
func handleConnectTunnel(w http.ResponseWriter, r *http.Request, cfgStore *config.Store, states *health.StateStore, hostName string, h config.HostConfig) {
	cfg := cfgStore.Snapshot()
	eff := cfg.Effective(h)
	snap := states.Get(hostName).GetFresh(eff)

	var upstream net.Conn
	chained := false
	if snap.Reachable {
		conn, err := chainedConnect(snap.IP.String(), snap.Port, r.Host, eff.DialTimeout)
		if err != nil {
			log.Printf("host %q: chained CONNECT to %s via upstream failed, falling back to DIRECT: %v", hostName, r.Host, err)
			reportUpstreamFailure(cfgStore, states, hostName)
		} else {
			upstream = conn
			chained = true
		}
	}
	if upstream == nil {
		conn, err := net.DialTimeout("tcp", r.Host, eff.DialTimeout)
		if err != nil {
			http.Error(w, "could not connect to "+r.Host+": "+err.Error(), http.StatusBadGateway)
			return
		}
		upstream = conn
	}
	log.Printf("host %q: CONNECT %s from %s -> %s", hostName, r.Host, clientIP(r), chainedOrDirectLabel(chained))

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		upstream.Close()
		return
	}

	go splice(client, upstream)
}

// handleConnectIntercept implements intercept_ssl: hijack the client
// connection, then instead of tunneling its bytes untouched, terminate TLS
// right here using a leaf certificate the local CA issues on the fly for
// whatever hostname the client's ClientHello asks for. The decrypted
// HTTP/1.1 requests are served through proxyHandler exactly like a plain
// HTTP proxy request — same chained-vs-DIRECT decision, same access
// logging (now with the real decrypted path, not just the CONNECT
// host:port) — except with the URL filled in as an https:// absolute URL
// first, since requests read off a terminated connection arrive in
// origin-form. Forwarding that on with an https:// URL is what makes the
// eventual outbound leg (to the upstream or straight to the destination)
// a fresh, real TLS connection again — nothing between this box and the
// real destination is ever sent in the clear.
func handleConnectIntercept(w http.ResponseWriter, r *http.Request, hostName string, ca *mitm.CA, proxyHandler http.HandlerFunc) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		return
	}

	fallbackHost, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		fallbackHost = r.Host
	}
	tlsConfig := &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return ca.CertificateFor(hello, fallbackHost)
		},
		// Terminating HTTP/2 ourselves would need a second server stack
		// (golang.org/x/net/http2); simplest to just not offer h2 in ALPN
		// so browsers negotiate HTTP/1.1 with us instead, same as Charles
		// and mitmproxy do by default.
		NextProtos: []string{"http/1.1"},
	}
	tlsConn := tls.Server(client, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("host %q: TLS handshake with client failed for %s: %v", hostName, r.Host, err)
		tlsConn.Close()
		return
	}

	ln := newSingleConnListener(tlsConn)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Scheme == "" {
			r.URL.Scheme = "https"
		}
		if r.URL.Host == "" {
			r.URL.Host = r.Host
		}
		proxyHandler(w, r)
	})}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, errListenerClosed) {
		log.Printf("host %q: intercepted HTTPS session for %s ended: %v", hostName, r.Host, err)
	}
}

// chainedConnect dials the upstream proxy (Charles) and issues our own
// CONNECT on the client's behalf, returning the established tunnel.
func chainedConnect(upstreamHost string, upstreamPort int, target string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(upstreamHost, strconv.Itoa(upstreamPort)), timeout)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("upstream CONNECT returned %s", resp.Status)
	}
	// br may already hold bytes buffered past the response headers (the
	// start of the tunneled stream); read through it, not the raw conn.
	return &bufferedConn{Conn: conn, r: br}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// clientIP extracts the originating device's LAN address for access
// logging. r.RemoteAddr is exactly that here (never Charles's or the
// target's) because client devices connect straight to this box per the
// PAC — see "Why the architecture looks like this" in CLAUDE.md.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func pathLabel(proxyURL *url.URL) string {
	if proxyURL == nil {
		return "DIRECT"
	}
	return "chained via " + proxyURL.Host
}

func chainedOrDirectLabel(chained bool) string {
	if chained {
		return "chained"
	}
	return "DIRECT"
}

func splice(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// errListenerClosed is returned by singleConnListener.Accept once its one
// connection has finished, so http.Server.Serve returns cleanly instead of
// hanging around waiting for a second connection that will never come.
var errListenerClosed = errors.New("proxy: single-connection listener closed")

// singleConnListener adapts one already-established net.Conn (here, a
// freshly TLS-terminated hijacked connection) into a net.Listener, so
// http.Server can drive HTTP/1.1 request/response parsing — including
// keep-alive and pipelining — over it instead of us hand-rolling a
// bufio.Reader + http.ReadRequest loop. The listener closes itself as soon
// as that one connection is closed (by the server, once the client
// disconnects or the connection is otherwise done), which is what lets
// http.Server.Serve return instead of blocking forever on a second Accept.
type singleConnListener struct {
	conn     net.Conn
	accept   chan struct{}
	closed   chan struct{}
	closeOne sync.Once
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	l := &singleConnListener{conn: conn, accept: make(chan struct{}, 1), closed: make(chan struct{})}
	l.accept <- struct{}{}
	return l
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	select {
	case <-l.accept:
		return &notifyOnCloseConn{Conn: l.conn, notify: l.Close}, nil
	case <-l.closed:
		return nil, errListenerClosed
	}
}

func (l *singleConnListener) Close() error {
	l.closeOne.Do(func() { close(l.closed) })
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// notifyOnCloseConn calls notify once, the first time Close is called —
// letting singleConnListener find out (and shut itself down) exactly when
// http.Server is done with the one connection it was given.
type notifyOnCloseConn struct {
	net.Conn
	notify func() error
	once   sync.Once
}

func (c *notifyOnCloseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.notify() })
	return err
}

// runningHost is one host's live forward-proxy listener.
type runningHost struct {
	listenPort int
	server     *http.Server
}

// Manager owns the forward-proxy listener for every configured host,
// starting/stopping/rebinding them as the config is hot-reloaded.
type Manager struct {
	cfgStore *config.Store
	states   *health.StateStore

	caOnce sync.Once
	ca     *mitm.CA
	caErr  error

	mu      sync.Mutex
	running map[string]*runningHost
}

func NewManager(cfgStore *config.Store, states *health.StateStore) *Manager {
	return &Manager{
		cfgStore: cfgStore,
		states:   states,
		running:  make(map[string]*runningHost),
	}
}

// getCA lazily loads (or, on first use anywhere, generates) the shared
// local CA used to terminate TLS for any host with intercept_ssl enabled.
// Nothing touches disk unless intercept_ssl is actually used at least
// once — same "don't do work nobody asked for yet" spirit as the health
// cache in the health package.
func (m *Manager) getCA() (*mitm.CA, error) {
	m.caOnce.Do(func() {
		certPath := filepath.Join(m.cfgStore.CertsDir(), mitm.CACertFileName)
		keyPath := filepath.Join(m.cfgStore.ConfigDir(), mitm.CAKeyFileName)
		m.ca, m.caErr = mitm.LoadOrCreate(certPath, keyPath)
		if m.caErr != nil {
			log.Printf("intercept_ssl: could not load or create the local CA: %v", m.caErr)
		} else {
			log.Printf("intercept_ssl: local CA ready (public cert at %s)", certPath)
		}
	})
	return m.ca, m.caErr
}

// Reconcile starts a listener for any host in the current config that
// doesn't have one, stops listeners for hosts no longer present, and
// rebinds any whose listen_port changed. Safe to call repeatedly.
func (m *Manager) Reconcile() {
	cfg := m.cfgStore.Snapshot()
	wanted := make(map[string]config.HostConfig, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		wanted[h.Name] = h
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for name, rh := range m.running {
		h, ok := wanted[name]
		if !ok {
			m.stopLocked(name, rh, "removed from config")
			continue
		}
		if h.ListenPort != rh.listenPort {
			m.stopLocked(name, rh, fmt.Sprintf("listen_port changed to %d", h.ListenPort))
			m.startLocked(name, h)
		}
	}

	for name, h := range wanted {
		if _, ok := m.running[name]; !ok {
			m.startLocked(name, h)
		}
	}
}

func (m *Manager) stopLocked(name string, rh *runningHost, reason string) {
	go rh.server.Close()
	delete(m.running, name)
	m.states.Remove(name)
	log.Printf("host %q: stopped listening on :%d (%s)", name, rh.listenPort, reason)
}

func (m *Manager) startLocked(name string, h config.HostConfig) {
	addr := fmt.Sprintf(":%d", h.ListenPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("host %q: could not listen on %s: %v", name, addr, err)
		return
	}
	srv := &http.Server{Handler: NewHostHandler(m.cfgStore, m.states, name, m.getCA)}
	m.running[name] = &runningHost{listenPort: h.ListenPort, server: srv}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("host %q: proxy server on %s exited: %v", name, addr, err)
		}
	}()
	log.Printf("host %q: proxying on %s -> %s:%d (falls back to DIRECT if unreachable)", name, addr, h.MDNSHostname, h.Port)
}
