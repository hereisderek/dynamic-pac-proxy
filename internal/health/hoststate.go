// Package health is a lazy, TTL-gated reachability cache: one Snapshot per
// configured host, refreshed with a real mDNS resolve + TCP dial only when
// a caller actually asks for a stale one. There is deliberately no
// background polling ticker per host — see CLAUDE.md's "Why the
// architecture looks like this" for the reasoning.
package health

import (
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
	"github.com/derekhud/dynamic-pac-proxy/internal/mdns"
)

// Snapshot is the last-known resolution/reachability result for one
// configured host.
type Snapshot struct {
	Hostname  string
	IP        net.IP
	Port      int
	Reachable bool
	LastCheck time.Time
	LastError string
}

func isFresh(snap Snapshot, ttl time.Duration) bool {
	return !snap.LastCheck.IsZero() && time.Since(snap.LastCheck) < ttl
}

// checkHost does the actual mDNS resolve + TCP dial for one host.
func checkHost(eff config.EffectiveHost) Snapshot {
	ip, err := mdns.ResolveA(eff.MDNSHostname, eff.MDNSTimeout)
	if err != nil {
		return Snapshot{Hostname: eff.MDNSHostname, Port: eff.Port, LastCheck: time.Now(), LastError: "mdns: " + err.Error()}
	}

	addr := net.JoinHostPort(ip.String(), strconv.Itoa(eff.Port))
	conn, dialErr := net.DialTimeout("tcp", addr, eff.DialTimeout)
	if dialErr != nil {
		return Snapshot{Hostname: eff.MDNSHostname, IP: ip, Port: eff.Port, LastCheck: time.Now(), LastError: "dial: " + dialErr.Error()}
	}
	conn.Close()
	return Snapshot{Hostname: eff.MDNSHostname, IP: ip, Port: eff.Port, Reachable: true, LastCheck: time.Now()}
}

// State is one host's health cache. Checks happen lazily: GetFresh only
// re-resolves/re-dials when the cached result is older than the host's
// refresh_interval, and coalesces concurrent callers behind checkMu so a
// burst of simultaneous requests doesn't trigger duplicate mDNS queries.
type State struct {
	checkMu sync.Mutex

	mu                 sync.RWMutex
	snap               Snapshot
	lastFailureRecheck time.Time
}

func newState() *State { return &State{} }

func (h *State) GetFresh(eff config.EffectiveHost) Snapshot {
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

// ReportFailure invalidates the cache after a live request actually failed
// to reach this host despite the cache saying it was reachable, so the
// next GetFresh call re-checks immediately instead of waiting out the rest
// of refresh_interval. Rate-limited by cooldown: a burst of requests all
// failing in the same window only forces one early recheck, not one each.
// Returns true if it invalidated, false if it was a no-op (still cooling
// down from a previous invalidation).
func (h *State) ReportFailure(cooldown time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.lastFailureRecheck.IsZero() && time.Since(h.lastFailureRecheck) < cooldown {
		return false
	}
	h.lastFailureRecheck = time.Now()
	h.snap.LastCheck = time.Time{}
	return true
}

func (h *State) Peek() Snapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.snap
}

// SetSnapshot overwrites the cached snapshot directly, bypassing a real
// mDNS resolve + TCP dial — used to seed or override cached health state,
// e.g. from tests exercising the proxy/webui packages against a known
// reachability state.
func (h *State) SetSnapshot(snap Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.snap = snap
}

// StateStore holds one State per configured host name.
type StateStore struct {
	mu sync.Mutex
	m  map[string]*State
}

func NewStateStore() *StateStore {
	return &StateStore{m: make(map[string]*State)}
}

func (s *StateStore) Get(name string) *State {
	s.mu.Lock()
	defer s.mu.Unlock()
	hs, ok := s.m[name]
	if !ok {
		hs = newState()
		s.m[name] = hs
	}
	return hs
}

func (s *StateStore) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
}
