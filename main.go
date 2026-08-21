// Command dynamic-pac-proxy is the entrypoint: flag parsing, wiring the
// config/health/proxy/webui packages together, and the top-level HTTP mux.
// The actual logic lives under internal/ — see CLAUDE.md for the package
// breakdown.
package main

import (
	_ "embed"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/derekhud/dynamic-pac-proxy/internal/addon"
	"github.com/derekhud/dynamic-pac-proxy/internal/config"
	"github.com/derekhud/dynamic-pac-proxy/internal/edge"
	"github.com/derekhud/dynamic-pac-proxy/internal/health"
	"github.com/derekhud/dynamic-pac-proxy/internal/install"
	"github.com/derekhud/dynamic-pac-proxy/internal/proxy"
	"github.com/derekhud/dynamic-pac-proxy/internal/webui"
)

//go:embed deploy/config.yaml
var seedConfigYAML []byte

// configPollInterval is how often the config file's mtime is checked for
// hot-reload. It's intentionally not itself configurable.
const configPollInterval = 3 * time.Second

func main() {
	configFlag := flag.String("config", "", "path to YAML config file (default: "+config.EtcConfigPath+", or config.yaml next to the binary)")
	installFlag := flag.Bool("install", false, "install and start as a system service (auto-detects systemd or OpenRC), then exit")
	uninstallFlag := flag.Bool("uninstall", false, "stop and remove the installed system service, then exit")
	flag.Parse()

	if *installFlag && *uninstallFlag {
		log.Fatal("--install and --uninstall are mutually exclusive")
	}
	if *installFlag {
		if err := install.Run(*configFlag, seedConfigYAML); err != nil {
			log.Fatalf("install: %v", err)
		}
		return
	}
	if *uninstallFlag {
		if err := install.Uninstall(); err != nil {
			log.Fatalf("uninstall: %v", err)
		}
		return
	}

	configPath, source := config.ResolveConfigPath(*configFlag)
	log.Printf("using config file: %s (%s)", configPath, source)

	cfgStore, err := config.LoadInitial(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// addonRuntime is shared by every "sites:" addon consumer — today just
	// edgeManager (Shape B's public reverse-proxy mirrors), and in the
	// future the intercept_ssl pipeline in internal/proxy too.
	addonRuntime := addon.NewRuntime()

	states := health.NewStateStore()
	manager := proxy.NewManager(cfgStore, states)
	manager.Reconcile() // start a forward-proxy listener for every host in the initial config

	edgeManager := edge.NewManager(cfgStore, addonRuntime)
	edgeManager.Reconcile() // start a public HTTPS listener for every serve-enabled site in the initial config

	go func() {
		ticker := time.NewTicker(configPollInterval)
		defer ticker.Stop()
		for range ticker.C {
			cfgStore.ReloadIfChanged()
			manager.Reconcile()     // pick up hosts added/removed/rebound by the reload
			edgeManager.Reconcile() // pick up sites added/removed/rebound by the reload
		}
	}()

	// listen_addr is read once at startup: changing it requires a restart
	// since it means rebinding the HTTP listener.
	listenAddr := cfgStore.Snapshot().ListenAddr

	mux := http.NewServeMux()

	mux.Handle("/", webui.IndexHandler(cfgStore))

	mux.HandleFunc("/proxy/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/proxy/"), ".pac")
		if name == "" || !strings.HasSuffix(r.URL.Path, ".pac") {
			http.NotFound(w, r)
			return
		}
		cfg := cfgStore.Snapshot()
		h, ok := cfg.FindHost(name)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		w.Write([]byte(webui.BuildPAC(cfg.AdvertiseHost, h.ServerPort)))
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		webui.WriteStatusJSON(w, cfgStore.Snapshot(), states)
	})

	if err := os.MkdirAll(cfgStore.CertsDir(), 0o755); err != nil {
		log.Printf("could not create certs dir %s: %v", cfgStore.CertsDir(), err)
	}
	mux.Handle("/certs", webui.CertsIndexHandler(cfgStore))
	mux.Handle("/certs/", webui.CertsFileHandler(cfgStore))

	log.Printf("dynamic-pac-proxy listening on %s (config=%s)", listenAddr, configPath)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
