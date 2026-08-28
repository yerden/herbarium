package linkplane_test

import (
	"path/filepath"
	"testing"

	"github.com/yerden/herbarium/internal/linkplane"
)

func TestReadMapApp1(t *testing.T) {
	mp := filepath.Join(repoRoot(t), "testdata", "fixture", "builddir", "app1.map")
	mf, err := linkplane.ReadMap(mp)
	if err != nil {
		t.Fatalf("ReadMap: %v", err)
	}
	// hook won by app1/app1.p/strong_override.c.o (strong override).
	if got := mf.SymbolOrigin["hook"]; got != "app1/app1.p/strong_override.c.o" {
		t.Errorf("app1 hook winning_object = %q, want app1/app1.p/strong_override.c.o", got)
	}
	// add_ints/mul_ints/compute all come from
	// lib/libshared.a.p/shared_utils.c.o. never_called is dead-stripped
	// via -Wl,--gc-sections so ld emits its section but no symbol
	// assignment — SymbolOrigin correctly leaves it unmapped.
	for _, name := range []string{"add_ints", "mul_ints", "compute"} {
		want := "lib/libshared.a.p/shared_utils.c.o"
		if got := mf.SymbolOrigin[name]; got != want {
			t.Errorf("app1 %s winning_object = %q, want %q", name, got, want)
		}
	}
	if got, ok := mf.SymbolOrigin["never_called"]; ok {
		t.Errorf("app1 never_called winning_object = %q, want unset (dead-stripped)", got)
	}
	// main comes from app1/app1.p/main.c.o.
	if got := mf.SymbolOrigin["main"]; got != "app1/app1.p/main.c.o" {
		t.Errorf("app1 main winning_object = %q", got)
	}

	// Archive pulls: shared_utils.c.o pulled by main.c.o for `compute`,
	// dispatch_impls.c.o pulled by main.c.o for `g_ops`. weak_impl.c.o
	// must NOT be pulled — the strong override satisfied `hook` already.
	pulledObjs := map[string]bool{}
	for _, p := range mf.ArchivePulls {
		pulledObjs[p.Object] = true
	}
	if pulledObjs["lib/libshared.a.p/weak_impl.c.o"] {
		t.Error("app1: weak_impl.c.o unexpectedly pulled — strong override should have satisfied hook")
	}
	if !pulledObjs["lib/libshared.a.p/shared_utils.c.o"] {
		t.Error("app1: shared_utils.c.o not in archive pulls")
	}
}

func TestReadMapApp2(t *testing.T) {
	mp := filepath.Join(repoRoot(t), "testdata", "fixture", "builddir", "app2.map")
	mf, err := linkplane.ReadMap(mp)
	if err != nil {
		t.Fatalf("ReadMap: %v", err)
	}
	// hook resolves to the weak def in lib/libshared.a.p/weak_impl.c.o.
	if got := mf.SymbolOrigin["hook"]; got != "lib/libshared.a.p/weak_impl.c.o" {
		t.Errorf("app2 hook winning_object = %q, want lib/libshared.a.p/weak_impl.c.o", got)
	}

	// The load list should show weak_impl.c.o was pulled by main.c.o
	// for the `hook` reference.
	var found bool
	for _, p := range mf.ArchivePulls {
		if p.Object == "lib/libshared.a.p/weak_impl.c.o" && p.Symbol == "hook" {
			found = true
		}
	}
	if !found {
		t.Errorf("app2: no archive pull for hook via weak_impl.c.o; got %v", mf.ArchivePulls)
	}
}

func TestArchiveFor(t *testing.T) {
	cases := map[string]string{
		"lib/libshared.a.p/weak_impl.c.o":       "lib/libshared.a",
		"lib/libshared.a.p/shared_utils.c.o":    "lib/libshared.a",
		"app1/app1.p/main.c.o":                  "",
		"app1/app1.p/strong_override.c.o":       "",
		"/usr/lib/.../crtn.o":                   "",
	}
	for path, want := range cases {
		if got := linkplane.ArchiveFor(path); got != want {
			t.Errorf("ArchiveFor(%q) = %q, want %q", path, got, want)
		}
	}
}
