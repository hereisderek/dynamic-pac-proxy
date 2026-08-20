package main

import (
	"testing"
	"time"
)

// TestLazyHealthCheckTTL verifies the core efficiency contract: a health
// check only happens on a cache miss or after the TTL expires, never on
// every call.
func TestLazyHealthCheckTTL(t *testing.T) {
	eff := effectiveHost{
		Name:            "ttl-host",
		MDNSHostname:    "definitely-does-not-exist.local",
		Port:            9999,
		RefreshInterval: 300 * time.Millisecond,
		MDNSTimeout:     150 * time.Millisecond,
		DialTimeout:     150 * time.Millisecond,
	}

	hs := newHostState()

	start := time.Now()
	hs.getFresh(eff)
	firstElapsed := time.Since(start)
	if firstElapsed < eff.MDNSTimeout {
		t.Fatalf("first call returned too fast (%s), expected a real mDNS attempt taking >= %s", firstElapsed, eff.MDNSTimeout)
	}

	start = time.Now()
	hs.getFresh(eff)
	cachedElapsed := time.Since(start)
	if cachedElapsed > 20*time.Millisecond {
		t.Fatalf("second call within TTL took %s, expected an instant cache hit with no re-check", cachedElapsed)
	}

	time.Sleep(eff.RefreshInterval + 50*time.Millisecond)

	start = time.Now()
	hs.getFresh(eff)
	staleElapsed := time.Since(start)
	if staleElapsed < eff.MDNSTimeout {
		t.Fatalf("call after TTL expiry returned too fast (%s), expected a fresh mDNS attempt taking >= %s", staleElapsed, eff.MDNSTimeout)
	}
}
