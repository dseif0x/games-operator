package udev

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Class is what udev's input_id would tag the device as; it decides the
// ID_INPUT_* properties, which is what SDL and libinput go by.
type Class string

const (
	ClassJoystick      Class = "joystick"
	ClassMouse         Class = "mouse"
	ClassTouchpad      Class = "touchpad"
	ClassKeyboard      Class = "keyboard"
	ClassTouchscreen   Class = "touchscreen"
	ClassTablet        Class = "tablet"
	ClassAccelerometer Class = "accelerometer"
	// ClassKey is a device with keys that is none of the above.
	ClassKey Class = "key"
	// ClassHidraw is the raw HID node of a uhid device (the DualSense):
	// Steam talks to the controller through it.
	ClassHidraw Class = "hidraw"
)

// Device is one device node and what sysfs says about it.
type Device struct {
	Subsystem string // input or hidraw
	Name      string // sysfs name: event16, js0, mouse0, hidraw3
	DevNode   string // /dev/input/event16
	DevPath   string // /devices/virtual/misc/uhid/…/input/input30/event16
	Major     int
	Minor     int
	Class     Class
	Product   string // the input device's name
	Uniq      string // its unique id (the DualSense's MAC)
	// Virtual is true for devices created through uinput or uhid, which
	// is all Wolf makes; the node's own hardware is left alone.
	Virtual bool
}

var (
	inputNode  = regexp.MustCompile(`^(event|js|mouse)[0-9]+$`)
	hidrawNode = regexp.MustCompile(`^hidraw[0-9]+$`)
)

// IsInputNode reports whether name is a node under /dev/input worth
// announcing (event*, js*, mouse*; not the mice aggregate).
func IsInputNode(name string) bool { return inputNode.MatchString(name) }

// IsHidrawNode reports whether name is a /dev/hidraw* node.
func IsHidrawNode(name string) bool { return hidrawNode.MatchString(name) }

// Inspect reads the node's sysfs entry (sysRoot is /sys).
func Inspect(sysRoot, subsystem, name string) (*Device, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "class", subsystem, name))
	if err != nil {
		return nil, err
	}
	devPath := strings.TrimPrefix(resolved, filepath.Clean(sysRoot))
	ue, err := readKV(filepath.Join(resolved, "uevent"))
	if err != nil {
		return nil, err
	}
	if ue["DEVNAME"] == "" || ue["MAJOR"] == "" {
		return nil, fmt.Errorf("%s/%s: uevent without DEVNAME/MAJOR", subsystem, name)
	}
	d := &Device{
		Subsystem: subsystem, Name: name, DevPath: devPath, DevNode: "/dev/" + ue["DEVNAME"],
		Virtual: strings.HasPrefix(devPath, "/devices/virtual/"),
	}
	d.Major, _ = strconv.Atoi(ue["MAJOR"])
	d.Minor, _ = strconv.Atoi(ue["MINOR"])
	if subsystem == "hidraw" {
		d.Class = ClassHidraw
		hid, _ := readKV(filepath.Join(resolved, "device", "uevent"))
		d.Product, d.Uniq = hid["HID_NAME"], hid["HID_UNIQ"]
		return d, nil
	}
	parent := filepath.Join(resolved, "device")
	d.Product = readLine(filepath.Join(parent, "name"))
	d.Uniq = readLine(filepath.Join(parent, "uniq"))
	d.Class = classify(d.Product, capabilities{
		key:   readBitmap(filepath.Join(parent, "capabilities", "key")),
		abs:   readBitmap(filepath.Join(parent, "capabilities", "abs")),
		rel:   readBitmap(filepath.Join(parent, "capabilities", "rel")),
		props: readBitmap(filepath.Join(parent, "properties")),
	})
	return d, nil
}

// DBName is the device's file under /run/udev/data.
func (d *Device) DBName() string { return fmt.Sprintf("c%d:%d", d.Major, d.Minor) }

// Props are the properties of the device's hotplug message, the ones
// Wolf's docker runner sends (gen_udev_base_event and the per-device
// additions), so an app behaves the same here as in Wolf's own setup.
func (d *Device) Props(action string, seq uint64, now time.Time) map[string]string {
	p := map[string]string{
		"ACTION":           action,
		"SEQNUM":           strconv.FormatUint(seq, 10),
		"USEC_INITIALIZED": strconv.FormatInt(now.UnixMicro(), 10),
		"SUBSYSTEM":        d.Subsystem,
		"ID_INPUT":         "1",
		"ID_SERIAL":        "noserial",
		"TAGS":             ":seat:uaccess:",
		"CURRENT_TAGS":     ":seat:uaccess:",
		"DEVNAME":          d.DevNode,
		"DEVPATH":          d.DevPath,
		"MAJOR":            strconv.Itoa(d.Major),
		"MINOR":            strconv.Itoa(d.Minor),
	}
	switch d.Class {
	case ClassJoystick:
		p["ID_INPUT_JOYSTICK"], p[".INPUT_CLASS"] = "1", "joystick"
		if d.Uniq != "" {
			p["UNIQ"] = d.Uniq
		}
	case ClassMouse:
		p["ID_INPUT_MOUSE"], p[".INPUT_CLASS"] = "1", "mouse"
	case ClassTouchpad:
		p["ID_INPUT_TOUCHPAD"], p[".INPUT_CLASS"] = "1", "mouse"
		p["ID_INPUT_TOUCHPAD_INTEGRATION"] = "internal"
	case ClassKeyboard:
		p["ID_INPUT_KEYBOARD"], p["ID_INPUT_KEY"], p[".INPUT_CLASS"] = "1", "1", "kbd"
	case ClassTouchscreen:
		p["ID_INPUT_TOUCHSCREEN"], p[".INPUT_CLASS"] = "1", "mouse"
	case ClassTablet:
		p["ID_INPUT_TABLET"], p[".INPUT_CLASS"] = "1", "mouse"
	case ClassAccelerometer:
		p["ID_INPUT_ACCELEROMETER"] = "1"
		p["ID_INPUT_WIDTH_MM"], p["ID_INPUT_HEIGHT_MM"] = "8", "8"
		p["IIO_SENSOR_PROXY_TYPE"] = "input-accel"
		if d.Uniq != "" {
			p["UNIQ"] = d.Uniq
		}
	case ClassKey:
		p["ID_INPUT_KEY"] = "1"
	case ClassHidraw:
	}
	return p
}

