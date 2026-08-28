package gccdump_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yerden/herbarium/internal/gccdump"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func TestParseCIFixture(t *testing.T) {
	ci, err := gccdump.ParseCIFile(filepath.Join(
		repoRoot(t), "testdata", "samples", "gcc-16", "app1", "main.c.ci",
	))
	if err != nil {
		t.Fatalf("ParseCIFile: %v", err)
	}
	if ci.Title == "" {
		t.Error("Title empty")
	}
	if _, ok := ci.Nodes["main"]; !ok {
		t.Errorf("missing node 'main'; nodes=%v", nodeNames(ci))
	}
	main := ci.Nodes["main"]
	if main.IsExternal {
		t.Error("main marked external")
	}
	if main.DeclFile == "" || main.DeclLine == 0 {
		t.Errorf("main lacks decl location: %+v", main)
	}
	if main.StackBytes == 0 {
		t.Errorf("main lacks stack usage: %+v", main)
	}
	if main.StackKind != "static" {
		t.Errorf("main.StackKind = %q, want static", main.StackKind)
	}

	// External references — printf, compute, hook — must be marked
	// external and lack stack usage.
	for _, name := range []string{"printf", "compute", "hook"} {
		n, ok := ci.Nodes[name]
		if !ok {
			t.Errorf("missing node %s", name)
			continue
		}
		if !n.IsExternal {
			t.Errorf("%s not marked external", name)
		}
		if n.StackBytes != 0 {
			t.Errorf("%s: stack bytes %d, want 0", name, n.StackBytes)
		}
	}

	// Indirect placeholder must exist and be flagged.
	ic, ok := ci.Nodes["__indirect_call"]
	if !ok {
		t.Fatalf("missing __indirect_call placeholder")
	}
	if !ic.IsIndirectPlaceholder {
		t.Error("__indirect_call not flagged as indirect placeholder")
	}

	// Edges: expect main → compute, main → hook, main → printf, plus
	// two indirect edges to __indirect_call (for g_ops.add and g_ops.mul
	// which inlined into main).
	var direct, indirect int
	for _, e := range ci.Edges {
		if e.Source != "main" {
			continue
		}
		if e.Indirect {
			indirect++
		} else {
			direct++
		}
		if e.SiteFile == "" || e.SiteLine == 0 {
			t.Errorf("edge %s → %s missing site location", e.Source, e.Target)
		}
	}
	if indirect != 2 {
		t.Errorf("indirect edges from main = %d, want 2 (g_ops.add + g_ops.mul)", indirect)
	}
	if direct < 3 {
		t.Errorf("direct edges from main = %d, want at least 3 (compute, hook, printf)", direct)
	}
}

func nodeNames(ci *gccdump.CI) []string {
	out := make([]string, 0, len(ci.Nodes))
	for k := range ci.Nodes {
		out = append(out, k)
	}
	return out
}
