package gccdump_test

import (
	"path/filepath"
	"testing"

	"github.com/yerden/herbarium/internal/gccdump"
)

func TestParseCgraphApp1Main(t *testing.T) {
	cg, err := gccdump.ParseCgraphFile(filepath.Join(
		repoRoot(t), "testdata", "samples", "gcc-16", "app1", "main.c.c.000i.cgraph",
	))
	if err != nil {
		t.Fatalf("ParseCgraphFile: %v", err)
	}

	// TrivialNeeded should include main.
	if !containsAny(cg.TrivialNeeded, []string{"5"}) {
		t.Errorf("TrivialNeeded missing main's id 5: %v", cg.TrivialNeeded)
	}

	// Expected symbols by name (looked up by iterating the map).
	want := map[string]struct {
		kind         string
		hasBody      bool // Analyzed=true and !BodyRemoved
		callsInto    []string
		hasIndirects int
		isClone      bool
		parentID     string
	}{
		"main":                     {kind: "function", hasBody: true, callsInto: []string{"printf", "hook", "compute", "use_dispatch.constprop.0"}, hasIndirects: 0},
		"use_dispatch.constprop.0": {kind: "function", hasBody: true, hasIndirects: 2, isClone: true, parentID: "4"},
		"use_dispatch":             {kind: "function", hasBody: false /* body removed in optimized */},
		"g_ops":                    {kind: "variable"},
		"printf":                   {kind: "function"},
	}
	byName := map[string]*gccdump.Function{}
	for _, fn := range cg.Symbols {
		byName[fn.Name] = fn
	}
	for name, w := range want {
		got, ok := byName[name]
		if !ok {
			t.Errorf("missing symbol %s", name)
			continue
		}
		if got.Kind != w.kind {
			t.Errorf("%s.Kind = %q, want %q", name, got.Kind, w.kind)
		}
		if w.isClone {
			if got.CloneOfID != w.parentID {
				t.Errorf("%s.CloneOfID = %q, want %q", name, got.CloneOfID, w.parentID)
			}
		}
		if w.hasIndirects > 0 && len(got.IndirectSites) != w.hasIndirects {
			t.Errorf("%s.IndirectSites = %d, want %d", name, len(got.IndirectSites), w.hasIndirects)
		}
		for _, expected := range w.callsInto {
			if !containsCallToName(got.Called, cg.Symbols, expected) {
				t.Errorf("%s missing call to %s (called ids=%v)", name, expected, callTargetIDs(got.Called))
			}
		}
	}

	// main.Called should include the clone with Inlined=true.
	main := byName["main"]
	var foundInlined bool
	for _, c := range main.Called {
		if fn, ok := cg.Symbols[c.TargetLocalID]; ok && fn.LinkageName == "use_dispatch.constprop" && c.Inlined {
			foundInlined = true
		}
	}
	if !foundInlined {
		t.Errorf("expected main → use_dispatch.constprop tagged (inlined); calls=%+v", main.Called)
	}
}

func TestParseCgraphSharedUtils(t *testing.T) {
	cg, err := gccdump.ParseCgraphFile(filepath.Join(
		repoRoot(t), "testdata", "fixture", "builddir",
		"lib", "libshared.a.p", "shared_utils.c.c.000i.cgraph",
	))
	if err != nil {
		t.Fatalf("ParseCgraphFile: %v", err)
	}
	byName := map[string]*gccdump.Function{}
	for _, fn := range cg.Symbols {
		byName[fn.Name] = fn
	}
	// compute must call add_ints + mul_ints.
	compute, ok := byName["compute"]
	if !ok {
		t.Fatalf("missing compute")
	}
	if !containsCallToName(compute.Called, cg.Symbols, "add_ints") || !containsCallToName(compute.Called, cg.Symbols, "mul_ints") {
		t.Errorf("compute.Called missing expected targets; got=%v", callTargetIDs(compute.Called))
	}
	// never_called must be present but not called.
	nc, ok := byName["never_called"]
	if !ok {
		t.Fatalf("missing never_called")
	}
	if len(nc.Called) != 0 {
		t.Errorf("never_called.Called = %v, want empty", nc.Called)
	}
	// Address-taken carries in Referring from g_ops.
	// (dispatch_impls.c.cgraph would confirm this — checked below.)
}

func TestParseCgraphDispatchImpls(t *testing.T) {
	cg, err := gccdump.ParseCgraphFile(filepath.Join(
		repoRoot(t), "testdata", "samples", "gcc-16",
		"lib", "dispatch_impls.c.c.000i.cgraph",
	))
	if err != nil {
		t.Fatalf("ParseCgraphFile: %v", err)
	}
	byName := map[string]*gccdump.Function{}
	for _, fn := range cg.Symbols {
		byName[fn.Name] = fn
	}
	// add_ints and mul_ints must be marked address-taken (g_ops holds
	// their addresses).
	for _, name := range []string{"add_ints", "mul_ints"} {
		fn, ok := byName[name]
		if !ok {
			t.Errorf("missing %s", name)
			continue
		}
		if !fn.AddressTaken {
			t.Errorf("%s not marked address-taken", name)
		}
	}
	// g_ops references add_ints and mul_ints via addr.
	gOps, ok := byName["g_ops"]
	if !ok {
		t.Fatal("missing g_ops")
	}
	if gOps.Kind != "variable" {
		t.Errorf("g_ops.Kind = %q, want variable", gOps.Kind)
	}
	var addrRefs int
	for _, r := range gOps.Refs {
		if r.Kind == "addr" {
			addrRefs++
		}
	}
	if addrRefs != 2 {
		t.Errorf("g_ops addr refs = %d, want 2", addrRefs)
	}
}

func TestParseCgraphWeakLinkage(t *testing.T) {
	cg, err := gccdump.ParseCgraphFile(filepath.Join(
		repoRoot(t), "testdata", "fixture", "builddir",
		"lib", "libshared.a.p", "weak_impl.c.c.000i.cgraph",
	))
	if err != nil {
		t.Fatalf("ParseCgraphFile: %v", err)
	}
	byName := map[string]*gccdump.Function{}
	for _, fn := range cg.Symbols {
		byName[fn.Name] = fn
	}
	hook, ok := byName["hook"]
	if !ok {
		t.Fatal("missing hook")
	}
	// weak must appear in Visibility flags for the weak_impl.c definition.
	var isWeak bool
	for _, v := range hook.VisibilityFlags {
		if v == "weak" {
			isWeak = true
		}
	}
	if !isWeak {
		t.Errorf("hook not marked weak: VisibilityFlags=%v", hook.VisibilityFlags)
	}
}

func containsAny(hay []string, needles []string) bool {
	set := make(map[string]struct{}, len(hay))
	for _, s := range hay {
		set[s] = struct{}{}
	}
	for _, n := range needles {
		if _, ok := set[n]; ok {
			return true
		}
	}
	return false
}

func containsCallToName(calls []gccdump.Call, byID map[string]*gccdump.Function, targetName string) bool {
	for _, c := range calls {
		fn, ok := byID[c.TargetLocalID]
		if !ok {
			// byID is name-keyed above; skip name→fn resolution for this path.
			continue
		}
		if fn.Name == targetName {
			return true
		}
	}
	return false
}

func callTargetIDs(calls []gccdump.Call) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.TargetLocalID
		if c.Inlined {
			out[i] += "(inlined)"
		}
	}
	return out
}
