package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// buildPAC always points at this box's own fixed address for the host —
// reachability of the upstream (Charles) is now handled transparently by
// the forward proxy itself, per-request, so the PAC never needs to change.
func buildPAC(advertiseHost string, listenPort int) string {
	if advertiseHost == "" {
		return "// advertise_host is not set in config.yaml — set it to this box's\n" +
			"// LAN IP or hostname (reachable by your devices), then re-fetch this URL.\n" +
			"function FindProxyForURL(url, host) {\n    return \"DIRECT\";\n}\n"
	}
	return fmt.Sprintf("function FindProxyForURL(url, host) {\n    return \"PROXY %s:%d; DIRECT\";\n}\n", advertiseHost, listenPort)
}

type hostStatus struct {
	Name       string  `json:"name"`
	Hostname   string  `json:"hostname"`
	Port       int     `json:"port"`
	ListenPort int     `json:"listen_port"`
	ResolvedIP *string `json:"resolved_ip"`
	Reachable  bool    `json:"reachable"`
	LastCheck  string  `json:"last_check"`
	Error      string  `json:"error"`
}

// writeStatusJSON reports each host's cached health, forcing a check for
// any host whose cache is past its refresh_interval TTL — viewing /status
// counts as "a request came in" just like traffic hitting a proxy port.
func writeStatusJSON(w http.ResponseWriter, cfg fileConfig, states *stateStore) {
	out := make([]hostStatus, 0, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		eff := cfg.effective(h)
		snap := states.get(h.Name).getFresh(eff)

		hs := hostStatus{
			Name:       h.Name,
			Hostname:   h.MDNSHostname,
			Port:       h.Port,
			ListenPort: h.ListenPort,
			Reachable:  snap.reachable,
			Error:      snap.lastError,
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
