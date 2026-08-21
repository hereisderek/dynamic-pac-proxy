package webui

import (
	"fmt"
	"html/template"
	"net/http"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
)

// indexTemplate renders the landing page at "/": one entry per configured
// host with its PAC URL (what to paste into a device's "Automatic Proxy
// Configuration" field) and the equivalent manual proxy address, plus
// links to the other pages this daemon serves.
var indexTemplate = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>dynamic-pac-proxy</title>
<style>
body { font-family: system-ui, sans-serif; max-width: 640px; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; }
h1 { font-size: 1.4rem; }
ul.hosts { list-style: none; padding: 0; }
ul.hosts li { margin: 0 0 1.2rem; padding-bottom: 1.2rem; border-bottom: 1px solid #e0e0e0; }
ul.hosts li:last-child { border-bottom: none; }
.name { font-weight: 600; font-size: 1.05rem; }
.url { margin: 0.3rem 0; }
.url code { background: #f5f5f5; padding: 0.15rem 0.4rem; border-radius: 4px; word-break: break-all; }
.meta { color: #888; font-size: 0.85rem; }
.empty { color: #888; }
nav { margin-top: 1.5rem; font-size: 0.9rem; }
nav a { margin-right: 1rem; }
</style>
</head>
<body>
<h1>dynamic-pac-proxy</h1>
<p>Point a device's "Automatic Proxy Configuration" setting at a PAC URL
below, or configure its proxy manually with the matching address.</p>

{{if .Hosts}}
<ul class="hosts">
{{range .Hosts}}<li>
  <div class="name">{{.Name}}</div>
  <div class="url">PAC: <a href="{{.PACPath}}"><code>{{.PACURL}}</code></a></div>
  <div class="url">Manual: <code>{{.ManualAddr}}</code></div>
  <div class="meta">forwards to {{.Target}}:{{.Port}}{{if .InterceptSSL}} · SSL interception enabled{{end}}</div>
</li>
{{end}}
</ul>
{{else}}
<p class="empty">No hosts configured yet — add one under <code>hosts:</code> in config.yaml.</p>
{{end}}

<nav><a href="/status">/status</a> <a href="/certs">/certs</a></nav>
</body>
</html>
`))

type indexHost struct {
	Name         string
	PACPath      string
	PACURL       string
	ManualAddr   string
	Target       string
	Port         int
	InterceptSSL bool
}

// IndexHandler serves the "/" landing page listing every configured
// host's PAC URL and manual proxy address. It only matches the exact root
// path — ServeMux registers "/" as a catch-all, so anything else falls
// through to it too unless we reject it here, and unknown paths should
// still 404 rather than silently showing this page.
func IndexHandler(cfgStore *config.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		cfg := cfgStore.Snapshot()
		port, _ := config.PortFromAddr(cfg.ListenAddr)

		hosts := make([]indexHost, 0, len(cfg.Hosts))
		for _, h := range cfg.Hosts {
			pacPath := fmt.Sprintf("/proxy/%s.pac", h.Name)
			pacURL := pacPath
			manualAddr := fmt.Sprintf("(set advertise_host in config.yaml):%d", h.ListenPort)
			if cfg.AdvertiseHost != "" {
				pacURL = fmt.Sprintf("http://%s:%d%s", cfg.AdvertiseHost, port, pacPath)
				manualAddr = fmt.Sprintf("%s:%d", cfg.AdvertiseHost, h.ListenPort)
			}
			hosts = append(hosts, indexHost{
				Name:         h.Name,
				PACPath:      pacPath,
				PACURL:       pacURL,
				ManualAddr:   manualAddr,
				Target:       h.Target(),
				Port:         h.Port,
				InterceptSSL: h.InterceptSSL,
			})
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		indexTemplate.Execute(w, struct{ Hosts []indexHost }{Hosts: hosts})
	})
}
