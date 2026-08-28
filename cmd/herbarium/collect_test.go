package main

import (
	"database/sql"
	"path/filepath"
	"runtime"
	"testing"

	_ "modernc.org/sqlite"
)

// TestCollectSmoke runs the collect subcommand against the fixture
// builddir end-to-end and verifies the produced .hbr is opened by
// serve and has the expected meta stamps. Kept in cmd/ so a regression
// in wiring (missing dep, subcommand not registered) fails the build.
func TestCollectSmoke(t *testing.T) {
	repo := repoRoot(t)
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")
	out := filepath.Join(t.TempDir(), "smoke.hbr")

	if code := runCollect([]string{
		"--builddir", bdir,
		"--project-root", proot,
		"--out", out,
	}); code != 0 {
		t.Fatalf("runCollect exit code = %d, want 0", code)
	}

	if code := runServe([]string{"--hbr", out, "--check"}); code != 0 {
		t.Errorf("runServe --check exit code = %d, want 0", code)
	}

	// Meta stamps herbarium collect writes must be present.
	db, err := sql.Open("sqlite", "file:"+out+"?mode=ro")
	if err != nil {
		t.Fatalf("open produced db: %v", err)
	}
	defer db.Close()

	wantKeys := []string{"schema_version", "gcc_version", "meson_version", "indexed_at", "project_root_hint", "herbarium_version"}
	for _, k := range wantKeys {
		var v string
		if err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, k).Scan(&v); err != nil {
			t.Errorf("meta[%q]: %v", k, err)
			continue
		}
		if v == "" {
			t.Errorf("meta[%q] empty", k)
		}
	}
}

// TestCollectTargetFilter verifies --target scopes link-plane ingest
// to the requested target only. The full fixture has three targets
// (app1, app2, shared); after --target=app1 the `targets` table must
// have exactly one row.
func TestCollectTargetFilter(t *testing.T) {
	repo := repoRoot(t)
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")
	out := filepath.Join(t.TempDir(), "app1.hbr")

	if code := runCollect([]string{
		"--builddir", bdir,
		"--project-root", proot,
		"--out", out,
		"--target", "app1",
	}); code != 0 {
		t.Fatalf("runCollect exit code = %d, want 0", code)
	}

	db, err := sql.Open("sqlite", "file:"+out+"?mode=ro")
	if err != nil {
		t.Fatalf("open produced db: %v", err)
	}
	defer db.Close()

	var names []string
	rows, err := db.Query(`SELECT name FROM targets ORDER BY name`)
	if err != nil {
		t.Fatalf("query targets: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(names) != 1 || names[0] != "app1" {
		t.Errorf("targets after --target=app1 = %v, want [app1]", names)
	}
}

// TestCollectUnknownTarget makes the failure loud when the user typos
// a target name — silently indexing nothing would be worse.
func TestCollectUnknownTarget(t *testing.T) {
	repo := repoRoot(t)
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")
	out := filepath.Join(t.TempDir(), "smoke.hbr")

	if code := runCollect([]string{
		"--builddir", bdir,
		"--project-root", proot,
		"--out", out,
		"--target", "no_such_target",
	}); code == 0 {
		t.Error("runCollect with unknown --target returned 0, want non-zero")
	}
}

// TestCollectRefusesExistingOutput guards the plan's requirement that we
// don't silently overwrite an existing .hbr (incremental re-ingest is a
// distinct code path landing in Phase 7).
func TestCollectRefusesExistingOutput(t *testing.T) {
	repo := repoRoot(t)
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")
	out := filepath.Join(t.TempDir(), "smoke.hbr")

	if code := runCollect([]string{"--builddir", bdir, "--project-root", proot, "--out", out}); code != 0 {
		t.Fatalf("first runCollect exit code = %d, want 0", code)
	}
	if code := runCollect([]string{"--builddir", bdir, "--project-root", proot, "--out", out}); code == 0 {
		t.Error("second runCollect returned 0, want non-zero (should refuse to clobber)")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}
