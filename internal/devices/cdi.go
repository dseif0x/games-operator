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
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ProtonSeccomp is the seccomp profile for the Proton presets: the
// runtime's default profile plus the namespace and mount syscalls
// Proton's sandbox (pressure-vessel, bwrap) needs without CAP_SYS_ADMIN.
// Everything else the default blocks (bpf, keyctl, userfaultfd, module
// loading, reboot, …) stays blocked.
//
//go:embed proton-seccomp.json
var ProtonSeccomp []byte

// SeccompFile is the profile's file name; pods reference it as
// <dir>/SeccompFile relative to the kubelet's seccomp root.
const SeccompFile = "proton.json"

// WriteSeccompProfile installs the profile into dir atomically and
// reports whether it changed.
func WriteSeccompProfile(dir string) (bool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the kubelet reads it
		return false, err
	}
	path := filepath.Join(dir, SeccompFile)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(ProtonSeccomp) {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, ProtonSeccomp, 0o644); err != nil { //nolint:gosec // see above
		return false, err
	}
	return true, os.Rename(tmp, path)
}

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

// Node is one device node of the set.
type Node struct {
	Path         string // in the container, e.g. /dev/input/event0
	Major, Minor int
}

// Nodes lists the set. Minor numbers follow the kernel: event nodes 64-95
// then the dynamic range from 256, js 0-15, mouse 32-62, mice 63.
func Nodes(hidrawMajor int) []Node {
	out := []Node{{"/dev/uinput", 10, 223}, {"/dev/uhid", 10, 239}}
	for i := 0; i < 32; i++ {
		out = append(out, Node{fmt.Sprintf("/dev/input/event%d", i), 13, 64 + i})
	}
	for i := 32; i < 64; i++ {
		out = append(out, Node{fmt.Sprintf("/dev/input/event%d", i), 13, 256 + i - 32})
	}
	for i := 0; i < 16; i++ {
		out = append(out, Node{fmt.Sprintf("/dev/input/js%d", i), 13, i})
	}
	for i := 0; i < 31; i++ {
		out = append(out, Node{fmt.Sprintf("/dev/input/mouse%d", i), 13, 32 + i})
	}
	out = append(out, Node{"/dev/input/mice", 13, 63})
	if hidrawMajor > 0 {
		for i := 0; i < 32; i++ {
			out = append(out, Node{fmt.Sprintf("/dev/hidraw%d", i), hidrawMajor, i})
		}
	}
	return out
}

// Spec renders the CDI spec. With hostDir, every node is one the plugin
// keeps on the node (CreateHostNodes): the device entry names that host
// path, which gives the container its device cgroup rule and, in a pod
// with its own user namespace (where the runtime cannot mknod), a bind
// mount of the node; a mount entry then puts it at its usual /dev path.
// Empty hostDir: nodes are mknod'd in the container at their /dev path,
// which only works without a user namespace.
func Spec(hidrawMajor int, hostDir string) string {
	var b strings.Builder
	b.WriteString("cdiVersion: \"0.6.0\"\nkind: " + Kind + "\ndevices:\n  - name: " + DeviceName + "\n    containerEdits:\n      deviceNodes:\n")
	nodes := Nodes(hidrawMajor)
	for _, n := range nodes {
		if hostDir != "" {
			fmt.Fprintf(&b, "        - {path: %s, hostPath: %s, type: c, major: %d, minor: %d, fileMode: 438, permissions: rwm}\n", hostDir+n.Path, hostDir+n.Path, n.Major, n.Minor)
		} else {
			fmt.Fprintf(&b, "        - {path: %s, type: c, major: %d, minor: %d, fileMode: 438, permissions: rwm}\n", n.Path, n.Major, n.Minor)
		}
	}
	if hostDir != "" {
		b.WriteString("      mounts:\n")
		for _, n := range nodes {
			fmt.Fprintf(&b, "        - {hostPath: %s, containerPath: %s, options: [bind, rw]}\n", hostDir+n.Path, n.Path)
		}
	}
	return b.String()
}

// OpenRenderNodes makes the DRM render nodes in dir (the node's /dev/dri)
// world-accessible: the GPU runtime bind-mounts them into the containers
// with their host ownership, which a pod in its own user namespace
// cannot match. Returns how many were changed.
func OpenRenderNodes(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "renderD") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() == 0o666 {
			continue
		}
		if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // a headless node's render node
			return n, err
		}
		n++
	}
	return n, nil
}

// CreateHostNodes mknods the set below dir (dir/dev/input/event0, …),
// world-readable and -writable, and reports how many were created. The
// directory must be on a filesystem mounted without nodev.
func CreateHostNodes(dir string, hidrawMajor int) (int, error) {
	created := 0
	for _, n := range Nodes(hidrawMajor) {
		path := filepath.Join(dir, n.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // device directory
			return created, err
		}
		if st, err := os.Lstat(path); err == nil {
			if st.Mode()&os.ModeCharDevice != 0 {
				_ = os.Chmod(path, 0o666) //nolint:gosec // the app's user must open it
				continue
			}
			_ = os.Remove(path)
		}
		if err := unix.Mknod(path, unix.S_IFCHR|0o666, int(unix.Mkdev(uint32(n.Major), uint32(n.Minor)))); err != nil { //nolint:gosec // major/minor are small
			return created, fmt.Errorf("mknod %s: %w", path, err)
		}
		_ = os.Chmod(path, 0o666) //nolint:gosec // see above
		created++
	}
	return created, nil
}

// WriteSpec writes the spec into dir atomically and reports whether it
// changed.
func WriteSpec(dir string, hidrawMajor int, hostDir string) (bool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the runtime reads it
		return false, err
	}
	path := filepath.Join(dir, SpecFile)
	want := Spec(hidrawMajor, hostDir)
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
