package udev

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Manager watches the node's /dev for the virtual devices Wolf creates
// and announces each one: a database entry in UdevDir (mounted as
// /run/udev in Wolf and the app), world-readable permissions on the node
// (the app runs as its own user), and a hotplug message through Send.
type Manager struct {
	// DevRoot is the node's /dev as mounted in this container.
	DevRoot string
	// SysRoot is /sys.
	SysRoot string
	// UdevDir is the shared /run/udev.
	UdevDir string
	// Send multicasts one monitor message; nil announces through the
	// database only (apps started after the device still find it).
	Send func([]byte) error
	Log  *slog.Logger
	// Now is the clock (tests).
	Now func() time.Time

	mu       sync.Mutex
	seq      uint64
	known    map[string]*Device
	sendDown bool
}

func (m *Manager) init() {
	if m.Log == nil {
		m.Log = slog.Default()
	}
	if m.SysRoot == "" {
		m.SysRoot = "/sys"
	}
	if m.Now == nil {
		m.Now = time.Now
	}
	if m.known == nil {
		m.known = map[string]*Device{}
	}
}

// Prepare creates the database directory and udev's control socket
// stand-in (its existence tells libudev that a udevd is in charge).
func (m *Manager) Prepare() error {
	m.init()
	if err := os.MkdirAll(filepath.Join(m.UdevDir, "data"), 0o755); err != nil { //nolint:gosec // libudev in the app reads it as another user
		return err
	}
	ctl := filepath.Join(m.UdevDir, "control")
	if _, err := os.Stat(ctl); err != nil {
		if err := os.WriteFile(ctl, nil, 0o666); err != nil { //nolint:gosec // udev's own mode for it
			return err
		}
		_ = os.Chmod(ctl, 0o666) //nolint:gosec // see above
	}
	return nil
}

// Scan announces every virtual device already present.
func (m *Manager) Scan() {
	m.init()
	if entries, err := os.ReadDir(filepath.Join(m.DevRoot, "input")); err == nil {
		for _, e := range entries {
			if IsInputNode(e.Name()) {
				m.add("input", e.Name())
			}
		}
	}
	if entries, err := os.ReadDir(m.DevRoot); err == nil {
		for _, e := range entries {
			if IsHidrawNode(e.Name()) {
				m.add("hidraw", e.Name())
			}
		}
	}
}

// Run prepares, scans, then follows the devices until ctx ends: through
// inotify on the node's /dev when DevRoot is set (the privileged layout),
// else by polling sysfs (the device plugin layout, where the nodes are in
// the container from the start and only the kernel's view changes).
func (m *Manager) Run(ctx context.Context) error {
	if err := m.Prepare(); err != nil {
		return err
	}
	if m.DevRoot == "" {
		return m.poll(ctx)
	}
	// Non-blocking, so the os.File reads through the runtime poller and
	// a Close from the context's goroutine wakes the read up.
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify: %w", err)
	}
	f := os.NewFile(uintptr(fd), "inotify")
	defer f.Close()
	const mask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_TO | unix.IN_MOVED_FROM
	wdInput, err := unix.InotifyAddWatch(fd, filepath.Join(m.DevRoot, "input"), mask)
	if err != nil {
		return fmt.Errorf("watch %s/input: %w", m.DevRoot, err)
	}
	wdDev, err := unix.InotifyAddWatch(fd, m.DevRoot, mask)
	if err != nil {
		return fmt.Errorf("watch %s: %w", m.DevRoot, err)
	}
	// The watches are in place before the scan: nothing slips between.
	m.Scan()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	buf := make([]byte, 64*1024)
	for {
		n, err := f.Read(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return fmt.Errorf("inotify read: %w", err)
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			wd := int(int32(binary.NativeEndian.Uint32(buf[off:])))
			evMask := binary.NativeEndian.Uint32(buf[off+4:])
			nameLen := int(binary.NativeEndian.Uint32(buf[off+12:]))
			name := strings.TrimRight(string(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+nameLen]), "\x00")
			off += unix.SizeofInotifyEvent + nameLen
			var subsystem string
			switch {
			case wd == wdInput && IsInputNode(name):
				subsystem = "input"
			case wd == wdDev && IsHidrawNode(name):
				subsystem = "hidraw"
			default:
				continue
			}
			switch {
			case evMask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0:
				m.add(subsystem, name)
			case evMask&(unix.IN_DELETE|unix.IN_MOVED_FROM) != 0:
				m.remove(subsystem, name)
			}
		}
	}
}

