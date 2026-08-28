package linkplane_test

import (
	"path/filepath"
	"testing"

	"github.com/yerden/herbarium/internal/linkplane"
)

func TestRunObjdumpApp1(t *testing.T) {
	binary := filepath.Join(repoRoot(t), "testdata", "fixture", "builddir", "app1", "app1")
	edges, err := linkplane.RunObjdump(binary)
	if err != nil {
		t.Fatalf("RunObjdump: %v", err)
	}
	// Expect edges from main to compute, hook, printf@plt.
	fromMain := map[string]bool{}
	for _, e := range edges {
		if e.Caller == "main" {
			fromMain[e.CalleeStripped()] = true
		}
	}
	for _, want := range []string{"compute", "hook", "printf"} {
		if !fromMain[want] {
			t.Errorf("app1: missing main → %s in objdump edges; got %v", want, fromMain)
		}
	}

	// compute → add_ints and compute → mul_ints (from libshared).
	fromCompute := map[string]bool{}
	for _, e := range edges {
		if e.Caller == "compute" {
			fromCompute[e.CalleeStripped()] = true
		}
	}
	if !fromCompute["add_ints"] || !fromCompute["mul_ints"] {
		t.Errorf("app1: compute missing calls to add_ints/mul_ints; got %v", fromCompute)
	}
}

func TestRunObjdumpApp2(t *testing.T) {
	binary := filepath.Join(repoRoot(t), "testdata", "fixture", "builddir", "app2", "app2")
	edges, err := linkplane.RunObjdump(binary)
	if err != nil {
		t.Fatalf("RunObjdump: %v", err)
	}
	// app2 has no strong_override.c; hook resolves to the weak def.
	// At the objdump level we can't tell strong from weak — both look
	// like `call ADDR <hook>` — but the edge must be present.
	var mainCallsHook bool
	for _, e := range edges {
		if e.Caller == "main" && e.CalleeStripped() == "hook" {
			mainCallsHook = true
		}
	}
	if !mainCallsHook {
		t.Errorf("app2: main → hook edge missing")
	}
}

func TestObjdumpSkipsIndirect(t *testing.T) {
	binary := filepath.Join(repoRoot(t), "testdata", "fixture", "builddir", "app1", "app1")
	edges, err := linkplane.RunObjdump(binary)
	if err != nil {
		t.Fatalf("RunObjdump: %v", err)
	}
	// g_ops isn't a function; it's a data ref. An edge `main → g_ops`
	// would mean we misparsed the indirect `call *...(%rip) # <g_ops>`
	// as a direct call.
	for _, e := range edges {
		if e.Caller == "main" && e.CalleeStripped() == "g_ops" {
			t.Errorf("misclassified indirect call as edge: %+v", e)
		}
	}
}
