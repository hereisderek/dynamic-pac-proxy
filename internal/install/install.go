// Package install implements `--install`/`--uninstall`: detecting the init
// system (systemd or OpenRC), rendering + writing the matching service
// definition, and seeding a config file if none exists yet.
package install

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
)

const (
	systemdUnitPath  = "/etc/systemd/system/dynamic-pac-proxy.service"
	openrcScriptPath = "/etc/init.d/dynamic-pac-proxy"
)

const systemdUnitTemplate = `[Unit]
Description=Dynamic PAC proxy (resolves configured hosts over mDNS)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --config %s
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
`

const openrcScriptTemplate = `#!/sbin/openrc-run

name="dynamic-pac-proxy"
description="Dynamic PAC proxy (resolves configured hosts over mDNS)"

command="%s"
command_args="--config %s"

# The binary runs in the foreground and doesn't fork/daemonize itself, so
# it's supervised (started, restarted on crash, tracked by pid) rather than
# backgrounded — this is what supervise-daemon is for.
supervisor="supervise-daemon"
pidfile="/run/${RC_SVCNAME}.pid"
respawn_delay=3
output_log="/var/log/${RC_SVCNAME}.log"
error_log="/var/log/${RC_SVCNAME}.log"

depend() {
	need net
	after firewall
}
`

// detectInitSystem identifies the running init system. /run/systemd/system
// is the standard, documented way for applications to check whether the
// system was booted with systemd (see sd_booted(3)); rc-service on PATH is
// a reliable signal for OpenRC (Alpine, Gentoo, ...).
func detectInitSystem() string {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return "systemd"
	}
	if _, err := exec.LookPath("rc-service"); err == nil {
		return "openrc"
	}
	return ""
}

func runCmd(name string, args ...string) error {
	log.Printf("running: %s %s", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// ensureConfigFile seeds path with seedYAML if nothing exists there yet. It
// never overwrites an existing file.
func ensureConfigFile(path string, seedYAML []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, seedYAML, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	log.Printf("wrote default config to %s (edit it, then restart the service)", path)
	return nil
}

// Run installs and starts the service. seedYAML is the example config
// (embedded by the caller from deploy/config.yaml) used to seed a fresh
// config file if the resolved path doesn't have one yet.
func Run(configFlagValue string, seedYAML []byte) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must be run as root, e.g.: sudo %s --install", os.Args[0])
	}

	binaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("determine binary path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(binaryPath); err == nil {
		binaryPath = resolved
	}

	configPath, source := config.ResolveConfigPath(configFlagValue)
	log.Printf("using config file: %s (%s)", configPath, source)
	if err := ensureConfigFile(configPath, seedYAML); err != nil {
		return err
	}

	switch initSystem := detectInitSystem(); initSystem {
	case "systemd":
		return installSystemd(binaryPath, configPath)
	case "openrc":
		return installOpenRC(binaryPath, configPath)
	default:
		return fmt.Errorf("couldn't detect a supported init system (systemd or OpenRC) — see README.md for manual install instructions")
	}
}

func installSystemd(binaryPath, configPath string) error {
	unit := fmt.Sprintf(systemdUnitTemplate, binaryPath, configPath)
	if err := os.WriteFile(systemdUnitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", systemdUnitPath, err)
	}
	log.Printf("wrote %s", systemdUnitPath)

	if err := runCmd("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runCmd("systemctl", "enable", "--now", "dynamic-pac-proxy"); err != nil {
		return err
	}
	log.Printf("installed and started via systemd. Check with: systemctl status dynamic-pac-proxy")
	return nil
}

func installOpenRC(binaryPath, configPath string) error {
	script := fmt.Sprintf(openrcScriptTemplate, binaryPath, configPath)
	if err := os.WriteFile(openrcScriptPath, []byte(script), 0o755); err != nil {
		return fmt.Errorf("write %s: %w", openrcScriptPath, err)
	}
	log.Printf("wrote %s", openrcScriptPath)

	if err := runCmd("rc-update", "add", "dynamic-pac-proxy", "default"); err != nil {
		return err
	}
	if err := runCmd("rc-service", "dynamic-pac-proxy", "start"); err != nil {
		return err
	}
	log.Printf("installed and started via OpenRC. Check with: rc-service dynamic-pac-proxy status")
	return nil
}

// Uninstall stops and removes the installed service definition, leaving
// the config file in place.
func Uninstall() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must be run as root, e.g.: sudo %s --uninstall", os.Args[0])
	}

	var err error
	switch initSystem := detectInitSystem(); initSystem {
	case "systemd":
		err = uninstallSystemd()
	case "openrc":
		err = uninstallOpenRC()
	default:
		return fmt.Errorf("couldn't detect a supported init system (systemd or OpenRC) — remove the service manually")
	}
	if err != nil {
		return err
	}
	log.Printf("uninstalled. Config left in place — remove /etc/dynamic-pac-proxy yourself if you no longer need it.")
	return nil
}

func uninstallSystemd() error {
	if _, err := os.Stat(systemdUnitPath); err != nil {
		log.Printf("%s not present, nothing to remove", systemdUnitPath)
		return nil
	}
	_ = runCmd("systemctl", "disable", "--now", "dynamic-pac-proxy")
	if err := os.Remove(systemdUnitPath); err != nil {
		return fmt.Errorf("remove %s: %w", systemdUnitPath, err)
	}
	log.Printf("removed %s", systemdUnitPath)
	_ = runCmd("systemctl", "daemon-reload")
	return nil
}

func uninstallOpenRC() error {
	if _, err := os.Stat(openrcScriptPath); err != nil {
		log.Printf("%s not present, nothing to remove", openrcScriptPath)
		return nil
	}
	_ = runCmd("rc-service", "dynamic-pac-proxy", "stop")
	_ = runCmd("rc-update", "del", "dynamic-pac-proxy", "default")
	if err := os.Remove(openrcScriptPath); err != nil {
		return fmt.Errorf("remove %s: %w", openrcScriptPath, err)
	}
	log.Printf("removed %s", openrcScriptPath)
	return nil
}