// PollEvery is the sysfs polling interval; a new device is announced
// within it (libinput in Wolf waits up to two seconds for udev).
var PollEvery = 250 * time.Millisecond

// poll announces devices as they come and go in sysfs.
func (m *Manager) poll(ctx context.Context) error {
	t := time.NewTicker(PollEvery)
	defer t.Stop()
	for {
		m.sync()
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// sync compares sysfs with the announced devices.
func (m *Manager) sync() {
	present := map[string]bool{}
	for _, c := range []struct {
		subsystem, dir string
		ok             func(string) bool
	}{
		{"input", filepath.Join(m.SysRoot, "class", "input"), IsInputNode},
		{"hidraw", filepath.Join(m.SysRoot, "class", "hidraw"), IsHidrawNode},
	} {
		entries, err := os.ReadDir(c.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !c.ok(e.Name()) {
				continue
			}
			key := c.subsystem + "/" + e.Name()
			present[key] = true
			m.mu.Lock()
			_, known := m.known[key]
			m.mu.Unlock()
			if !known {
				m.add(c.subsystem, e.Name())
			}
		}
	}
	m.mu.Lock()
	var gone []string
	for key := range m.known {
		if !present[key] {
			gone = append(gone, key)
		}
	}
	m.mu.Unlock()
	for _, key := range gone {
		subsystem, name, _ := strings.Cut(key, "/")
		m.remove(subsystem, name)
	}
}

// Known lists the announced devices.
func (m *Manager) Known() []*Device {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Device, 0, len(m.known))
	for _, d := range m.known {
		if d.Virtual {
			out = append(out, d)
		}
	}
	return out
}

func (m *Manager) add(subsystem, name string) {
	var d *Device
	var err error
	// The node can show up a moment before its sysfs attributes do.
	for i := 0; i < 25; i++ {
		if d, err = Inspect(m.SysRoot, subsystem, name); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		m.Log.Warn("input device not readable in sysfs", "subsystem", subsystem, "name", name, "err", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.known[subsystem+"/"+name]; dup {
		return
	}
	if !d.Virtual {
		// The node's own hardware: remembered so it is not inspected
		// again, never announced.
		m.known[subsystem+"/"+name] = d
		return
	}
	db := filepath.Join(m.UdevDir, "data", d.DBName())
	if err := os.WriteFile(db+".tmp", []byte(strings.Join(d.DBLines(), "\n")+"\n"), 0o644); err != nil { //nolint:gosec // udev's database is world-readable
		m.Log.Warn("udev database entry not written", "node", d.DevNode, "err", err)
		return
	}
	if err := os.Rename(db+".tmp", db); err != nil {
		m.Log.Warn("udev database entry not written", "node", d.DevNode, "err", err)
		return
	}
	if m.DevRoot != "" {
		if err := os.Chmod(m.nodePath(d), 0o666); err != nil { //nolint:gosec // the app's user must open it; the node is the pod's own
			m.Log.Warn("device node permissions not changed", "node", d.DevNode, "err", err)
		}
	}
	m.known[subsystem+"/"+name] = d
	m.announce(d, "add")
	m.Log.Info("input device plugged", "node", d.DevNode, "class", d.Class, "name", d.Product)
}

func (m *Manager) remove(subsystem, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.known[subsystem+"/"+name]
	if !ok {
		return
	}
	delete(m.known, subsystem+"/"+name)
	if !d.Virtual {
		return
	}
	_ = os.Remove(filepath.Join(m.UdevDir, "data", d.DBName()))
	m.announce(d, "remove")
	m.Log.Info("input device unplugged", "node", d.DevNode, "class", d.Class)
}

func (m *Manager) announce(d *Device, action string) {
	if m.Send == nil {
		return
	}
	m.seq++
	if err := m.Send(Message(d.Props(action, m.seq, m.Now()))); err != nil {
		if !m.sendDown {
			m.Log.Warn("hotplug message not sent; apps see new devices only when they start", "err", err)
		}
		m.sendDown = true
		return
	}
	m.sendDown = false
}

func (m *Manager) nodePath(d *Device) string {
	return filepath.Join(m.DevRoot, strings.TrimPrefix(d.DevNode, "/dev/"))
}
