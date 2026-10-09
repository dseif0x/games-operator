package udev

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMurmurHash2(t *testing.T) {
	// Reference values from Wolf's fake-udev (MurmurHash2.cpp, seed 0).
	for s, want := range map[string]uint32{"input": 0xc1a28470, "hidraw": 0xc2caf397, "usb": 0x0577c5e5, "": 0} {
		if got := murmurHash2([]byte(s), 0); got != want {
			t.Errorf("hash(%q) = %#x, want %#x", s, got, want)
		}
	}
}

func TestMessage(t *testing.T) {
	msg := Message(map[string]string{"SUBSYSTEM": "input", "ACTION": "add", "DEVNAME": "/dev/input/event23", ".INPUT_CLASS": "joystick"})
	if !bytes.HasPrefix(msg, []byte("libudev\x00")) || binary.BigEndian.Uint32(msg[8:]) != 0xfeedcafe {
		t.Fatalf("header: %q", msg[:12])
	}
	off := binary.NativeEndian.Uint32(msg[16:])
	if binary.NativeEndian.Uint32(msg[12:]) != headerSize || off != headerSize {
		t.Fatalf("offsets: %v", msg[12:24])
	}
	if binary.BigEndian.Uint32(msg[24:]) != 0xc1a28470 || binary.BigEndian.Uint32(msg[28:]) != 0 {
		t.Fatalf("filter hashes: %v", msg[24:32])
	}
	body := string(msg[off:])
	if int(binary.NativeEndian.Uint32(msg[20:])) != len(body) {
		t.Fatalf("properties_len %d != %d", binary.NativeEndian.Uint32(msg[20:]), len(body))
	}
	// Sorted, NUL-terminated, the way udevd lays them out.
	if body != ".INPUT_CLASS=joystick\x00ACTION=add\x00DEVNAME=/dev/input/event23\x00SUBSYSTEM=input\x00" {
		t.Fatalf("body: %q", body)
	}
}

