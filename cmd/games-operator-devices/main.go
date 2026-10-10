// Command games-operator-devices runs on every game node: it writes the
// CDI spec for the input devices and serves the device plugin that hands
// it to app pods (internal/devices).
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dseif0x/games-operator/internal/devices"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("games-operator-devices exited", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cdiDir := envOr("DEVICES_CDI_DIR", "/var/run/cdi")
	socketDir := envOr("DEVICES_SOCKET_DIR", "/var/lib/kubelet/device-plugins")
	procDevices := envOr("DEVICES_PROC_DEVICES", "/proc/devices")
	modules := envOr("DEVICES_MODPROBE", "")
	sysctls := envOr("DEVICES_SYSCTL", "")
	seccompDir := envOr("DEVICES_SECCOMP_DIR", "/var/lib/kubelet/seccomp/games-operator")
	// Where the node keeps its copy of the device nodes (bind-mounted into
	// the containers); as seen from this container, and from the host.
	hostNodes := envOr("DEVICES_HOST_NODES", "/var/lib/games-operator/dev")
	hostNodesOnHost := envOr("DEVICES_HOST_NODES_HOSTPATH", hostNodes)
	// The node's /dev/dri as mounted here; empty leaves the render nodes alone.
	driDir := envOr("DEVICES_DRI_DIR", "")
	log.Info("games-operator-devices starting", "version", version, "cdi_dir", cdiDir, "modprobe", modules, "sysctl", sysctls, "seccomp_dir", seccompDir)

	// The seccomp profile the Proton presets run under (the kubelet reads
	// Localhost profiles from its seccomp directory).
	if seccompDir != "" {
		if changed, err := devices.WriteSeccompProfile(seccompDir); err != nil {
			log.Error("cannot write the seccomp profile", "err", err)
		} else if changed {
			log.Info("seccomp profile written", "file", seccompDir+"/"+devices.SeccompFile)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The modules the devices come from, loaded on the node (needs the
	// host's mount and PID namespaces: hostPID and privileged).
	for _, m := range strings.Split(modules, ",") {
		if m = strings.TrimSpace(m); m == "" {
			continue
		}
		cmd := exec.CommandContext(ctx, "nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "modprobe", m) //nolint:gosec // fixed argv, names from the chart
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Warn("modprobe failed", "module", m, "err", err, "out", strings.TrimSpace(string(out)))
		}
	}

	// Node settings the game pods need (kernel.apparmor_restrict_unprivileged_userns=0
	// for Proton's sandbox without SYS_ADMIN), applied through the host's
	// namespaces like the modules.
	for _, kv := range strings.Split(sysctls, ",") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		cmd := exec.CommandContext(ctx, "nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "sysctl", "-w", kv) //nolint:gosec // fixed argv, settings from the chart
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Warn("sysctl failed", "setting", kv, "err", err, "out", strings.TrimSpace(string(out)))
		} else {
			log.Info("sysctl applied", "setting", kv)
		}
	}

	write := func() {
		pd, err := os.ReadFile(procDevices)
		if err != nil {
			log.Warn("cannot read /proc/devices", "err", err)
			return
		}
		major := devices.HidrawMajor(string(pd))
		if n, err := devices.CreateHostNodes(hostNodes, major); err != nil {
			log.Error("cannot create the device nodes on the node", "dir", hostNodes, "err", err)
			return
		} else if n > 0 {
			log.Info("device nodes created on the node", "dir", hostNodesOnHost, "nodes", n)
		}
		if driDir != "" {
			if n, err := devices.OpenRenderNodes(driDir); err != nil {
				log.Warn("cannot open the render nodes", "dir", driDir, "err", err)
			} else if n > 0 {
				log.Info("render nodes made world-accessible", "dir", driDir, "nodes", n)
			}
		}
		if changed, err := devices.WriteSpec(cdiDir, major, hostNodesOnHost); err != nil {
			log.Error("cannot write the CDI spec", "err", err)
		} else if changed {
			log.Info("CDI spec written", "file", cdiDir+"/"+devices.SpecFile, "hidraw_major", major)
		}
	}
	write()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				write()
			}
		}
	}()

	p := &devices.Plugin{SocketDir: socketDir, Log: log}
	if n := envOr("DEVICES_COUNT", ""); n != "" {
		var c int
		for _, ch := range n {
			if ch < '0' || ch > '9' {
				return errors.New("DEVICES_COUNT must be a number")
			}
			c = c*10 + int(ch-'0')
		}
		p.Count = c
	}
	return p.Run(ctx)
}
