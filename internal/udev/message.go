// Package udev makes the virtual input devices Wolf creates visible to the
// app the way udev would: a database entry under /run/udev/data for every
// node, so libudev reports the device as initialized (Wolf's own libinput,
// and SDL in the app, both refuse devices that are not), and a hotplug
// message on the pod's netlink bus in the format udevd's monitor clients
// expect, so an app that is already running picks the controller up.
//
// A pod has no udevd of its own: the node's udevd runs in the host network
// namespace, and its messages never reach the pod. This is what Wolf's
// docker runner does with `fake-udev` for the containers it starts.
package udev

import (
	"encoding/binary"
	"sort"
)

// libudev monitor message: "libudev\0", the magic, then the header words
// (see udev_monitor_netlink_header in systemd), followed by "KEY=VALUE\0"
// properties. Receivers read the magic and the hashes in network byte
// order and the offsets in host order.
const (
	monitorMagic = 0xfeedcafe
	headerSize   = 40
	// udevGroup is the netlink multicast group udevd sends to (the
	// kernel's own uevents go to group 1).
	udevGroup = 2
)

// Message encodes props as one libudev monitor message. The subsystem
// and devtype hashes come from the SUBSYSTEM and DEVTYPE properties.
func Message(props map[string]string) []byte {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	body := make([]byte, 0, 512)
	for _, k := range keys {
		body = append(body, k...)
		body = append(body, '=')
		body = append(body, props[k]...)
		body = append(body, 0)
	}
	hdr := make([]byte, headerSize)
	copy(hdr, "libudev\x00")
	binary.BigEndian.PutUint32(hdr[8:], monitorMagic)
	binary.NativeEndian.PutUint32(hdr[12:], headerSize)
	binary.NativeEndian.PutUint32(hdr[16:], headerSize)
	binary.NativeEndian.PutUint32(hdr[20:], uint32(len(body)))
	if s := props["SUBSYSTEM"]; s != "" {
		binary.BigEndian.PutUint32(hdr[24:], murmurHash2([]byte(s), 0))
	}
	if d := props["DEVTYPE"]; d != "" {
		binary.BigEndian.PutUint32(hdr[28:], murmurHash2([]byte(d), 0))
	}
	// Tag bloom filters stay zero: nothing in a game pod filters by tag.
	return append(hdr, body...)
}
