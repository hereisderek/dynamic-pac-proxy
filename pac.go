package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

func buildPAC(ip net.IP, port int, reachable bool) string {
	if !reachable || ip == nil {
		return "function FindProxyForURL(url, host) {\n    return \"DIRECT\";\n}\n"
	}
	// The "; DIRECT" fallback lets a client fall through if the proxy drops
	// between our health check and the client's actual request.
	return fmt.Sprintf("function FindProxyForURL(url, host) {\n    return \"PROXY %s:%d; DIRECT\";\n}\n", ip.String(), port)
}

// combinedPAC returns the first reachable host (in config order) as a
// PROXY, or DIRECT if none are currently reachable.
func combinedPAC(cfg fileConfig, states *stateStore) string {
	for _, h := range cfg.Hosts {
		if snap, ok := states.get(h.Name); ok && snap.reachable {
			return buildPAC(snap.ip, snap.port, true)
		}
	}
	return buildPAC(nil, 0, false)
}

type hostStatus struct {
	Name       string  `json:"name"`
	Hostname   string  `json:"hostname"`
	Port       int     `json:"port"`
	ResolvedIP *string `json:"resolved_ip"`
	Reachable  bool    `json:"reachable"`
	LastCheck  string  `json:"last_check"`
	Error      string  `json:"error"`
}

func writeStatusJSON(w http.ResponseWriter, cfg fileConfig, states *stateStore) {
	out := make([]hostStatus, 0, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		snap, _ := states.get(h.Name)
		hs := hostStatus{
			Name:      h.Name,
			Hostname:  h.MDNSHostname,
			Port:      h.Port,
			Reachable: snap.reachable,
			Error:     snap.lastError,
		}
		if snap.ip != nil {
			ip := snap.ip.String()
			hs.ResolvedIP = &ip
		}
		if !snap.lastCheck.IsZero() {
			hs.LastCheck = snap.lastCheck.Format(time.RFC3339)
		}
		out = append(out, hs)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
