// Package devices is the games-operator device plugin: it hands app pods
// the character device numbers Wolf's virtual input devices will have,
// through a CDI spec, so the containers open them without being
// privileged.
//
// A container's device cgroup only admits device numbers listed when the
// container is created, and Wolf creates its devices later, once a client
// connects. There is no wildcard rule in Kubernetes, so the spec lists
// every number the input subsystem can hand out (event, js and mouse
// nodes of major 13, uinput and uhid, the hidraw major) as nodes that are
// created in the container up front; when the kernel later creates the
// matching device, the node simply starts working.
package devices

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// Kind is the CDI kind; Resource the extended resource pods request.
	Kind     = "games-operator.io/input"
	Resource = "games-operator.io/input"
	// DeviceName is the single CDI device, so the qualified name is
	// games-operator.io/input=all.
	DeviceName = "all"
	// SpecFile is the spec's file name under the CDI directory.
	SpecFile = "games-operator-input.yaml"
	// Annotation is the CDI annotation set next to the CRI field, for
	// runtimes that only read annotations.
	Annotation = "cdi.k8s.io/games-operator-input"
)

// QualifiedName is what a container is given.
func QualifiedName() string { return Kind + "=" + DeviceName }

// HidrawMajor reads the hidraw major from /proc/devices content; 0 when
// the module is not loaded.
func HidrawMajor(procDevices string) int {
	sc := bufio.NewScanner(strings.NewReader(procDevices))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[1] == "hidraw" {
			n, _ := strconv.Atoi(f[0])
			return n
		}
	}
	return 0
}

// Spec renders the CDI spec. Minor numbers follow the kernel: event nodes
// 64-95 then the dynamic range from 256, js 0-15, mouse 32-62, mice 63.
func Spec(hidrawMajor int) string {
	var b strings.Builder
	b.WriteString("cdiVersion: \"0.6.0\"\nkind: " + Kind + "\ndevices:\n  - name: " + DeviceName + "\n    containerEdits:\n      deviceNodes:\n")
	node := func(path string, major, minor int) {
		fmt.Fprintf(&b, "        - {path: %s, type: c, major: %d, minor: %d, fileMode: 438, permissions: rwm}\n", path, major, minor)
	}
	node("/dev/uinput", 10, 223)
	node("/dev/uhid", 10, 239)
	for i := 0; i < 32; i++ {
		node(fmt.Sprintf("/dev/input/event%d", i), 13, 64+i)
	}
	for i := 32; i < 64; i++ {
		node(fmt.Sprintf("/dev/input/event%d", i), 13, 256+i-32)
	}
	for i := 0; i < 16; i++ {
		node(fmt.Sprintf("/dev/input/js%d", i), 13, i)
	}
	for i := 0; i < 31; i++ {
		node(fmt.Sprintf("/dev/input/mouse%d", i), 13, 32+i)
	}
	node("/dev/input/mice", 13, 63)
	if hidrawMajor > 0 {
		for i := 0; i < 32; i++ {
			node(fmt.Sprintf("/dev/hidraw%d", i), hidrawMajor, i)
		}
	}
	return b.String()
}

// WriteSpec writes the spec into dir atomically and reports whether it
// changed.
func WriteSpec(dir string, hidrawMajor int) (bool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the runtime reads it
		return false, err
	}
	path := filepath.Join(dir, SpecFile)
	want := Spec(hidrawMajor)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == want {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(want), 0o644); err != nil { //nolint:gosec // see above
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	return true, nil
}
