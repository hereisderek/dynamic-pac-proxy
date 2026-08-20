// Package proxy is the actual forward-proxying engine: one HTTP handler per
// configured host that chains to the upstream (Charles) when it's reachable
// or falls back to DIRECT when it's not, plus the Manager that owns each
// host's dedicated listener and keeps them in sync with the live config.
package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
	"github.com/derekhud/dynamic-pac-proxy/internal/health"
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
func NewHostHandler(cfgStore *config.Store, states *health.StateStore, hostName string) http.Handler {
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

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			handleConnect(w, r, cfgStore, states, hostName)
			return
		}

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
	})
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

// handleConnect implements HTTPS tunneling: hijack the client connection,
// establish an upstream tunnel (chained through Charles if reachable, else
// straight to the target), and splice bytes between them.
func handleConnect(w http.ResponseWriter, r *http.Request, cfgStore *config.Store, states *health.StateStore, hostName string) {
	cfg := cfgStore.Snapshot()
	h, ok := cfg.FindHost(hostName)
	if !ok {
		http.Error(w, "host no longer configured", http.StatusBadGateway)
		return
	}
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
	srv := &http.Server{Handler: NewHostHandler(m.cfgStore, m.states, name)}
	m.running[name] = &runningHost{listenPort: h.ListenPort, server: srv}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("host %q: proxy server on %s exited: %v", name, addr, err)
		}
	}()
	log.Printf("host %q: proxying on %s -> %s:%d (falls back to DIRECT if unreachable)", name, addr, h.MDNSHostname, h.Port)
}
