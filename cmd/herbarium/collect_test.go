package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// TestCollectMultipleTargets verifies that --target may be repeated,
// with each occurrence contributing to the kept set. Mixing a
// comma-separated entry with a bare entry exercises both code paths in
// filterTargets.
func TestCollectMultipleTargets(t *testing.T) {
	repo := repoRoot(t)
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")
	out := filepath.Join(t.TempDir(), "app12.hbr")

	if code := runCollect([]string{
		"--builddir", bdir,
		"--project-root", proot,
		"--out", out,
		"--target", "app1",
		"--target", "app2,shared",
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
	want := []string{"app1", "app2", "shared"}
	if len(names) != len(want) {
		t.Fatalf("targets = %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("targets[%d] = %q, want %q", i, names[i], n)
		}
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

// TestCollectReplaceOverwrites covers the agent loop: re-collect over
// the .hbr a serve process is already holding. The second run must
// succeed and leave a valid index behind, with the temp-then-rename
// detour leaving no scratch file behind. (The -wal/-shm sidecars beside
// it are not ours — a WAL-mode database gets them recreated by every
// reader, including serve --check below.)
func TestCollectReplaceOverwrites(t *testing.T) {
	repo := repoRoot(t)
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")
	dir := t.TempDir()
	out := filepath.Join(dir, "replace.hbr")

	args := []string{"--builddir", bdir, "--project-root", proot, "--out", out}
	if code := runCollect(args); code != 0 {
		t.Fatalf("first runCollect exit code = %d, want 0", code)
	}
	if code := runCollect(append(args, "--replace")); code != 0 {
		t.Fatalf("runCollect --replace exit code = %d, want 0", code)
	}
	if code := runServe([]string{"--hbr", out, "--check"}); code != 0 {
		t.Errorf("runServe --check after --replace exit code = %d, want 0", code)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".replace.hbr.tmp-") {
			t.Errorf("scratch index left behind: %s", e.Name())
		}
	}
}

// TestCollectFailureLeavesNoIndex pins the other half of the
// temp-then-rename contract: a collect that dies mid-pipeline must not
// leave a stub at --out for a later --replace-less run to trip over, or
// for a serve to open and answer from.
func TestCollectFailureLeavesNoIndex(t *testing.T) {
	repo := repoRoot(t)
	proot := filepath.Join(repo, "testdata", "fixture")
	out := filepath.Join(t.TempDir(), "doomed.hbr")

	if code := runCollect([]string{
		"--builddir", filepath.Join(repo, "testdata", "fixture", "builddir"),
		"--project-root", proot,
		"--out", out,
		"--target", "no_such_target",
	}); code == 0 {
		t.Fatal("runCollect with an unknown target returned 0")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) = %v, want IsNotExist", out, err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}
