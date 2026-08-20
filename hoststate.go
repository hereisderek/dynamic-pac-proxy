package main

import (
	"context"
	"log"
	"net"
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

// stateStore holds one hostSnapshot per configured host name, safe for
// concurrent access from the HTTP handlers and each host's refresh loop.
type stateStore struct {
	mu sync.RWMutex
	m  map[string]hostSnapshot
}

func newStateStore() *stateStore {
	return &stateStore{m: make(map[string]hostSnapshot)}
}

func (s *stateStore) set(name string, snap hostSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = snap
}

func (s *stateStore) get(name string) (hostSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap, ok := s.m[name]
	return snap, ok
}

func (s *stateStore) remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
}

// hostManager starts one refresh goroutine per configured host and
// stops/removes it when the host disappears from the config on a
// hot-reload, without disturbing goroutines for hosts that are still
// present.
type hostManager struct {
	cfgStore *configStore
	states   *stateStore

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

func newHostManager(cfgStore *configStore, states *stateStore) *hostManager {
	return &hostManager{
		cfgStore: cfgStore,
		states:   states,
		running:  make(map[string]context.CancelFunc),
	}
}

// reconcile starts a refresh loop for any host in the current config that
// doesn't have one running yet, and stops loops for hosts no longer
// present. Safe to call repeatedly; a no-op call is cheap.
func (m *hostManager) reconcile() {
	cfg := m.cfgStore.snapshot()
	wanted := make(map[string]bool, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		wanted[h.Name] = true
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for name, cancel := range m.running {
		if !wanted[name] {
			cancel()
			delete(m.running, name)
			m.states.remove(name)
			log.Printf("host %q removed from config, stopped", name)
		}
	}

	for name := range wanted {
		if _, ok := m.running[name]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.running[name] = cancel
		go m.runHost(ctx, name)
		log.Printf("host %q added, refresh loop started", name)
	}
}

// runHost re-resolves and re-checks its host on effHost.RefreshInterval,
// re-reading that host's settings from the live config each cycle so a
// hot-reloaded port/timeout change applies without restarting the loop.
func (m *hostManager) runHost(ctx context.Context, name string) {
	for {
		cfg := m.cfgStore.snapshot()
		h, ok := cfg.findHost(name)
		if !ok {
			return
		}
		eff := cfg.effective(h)

		ip, err := ResolveA(eff.MDNSHostname, eff.MDNSTimeout)
		switch {
		case err != nil:
			m.states.set(name, hostSnapshot{
				hostname: eff.MDNSHostname, port: eff.Port,
				lastCheck: time.Now(), lastError: "mdns: " + err.Error(),
			})
		default:
			addr := net.JoinHostPort(ip.String(), strconv.Itoa(eff.Port))
			conn, dialErr := net.DialTimeout("tcp", addr, eff.DialTimeout)
			if dialErr != nil {
				m.states.set(name, hostSnapshot{
					hostname: eff.MDNSHostname, ip: ip, port: eff.Port,
					lastCheck: time.Now(), lastError: "dial: " + dialErr.Error(),
				})
			} else {
				conn.Close()
				m.states.set(name, hostSnapshot{
					hostname: eff.MDNSHostname, ip: ip, port: eff.Port, reachable: true,
					lastCheck: time.Now(),
				})
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(eff.RefreshInterval):
		}
	}
}
