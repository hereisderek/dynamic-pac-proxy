package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// hostSnapshot is the last-known resolution/reachability result for one
// configured host.
type hostSnapshot struct {
	hostname  string
	ip        net.IP
	port      int
	reachable bool
	lastCheck time.Time
	lastError string
}

func isFresh(snap hostSnapshot, ttl time.Duration) bool {
	return !snap.lastCheck.IsZero() && time.Since(snap.lastCheck) < ttl
}

// checkHost does the actual mDNS resolve + TCP dial for one host.
func checkHost(eff effectiveHost) hostSnapshot {
	ip, err := ResolveA(eff.MDNSHostname, eff.MDNSTimeout)
	if err != nil {
		return hostSnapshot{hostname: eff.MDNSHostname, port: eff.Port, lastCheck: time.Now(), lastError: "mdns: " + err.Error()}
	}

	addr := net.JoinHostPort(ip.String(), strconv.Itoa(eff.Port))
	conn, dialErr := net.DialTimeout("tcp", addr, eff.DialTimeout)
	if dialErr != nil {
		return hostSnapshot{hostname: eff.MDNSHostname, ip: ip, port: eff.Port, lastCheck: time.Now(), lastError: "dial: " + dialErr.Error()}
	}
	conn.Close()
	return hostSnapshot{hostname: eff.MDNSHostname, ip: ip, port: eff.Port, reachable: true, lastCheck: time.Now()}
}

// hostState is one host's health cache. Checks happen lazily: getFresh only
// re-resolves/re-dials when the cached result is older than the host's
// refresh_interval, and coalesces concurrent callers behind checkMu so a
// burst of simultaneous requests doesn't trigger duplicate mDNS queries.
type hostState struct {
	checkMu sync.Mutex

	mu   sync.RWMutex
	snap hostSnapshot
}

func newHostState() *hostState { return &hostState{} }

func (h *hostState) getFresh(eff effectiveHost) hostSnapshot {
	h.mu.RLock()
	snap := h.snap
	h.mu.RUnlock()
	if isFresh(snap, eff.RefreshInterval) {
		return snap
	}

	h.checkMu.Lock()
	defer h.checkMu.Unlock()

	// Someone else may have refreshed while we were waiting for checkMu.
	h.mu.RLock()
	snap = h.snap
	h.mu.RUnlock()
	if isFresh(snap, eff.RefreshInterval) {
		return snap
	}

	fresh := checkHost(eff)
	h.mu.Lock()
	h.snap = fresh
	h.mu.Unlock()
	return fresh
}

func (h *hostState) peek() hostSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.snap
}

// stateStore holds one hostState per configured host name.
type stateStore struct {
	mu sync.Mutex
	m  map[string]*hostState
}

func newStateStore() *stateStore {
	return &stateStore{m: make(map[string]*hostState)}
}

func (s *stateStore) get(name string) *hostState {
	s.mu.Lock()
	defer s.mu.Unlock()
	hs, ok := s.m[name]
	if !ok {
		hs = newHostState()
		s.m[name] = hs
	}
	return hs
}

func (s *stateStore) remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
}

// runningHost is one host's live forward-proxy listener.
type runningHost struct {
	listenPort int
	server     *http.Server
}

// hostManager owns the forward-proxy listener for every configured host,
// starting/stopping/rebinding them as the config is hot-reloaded.
type hostManager struct {
	cfgStore *configStore
	states   *stateStore

	mu      sync.Mutex
	running map[string]*runningHost
}

func newHostManager(cfgStore *configStore, states *stateStore) *hostManager {
	return &hostManager{
		cfgStore: cfgStore,
		states:   states,
		running:  make(map[string]*runningHost),
	}
}

// reconcile starts a listener for any host in the current config that
// doesn't have one, stops listeners for hosts no longer present, and
// rebinds any whose listen_port changed. Safe to call repeatedly.
func (m *hostManager) reconcile() {
	cfg := m.cfgStore.snapshot()
	wanted := make(map[string]hostConfig, len(cfg.Hosts))
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

func (m *hostManager) stopLocked(name string, rh *runningHost, reason string) {
	go rh.server.Close()
	delete(m.running, name)
	m.states.remove(name)
	log.Printf("host %q: stopped listening on :%d (%s)", name, rh.listenPort, reason)
}

func (m *hostManager) startLocked(name string, h hostConfig) {
	addr := fmt.Sprintf(":%d", h.ListenPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("host %q: could not listen on %s: %v", name, addr, err)
		return
	}
	srv := &http.Server{Handler: newHostProxyHandler(m.cfgStore, m.states, name)}
	m.running[name] = &runningHost{listenPort: h.ListenPort, server: srv}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("host %q: proxy server on %s exited: %v", name, addr, err)
		}
	}()
	log.Printf("host %q: proxying on %s -> %s:%d (falls back to DIRECT if unreachable)", name, addr, h.MDNSHostname, h.Port)
}