// DBLines is the udev database entry (the file under /run/udev/data):
// the ID_* properties, the seat and uaccess tags, and the format version.
// Its existence is what makes libudev call the device initialized.
func (d *Device) DBLines() []string {
	if d.Class == ClassHidraw {
		return []string{"V:1"}
	}
	lines := []string{"E:ID_INPUT=1"}
	switch d.Class {
	case ClassJoystick:
		lines = append(lines, "E:ID_INPUT_JOYSTICK=1", "E:ID_BUS=usb")
	case ClassTouchpad:
		lines = append(lines, "E:ID_INPUT_TOUCHPAD=1", "E:ID_BUS=usb")
	case ClassAccelerometer:
		lines = append(lines, "E:ID_INPUT_ACCELEROMETER=1", "E:ID_BUS=usb")
	case ClassMouse:
		lines = append(lines, "E:ID_INPUT_MOUSE=1", "E:ID_SERIAL=noserial")
	case ClassKeyboard:
		lines = append(lines, "E:ID_INPUT_KEYBOARD=1", "E:ID_INPUT_KEY=1", "E:ID_SERIAL=noserial")
	case ClassTouchscreen:
		lines = append(lines, "E:ID_INPUT_TOUCHSCREEN=1", "E:ID_SERIAL=noserial")
	case ClassTablet:
		lines = append(lines, "E:ID_INPUT_TABLET=1", "E:ID_SERIAL=noserial")
	default:
		lines = append(lines, "E:ID_INPUT_KEY=1", "E:ID_SERIAL=noserial")
	}
	return append(lines, "G:seat", "G:uaccess", "Q:seat", "Q:uaccess", "V:1")
}

// Event codes from linux/input-event-codes.h.
const (
	relX, relY        = 0x00, 0x01
	absX, absY        = 0x00, 0x01
	absMTPositionX    = 0x35
	keyQ, keyA, keyEn = 16, 30, 28
	btnLeft           = 0x110
	btnJoystick       = 0x120
	btnGamepad        = 0x130
	btnToolPen        = 0x140
	btnToolFinger     = 0x145
	btnTouch          = 0x14a
	propAccelerometer = 0x06
)

type bitmap []uint64 // least significant word first

func (b bitmap) has(bit int) bool {
	i := bit / 64
	return i < len(b) && b[i]&(1<<(bit%64)) != 0
}

type capabilities struct{ key, abs, rel, props bitmap }

// classify follows udev's input_id in spirit: the capabilities decide,
// the device name only breaks ties the way Wolf names its devices.
func classify(name string, c capabilities) Class {
	l := strings.ToLower(name)
	switch {
	case strings.Contains(l, "touchpad"):
		return ClassTouchpad
	case strings.Contains(l, "motion") || c.props.has(propAccelerometer):
		return ClassAccelerometer
	case c.key.has(btnGamepad) || c.key.has(btnJoystick):
		return ClassJoystick
	case c.key.has(btnToolPen):
		return ClassTablet
	case c.key.has(btnToolFinger) && c.key.has(btnTouch):
		return ClassTouchpad
	case c.key.has(btnTouch) && (c.abs.has(absMTPositionX) || c.abs.has(absX)):
		return ClassTouchscreen
	case c.rel.has(relX) && c.rel.has(relY) && c.key.has(btnLeft):
		return ClassMouse
	case c.abs.has(absX) && c.abs.has(absY) && c.key.has(btnLeft):
		// Wolf's absolute mouse: udev would call it a touchpad.
		return ClassTouchpad
	case c.key.has(keyQ) && c.key.has(keyA) && c.key.has(keyEn):
		return ClassKeyboard
	}
	switch {
	case strings.Contains(l, "pad") || strings.Contains(l, "controller") || strings.Contains(l, "joystick"):
		return ClassJoystick
	case strings.Contains(l, "keyboard"):
		return ClassKeyboard
	case strings.Contains(l, "mouse"):
		return ClassMouse
	case strings.Contains(l, "touch"):
		return ClassTouchscreen
	case strings.Contains(l, "pen") || strings.Contains(l, "tablet"):
		return ClassTablet
	}
	return ClassKey
}

// readBitmap parses a sysfs capability file: hex words, most significant
// first, 64 bits each.
func readBitmap(path string) bitmap {
	words := strings.Fields(readLine(path))
	out := make(bitmap, 0, len(words))
	for i := len(words) - 1; i >= 0; i-- {
		v, err := strconv.ParseUint(words[i], 16, 64)
		if err != nil {
			return nil
		}
		out = append(out, v)
	}
	return out
}

func readLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}

func readKV(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			out[k] = v
		}
	}
	return out, sc.Err()
}
