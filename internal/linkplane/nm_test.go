package linkplane_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yerden/herbarium/internal/linkplane"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func TestRunNMDefinedApp1(t *testing.T) {
	binary := filepath.Join(repoRoot(t), "testdata", "fixture", "builddir", "app1", "app1")
	syms, err := linkplane.RunNMDefined(binary)
	if err != nil {
		t.Fatalf("RunNMDefined: %v", err)
	}
	if len(syms) < 8 {
		t.Fatalf("too few symbols: %d", len(syms))
	}
	byName := map[string]linkplane.NMSymbol{}
	for _, s := range syms {
		byName[s.Name] = s
	}
	// hook in app1 should be strong (T) — the strong override won.
	hook, ok := byName["hook"]
	if !ok {
		t.Fatal("missing hook")
	}
	if hook.Kind != "T" {
		t.Errorf("app1.hook kind = %q, want T (strong override wins)", hook.Kind)
	}
	if hook.LinkageKind() != "strong" {
		t.Errorf("app1.hook LinkageKind = %q, want strong", hook.LinkageKind())
	}

	// main, compute, add_ints, mul_ints should be present. never_called
	// is dead-stripped by -Wl,--gc-sections and must NOT appear.
	for _, name := range []string{"main", "compute", "add_ints", "mul_ints"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("missing %s", name)
		}
	}
	if _, ok := byName["never_called"]; ok {
		t.Errorf("never_called present in nm output; expected dead-stripped")
	}
}

func TestRunNMDefinedApp2(t *testing.T) {
	binary := filepath.Join(repoRoot(t), "testdata", "fixture", "builddir", "app2", "app2")
	syms, err := linkplane.RunNMDefined(binary)
	if err != nil {
		t.Fatalf("RunNMDefined: %v", err)
	}
	byName := map[string]linkplane.NMSymbol{}
	for _, s := range syms {
		byName[s.Name] = s
	}
	// hook in app2 should be weak (W) — no strong override, weak wins.
	hook, ok := byName["hook"]
	if !ok {
		t.Fatal("missing hook")
	}
	if hook.Kind != "W" {
		t.Errorf("app2.hook kind = %q, want W (weak from libshared)", hook.Kind)
	}
	if hook.LinkageKind() != "weak" {
		t.Errorf("app2.hook LinkageKind = %q, want weak", hook.LinkageKind())
	}
}

func TestRunNMUndefined(t *testing.T) {
	binary := filepath.Join(repoRoot(t), "testdata", "fixture", "builddir", "app1", "app1")
	syms, err := linkplane.RunNMUndefined(binary)
	if err != nil {
		t.Fatalf("RunNMUndefined: %v", err)
	}
	// printf is undefined in the binary (linked dynamically via glibc).
	var foundPrintf bool
	for _, s := range syms {
		if s.Kind == "U" && (s.Name == "printf" || s.Name == "printf@GLIBC_2.2.5") {
			foundPrintf = true
		}
	}
	if !foundPrintf {
		t.Errorf("no undefined printf entry; got %d syms", len(syms))
	}
}
