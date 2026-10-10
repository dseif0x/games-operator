package devices

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpec(t *testing.T) {
	if got := HidrawMajor("Character devices:\n 10 misc\n 13 input\n511 hidraw\n"); got != 511 {
		t.Fatalf("hidraw major %d", got)
	}
	if got := HidrawMajor(" 13 input\n"); got != 0 {
		t.Fatalf("no hidraw: %d", got)
	}
	s := Spec(511, "")
	for _, want := range []string{
		"kind: games-operator.io/input", "name: all",
		"{path: /dev/uinput, type: c, major: 10, minor: 223, fileMode: 438, permissions: rwm}",
		"{path: /dev/uhid, type: c, major: 10, minor: 239,",
		"{path: /dev/input/event0, type: c, major: 13, minor: 64,",
		"{path: /dev/input/event31, type: c, major: 13, minor: 95,",
		"{path: /dev/input/event32, type: c, major: 13, minor: 256,",
		"{path: /dev/input/js0, type: c, major: 13, minor: 0,",
		"{path: /dev/input/mouse0, type: c, major: 13, minor: 32,",
		"{path: /dev/input/mice, type: c, major: 13, minor: 63,",
		"{path: /dev/hidraw0, type: c, major: 511, minor: 0,",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("spec lacks %q", want)
		}
	}
	if strings.Contains(Spec(0, ""), "hidraw") {
		t.Error("no hidraw nodes without the module")
	}
	hosted := Spec(511, "/var/lib/games-operator/dev")
	for _, want := range []string{
		"{path: /var/lib/games-operator/dev/dev/input/event0, hostPath: /var/lib/games-operator/dev/dev/input/event0, type: c, major: 13, minor: 64,",
		"      mounts:\n",
		"{hostPath: /var/lib/games-operator/dev/dev/input/event0, containerPath: /dev/input/event0, options: [bind, rw]}",
		"{hostPath: /var/lib/games-operator/dev/dev/uinput, containerPath: /dev/uinput, options: [bind, rw]}",
	} {
		if !strings.Contains(hosted, want) {
			t.Errorf("host-backed spec lacks %q", want)
		}
	}
	if strings.Contains(Spec(0, ""), "mounts:") {
		t.Error("no mounts without host nodes")
	}
	dri := t.TempDir()
	if err := os.WriteFile(filepath.Join(dri, "renderD128"), nil, 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dri, "card1"), nil, 0o660); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"renderD128", "card1"} {
		if err := os.Chmod(filepath.Join(dri, n), 0o660); err != nil { // past the umask
			t.Fatal(err)
		}
	}
	if n, err := OpenRenderNodes(dri); err != nil || n != 1 {
		t.Fatalf("render nodes: %d %v", n, err)
	}
	if st, _ := os.Stat(filepath.Join(dri, "renderD128")); st.Mode().Perm() != 0o666 {
		t.Fatal("render node mode")
	}
	if st, _ := os.Stat(filepath.Join(dri, "card1")); st.Mode().Perm() != 0o660 {
		t.Fatal("card nodes are left alone")
	}
	if n, _ := OpenRenderNodes(dri); n != 0 {
		t.Fatal("idempotent")
	}
	if n := len(Nodes(511)); n != 2+64+16+31+1+32 {
		t.Fatalf("node count %d", n)
	}
	dir := t.TempDir()
	if changed, err := WriteSpec(dir, 0, ""); err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	if changed, err := WriteSpec(dir, 0, ""); err != nil || changed {
		t.Fatalf("same content must not rewrite: %v %v", changed, err)
	}
	if changed, _ := WriteSpec(dir, 511, ""); !changed {
		t.Fatal("new hidraw major must rewrite")
	}
	b, _ := os.ReadFile(filepath.Join(dir, SpecFile))
	if !strings.Contains(string(b), "hidraw31") {
		t.Fatal("rewritten spec")
	}
	if QualifiedName() != "games-operator.io/input=all" {
		t.Fatal(QualifiedName())
	}
}

func TestSeccompProfile(t *testing.T) {
	var prof struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names    []string `json:"names"`
			Action   string   `json:"action"`
			Includes struct {
				Caps []string `json:"caps"`
			} `json:"includes"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal(ProtonSeccomp, &prof); err != nil {
		t.Fatal(err)
	}
	if prof.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Fatalf("default action %q", prof.DefaultAction)
	}
	allowed := map[string]bool{}
	gated := map[string]bool{}
	for _, r := range prof.Syscalls {
		for _, n := range r.Names {
			if r.Action == "SCMP_ACT_ALLOW" && len(r.Includes.Caps) == 0 {
				allowed[n] = true
			}
			for _, c := range r.Includes.Caps {
				if c == "CAP_SYS_ADMIN" {
					gated[n] = true
				}
			}
		}
	}
	for _, n := range []string{"unshare", "setns", "mount", "umount2", "pivot_root", "clone", "clone3", "mount_setattr", "open_tree", "move_mount"} {
		if !allowed[n] {
			t.Errorf("%s must be allowed for Proton's sandbox", n)
		}
	}
	for _, n := range []string{"bpf", "keyctl", "add_key", "userfaultfd", "kexec_load", "init_module", "finit_module", "reboot", "open_by_handle_at", "perf_event_open"} {
		if allowed[n] {
			t.Errorf("%s must stay blocked without a capability", n)
		}
	}
	if gated["unshare"] || gated["mount"] {
		t.Error("the namespace syscalls must not be gated on CAP_SYS_ADMIN any more")
	}
	dir := t.TempDir()
	if changed, err := WriteSeccompProfile(dir); err != nil || !changed {
		t.Fatalf("write: %v %v", changed, err)
	}
	if changed, _ := WriteSeccompProfile(dir); changed {
		t.Fatal("unchanged content must not rewrite")
	}
	// The written profile lists this node's syscall ABIs explicitly: the
	// 32-bit ones too, or a 32-bit Steam client dies with SIGSYS.
	written, _ := os.ReadFile(filepath.Join(dir, SeccompFile))
	var got struct {
		Architectures []string `json:"architectures"`
		ArchMap       []any    `json:"archMap"`
	}
	if err := json.Unmarshal(written, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.ArchMap) != 0 || len(got.Architectures) == 0 {
		t.Fatalf("architectures: %+v", got)
	}
	amd, _ := SeccompProfile("amd64")
	if !strings.Contains(string(amd), `"SCMP_ARCH_X86"`) || !strings.Contains(string(amd), `"SCMP_ARCH_X32"`) || !strings.Contains(string(amd), `"SCMP_ARCH_X86_64"`) {
		t.Fatalf("amd64 profile lacks the 32-bit ABIs: %s", amd[:200])
	}
	arm, _ := SeccompProfile("arm64")
	if !strings.Contains(string(arm), `"SCMP_ARCH_ARM"`) || strings.Contains(string(arm), `"SCMP_ARCH_X86"`) {
		t.Fatal("arm64 profile")
	}
}
