// Package webui holds the auxiliary HTTP endpoints served alongside the
// per-host proxy ports: PAC files, the /status health JSON, and the /certs
// certificate download page.
package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
	"github.com/derekhud/dynamic-pac-proxy/internal/health"
)

// BuildPAC always points at this box's own fixed address for the host —
// reachability of the upstream (Charles) is now handled transparently by
// the forward proxy itself, per-request, so the PAC never needs to change.
func BuildPAC(advertiseHost string, serverPort int) string {
	if advertiseHost == "" {
		return "// advertise_host is not set in config.yaml — set it to this box's\n" +
			"// LAN IP or hostname (reachable by your devices), then re-fetch this URL.\n" +
			"function FindProxyForURL(url, host) {\n    return \"DIRECT\";\n}\n"
	}
	return fmt.Sprintf("function FindProxyForURL(url, host) {\n    return \"PROXY %s:%d; DIRECT\";\n}\n", advertiseHost, serverPort)
}

type HostStatus struct {
	Name       string  `json:"name"`
	Hostname   string  `json:"hostname"`
	HostPort   int     `json:"host_port"`
	ServerPort int     `json:"server_port"`
	ResolvedIP *string `json:"resolved_ip"`
	Reachable  bool    `json:"reachable"`
	LastCheck  string  `json:"last_check"`
	Error      string  `json:"error"`
}

// WriteStatusJSON reports each host's cached health, forcing a check for
// any host whose cache is past its refresh_interval TTL — viewing /status
// counts as "a request came in" just like traffic hitting a proxy port.
func WriteStatusJSON(w http.ResponseWriter, cfg config.FileConfig, states *health.StateStore) {
	out := make([]HostStatus, 0, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		eff := cfg.Effective(h)
		snap := states.Get(h.Name).GetFresh(eff)

		hs := HostStatus{
			Name:       h.Name,
			Hostname:   h.Target(),
			HostPort:   h.HostPort,
			ServerPort: h.ServerPort,
			Reachable:  snap.Reachable,
			Error:      snap.LastError,
		}
		if snap.IP != nil {
			ip := snap.IP.String()
			hs.ResolvedIP = &ip
		}
		if !snap.LastCheck.IsZero() {
			hs.LastCheck = snap.LastCheck.Format(time.RFC3339)
		}
		out = append(out, hs)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
