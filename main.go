package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// configPollInterval is how often the config file's mtime is checked for
// hot-reload. It's intentionally not itself configurable.
const configPollInterval = 3 * time.Second

const etcConfigPath = "/etc/dynamic-pac-proxy/config.yaml"

// resolveConfigPath picks the config file location: an explicit --config
// flag wins outright; otherwise /etc/dynamic-pac-proxy/config.yaml if it
// exists, then config.yaml next to the binary if that exists, falling back
// to the /etc path (which loadInitial will treat as "use defaults" if nothing
// is actually there).
func resolveConfigPath(flagValue string) (path string, source string) {
	if flagValue != "" {
		return flagValue, "--config flag"
	}
	if _, err := os.Stat(etcConfigPath); err == nil {
		return etcConfigPath, "default location"
	}
	if exe, err := os.Executable(); err == nil {
		besideBinary := filepath.Join(filepath.Dir(exe), "config.yaml")
		if _, err := os.Stat(besideBinary); err == nil {
			return besideBinary, "next to binary"
		}
	}
	return etcConfigPath, "none found, falling back to default location"
}

func main() {
	configFlag := flag.String("config", "", "path to YAML config file (default: "+etcConfigPath+", or config.yaml next to the binary)")
	installFlag := flag.Bool("install", false, "install and start as a system service (auto-detects systemd or OpenRC), then exit")
	uninstallFlag := flag.Bool("uninstall", false, "stop and remove the installed system service, then exit")
	flag.Parse()

	if *installFlag && *uninstallFlag {
		log.Fatal("--install and --uninstall are mutually exclusive")
	}
	if *installFlag {
		if err := runInstall(*configFlag); err != nil {
			log.Fatalf("install: %v", err)
		}
		return
	}
	if *uninstallFlag {
		if err := runUninstall(); err != nil {
			log.Fatalf("uninstall: %v", err)
		}
		return
	}

	configPath, source := resolveConfigPath(*configFlag)
	log.Printf("using config file: %s (%s)", configPath, source)

	cfgStore, err := loadInitial(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	states := newStateStore()
	manager := newHostManager(cfgStore, states)
	manager.reconcile() // start a refresh loop for every host in the initial config

	go func() {
		ticker := time.NewTicker(configPollInterval)
		defer ticker.Stop()
		for range ticker.C {
			cfgStore.reloadIfChanged()
			manager.reconcile() // pick up hosts added/removed by the reload
		}
	}()

	// listen_addr is read once at startup: changing it requires a restart
	// since it means rebinding the HTTP listener.
	listenAddr := cfgStore.snapshot().ListenAddr

	mux := http.NewServeMux()

	mux.HandleFunc("/proxy.pac", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		w.Write([]byte(combinedPAC(cfgStore.snapshot(), states)))
	})

	mux.HandleFunc("/proxy/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/proxy/"), ".pac")
		if name == "" || !strings.HasSuffix(r.URL.Path, ".pac") {
			http.NotFound(w, r)
			return
		}
		cfg := cfgStore.snapshot()
		if _, ok := cfg.findHost(name); !ok {
			http.NotFound(w, r)
			return
		}
		snap, _ := states.get(name)
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		w.Write([]byte(buildPAC(snap.ip, snap.port, snap.reachable)))
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeStatusJSON(w, cfgStore.snapshot(), states)
	})

	log.Printf("dynamic-pac-proxy listening on %s (config=%s)", listenAddr, configPath)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
