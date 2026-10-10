package devices

import (
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
	s := Spec(511)
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
	if strings.Contains(Spec(0), "hidraw") {
		t.Error("no hidraw nodes without the module")
	}
	dir := t.TempDir()
	if changed, err := WriteSpec(dir, 0); err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	if changed, err := WriteSpec(dir, 0); err != nil || changed {
		t.Fatalf("same content must not rewrite: %v %v", changed, err)
	}
	if changed, _ := WriteSpec(dir, 511); !changed {
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