// fakeSys builds a sysfs with a DualSense (uhid: joypad, touchpad, motion
// sensors and a hidraw node), a uinput mouse, and the node's own power
// button.
func fakeSys(t *testing.T) string {
	t.Helper()
	sys := t.TempDir()
	type dev struct {
		class, name, target, uevent, product, uniq, key, abs, rel, props string
	}
	uhid := "../../devices/virtual/misc/uhid/0003:054C:0CE6.0010"
	devs := []dev{
		{"input", "event16", uhid + "/input/input30/event16", "MAJOR=13\nMINOR=80\nDEVNAME=input/event16\n",
			"Sony Interactive Entertainment DualSense Wireless Controller", "aa:bb:cc:dd:ee:ff",
			"1000000000000 0 0 0 0", "3", "0", "0"}, // BTN_GAMEPAD, ABS_X/Y
		{"input", "js0", uhid + "/input/input30/js0", "MAJOR=13\nMINOR=0\nDEVNAME=input/js0\n",
			"Sony Interactive Entertainment DualSense Wireless Controller", "aa:bb:cc:dd:ee:ff",
			"1000000000000 0 0 0 0", "3", "0", "0"},
		{"input", "event17", uhid + "/input/input31/event17", "MAJOR=13\nMINOR=81\nDEVNAME=input/event17\n",
			"Sony Interactive Entertainment DualSense Wireless Controller Touchpad", "aa:bb:cc:dd:ee:ff",
			"0", "3", "0", "0"},
		{"input", "event18", uhid + "/input/input32/event18", "MAJOR=13\nMINOR=82\nDEVNAME=input/event18\n",
			"Sony Interactive Entertainment DualSense Wireless Controller Motion Sensors", "aa:bb:cc:dd:ee:ff",
			"0", "3f", "0", "40"}, // INPUT_PROP_ACCELEROMETER
		{"input", "event19", "../../devices/virtual/input/input40/event19", "MAJOR=13\nMINOR=83\nDEVNAME=input/event19\n",
			"Wolf mouse virtual device", "", "10000 0 0 0 0", "0", "3", "0"}, // BTN_LEFT, REL_X/Y
		{"input", "event20", "../../devices/virtual/input/input41/event20", "MAJOR=13\nMINOR=84\nDEVNAME=input/event20\n",
			"Wolf keyboard virtual device", "", "fffffffffffffffe", "0", "0", "0"},
		{"input", "event2", "../../devices/LNXSYSTM:00/LNXPWRBN:00/input/input2/event2", "MAJOR=13\nMINOR=66\nDEVNAME=input/event2\n",
			"Power Button", "", "10000000000000000 0", "0", "0", "0"},
		{"hidraw", "hidraw3", uhid + "/hidraw/hidraw3", "MAJOR=240\nMINOR=3\nDEVNAME=hidraw3\n", "", "", "", "", "", ""},
	}
	for _, d := range devs {
		link := filepath.Join(sys, "class", d.class, d.name)
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(filepath.Dir(link), d.target)
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(d.target, link); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "uevent"), []byte(d.uevent), 0o644); err != nil {
			t.Fatal(err)
		}
		parent := filepath.Dir(target)
		if d.class == "hidraw" {
			parent = filepath.Dir(parent) // the HID device
			if err := os.WriteFile(filepath.Join(parent, "uevent"), []byte("HID_NAME=DualSense\nHID_UNIQ=aa:bb\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.MkdirAll(filepath.Join(parent, "capabilities"), 0o755); err != nil {
				t.Fatal(err)
			}
			for f, v := range map[string]string{"name": d.product, "uniq": d.uniq, "capabilities/key": d.key, "capabilities/abs": d.abs, "capabilities/rel": d.rel, "properties": d.props} {
				if err := os.WriteFile(filepath.Join(parent, f), []byte(v+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := os.Lstat(filepath.Join(target, "device")); err != nil {
			if err := os.Symlink("..", filepath.Join(target, "device")); err != nil {
				t.Fatal(err)
			}
		}
	}
	return sys
}

func TestInspect(t *testing.T) {
	sys := fakeSys(t)
	want := map[string]struct {
		class   Class
		virtual bool
		node    string
		db      string
	}{
		"input/event16":  {ClassJoystick, true, "/dev/input/event16", "c13:80"},
		"input/js0":      {ClassJoystick, true, "/dev/input/js0", "c13:0"},
		"input/event17":  {ClassTouchpad, true, "/dev/input/event17", "c13:81"},
		"input/event18":  {ClassAccelerometer, true, "/dev/input/event18", "c13:82"},
		"input/event19":  {ClassMouse, true, "/dev/input/event19", "c13:83"},
		"input/event20":  {ClassKeyboard, true, "/dev/input/event20", "c13:84"},
		"input/event2":   {ClassKey, false, "/dev/input/event2", "c13:66"},
		"hidraw/hidraw3": {ClassHidraw, true, "/dev/hidraw3", "c240:3"},
	}
	for key, w := range want {
		sub, name, _ := strings.Cut(key, "/")
		d, err := Inspect(sys, sub, name)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if d.Class != w.class || d.Virtual != w.virtual || d.DevNode != w.node || d.DBName() != w.db {
			t.Errorf("%s: got %+v", key, *d)
		}
		if !strings.HasPrefix(d.DevPath, "/devices/") {
			t.Errorf("%s: devpath %q", key, d.DevPath)
		}
	}
	d, _ := Inspect(sys, "input", "event16")
	p := d.Props("add", 7, time.Unix(1695908821, 0))
	for k, v := range map[string]string{
		"ACTION": "add", "SEQNUM": "7", "SUBSYSTEM": "input", "ID_INPUT": "1", "ID_INPUT_JOYSTICK": "1", ".INPUT_CLASS": "joystick",
		"DEVPATH": "/devices/virtual/misc/uhid/0003:054C:0CE6.0010/input/input30/event16", "MAJOR": "13", "MINOR": "80",
		"UNIQ": "aa:bb:cc:dd:ee:ff", "TAGS": ":seat:uaccess:", "USEC_INITIALIZED": "1695908821000000",
	} {
		if p[k] != v {
			t.Errorf("prop %s = %q, want %q", k, p[k], v)
		}
	}
	if got := strings.Join(d.DBLines(), "\n"); got != "E:ID_INPUT=1\nE:ID_INPUT_JOYSTICK=1\nE:ID_BUS=usb\nG:seat\nG:uaccess\nQ:seat\nQ:uaccess\nV:1" {
		t.Errorf("db: %q", got)
	}
	if _, err := Inspect(sys, "input", "event99"); err == nil {
		t.Error("a missing device must be an error")
	}
}

func TestManagerRun(t *testing.T) {
	sys := fakeSys(t)
	dev := t.TempDir()
	udevDir := filepath.Join(t.TempDir(), "udev")
	if err := os.MkdirAll(filepath.Join(dev, "input"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Present before the manager starts: a Wolf mouse and the node's power button.
	for _, n := range []string{"input/event19", "input/event2"} {
		if err := os.WriteFile(filepath.Join(dev, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	msgs := make(chan map[string]string, 16)
	m := &Manager{DevRoot: dev, SysRoot: sys, UdevDir: udevDir, Send: func(b []byte) error {
		msgs <- parse(b)
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	next := func() map[string]string {
		t.Helper()
		select {
		case p := <-msgs:
			return p
		case <-time.After(5 * time.Second):
			t.Fatal("no message")
			return nil
		}
	}
	if p := next(); p["ACTION"] != "add" || p["DEVNAME"] != "/dev/input/event19" || p["ID_INPUT_MOUSE"] != "1" {
		t.Fatalf("scan: %v", p)
	}
	if _, err := os.Stat(filepath.Join(udevDir, "data", "c13:83")); err != nil {
		t.Fatal("database entry for the mouse:", err)
	}
	if _, err := os.Stat(filepath.Join(udevDir, "control")); err != nil {
		t.Fatal("control file:", err)
	}
	if _, err := os.Stat(filepath.Join(udevDir, "data", "c13:66")); err == nil {
		t.Fatal("the node's own power button must not be announced")
	}
	if st, _ := os.Stat(filepath.Join(dev, "input/event19")); st.Mode().Perm() != 0o666 {
		t.Fatalf("node mode %v", st.Mode())
	}
	// Hotplug: the controller and its hidraw node.
	for _, n := range []string{"input/event16", "input/js0", "hidraw3"} {
		if err := os.WriteFile(filepath.Join(dev, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]string{}
	for i := 0; i < 3; i++ {
		p := next()
		seen[p["DEVNAME"]] = p["ACTION"] + "/" + p["SUBSYSTEM"] + "/" + p["ID_INPUT_JOYSTICK"]
	}
	if seen["/dev/input/event16"] != "add/input/1" || seen["/dev/input/js0"] != "add/input/1" || seen["/dev/hidraw3"] != "add/hidraw/" {
		t.Fatalf("hotplug: %v", seen)
	}
	if b, err := os.ReadFile(filepath.Join(udevDir, "data", "c13:80")); err != nil || !strings.Contains(string(b), "E:ID_INPUT_JOYSTICK=1\n") {
		t.Fatalf("controller database entry: %q %v", b, err)
	}
	if err := os.Remove(filepath.Join(dev, "input/event16")); err != nil {
		t.Fatal(err)
	}
	if p := next(); p["ACTION"] != "remove" || p["DEVNAME"] != "/dev/input/event16" || p["DEVPATH"] == "" {
		t.Fatalf("remove: %v", p)
	}
	if _, err := os.Stat(filepath.Join(udevDir, "data", "c13:80")); err == nil {
		t.Fatal("database entry must go with the device")
	}
	if n := len(m.Known()); n != 3 {
		t.Fatalf("known %d", n)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func parse(msg []byte) map[string]string {
	out := map[string]string{}
	off := binary.NativeEndian.Uint32(msg[16:])
	for _, kv := range strings.Split(strings.TrimRight(string(msg[off:]), "\x00"), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}
