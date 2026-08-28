package mesonintrospect_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yerden/herbarium/internal/mesonintrospect"
)

// fixtureBuilddir resolves testdata/fixture/builddir relative to the repo
// root, so the test works no matter where `go test` is invoked from.
func fixtureBuilddir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	// this file: <repo>/internal/mesonintrospect/mesonintrospect_test.go
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(repo, "testdata", "fixture", "builddir")
}

func TestLoadFixture(t *testing.T) {
	intro, err := mesonintrospect.Load(fixtureBuilddir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if intro.MesonVersion == "" {
		t.Error("MesonVersion empty")
	}
	if intro.CCompiler.ID != "gcc" {
		t.Errorf("CCompiler.ID = %q, want gcc", intro.CCompiler.ID)
	}
	if intro.CCompiler.Version == "" {
		t.Error("CCompiler.Version empty")
	}

	// Fixture defines 3 targets: shared (lib), app1, app2.
	byName := map[string]mesonintrospect.Target{}
	for _, tgt := range intro.Targets {
		byName[tgt.Name] = tgt
	}

	shared, ok := byName["shared"]
	if !ok {
		t.Fatalf("target 'shared' missing; got: %v", targetNames(intro.Targets))
	}
	if shared.Kind != "static_library" {
		t.Errorf("shared.Kind = %q, want static_library", shared.Kind)
	}
	if len(shared.Sources) != 4 {
		t.Errorf("shared.Sources = %v, want 4 entries", shared.Sources)
	}

	app1, ok := byName["app1"]
	if !ok {
		t.Fatalf("target 'app1' missing")
	}
	if app1.Kind != "executable" {
		t.Errorf("app1.Kind = %q, want executable", app1.Kind)
	}
	if len(app1.Sources) != 2 {
		t.Errorf("app1.Sources = %v, want 2 entries (main.c + strong_override.c)", app1.Sources)
	}
	// app1 was built with the dump flags — verify one of them shows up
	// in the compile parameters (preflight will do the real check).
	if !containsPrefix(app1.CompileParams, "-fdump-ipa-cgraph") {
		t.Errorf("app1.CompileParams missing -fdump-ipa-cgraph: %v", app1.CompileParams)
	}

	app2, ok := byName["app2"]
	if !ok {
		t.Fatalf("target 'app2' missing")
	}
	if len(app2.Sources) != 1 {
		t.Errorf("app2.Sources = %v, want 1 entry (main.c)", app2.Sources)
	}
}

func TestLoadMissingBuilddir(t *testing.T) {
	_, err := mesonintrospect.Load(t.TempDir())
	if err == nil {
		t.Fatal("expected error for empty tmp dir, got nil")
	}
}

func targetNames(ts []mesonintrospect.Target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

func containsPrefix(ss []string, prefix string) bool {
	for _, s := range ss {
		if s == prefix || (len(s) > len(prefix) && s[:len(prefix)] == prefix) {
			return true
		}
	}
	return false
}
