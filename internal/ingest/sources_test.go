package ingest_test

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/yerden/herbarium/internal/builddir"
	"github.com/yerden/herbarium/internal/ingest"
	"github.com/yerden/herbarium/internal/mesonintrospect"
	"github.com/yerden/herbarium/internal/store"
)

// TestSourcesAgainstFixture packs the fixture's sources end-to-end and
// verifies the shape: every C source and header the fixture actually
// #includes is present, system headers are filtered out, and blobs
// round-trip content exactly.
func TestSourcesAgainstFixture(t *testing.T) {
	repo := repoRoot(t)
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")

	db := freshDB(t)
	pr := mustResolver(t, bdir, proot)
	sum, err := ingest.Sources(db, mustCrawl(t, bdir), mustIntrospect(t, bdir), pr, ingest.SourcesOptions{})
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}

	// The fixture has 6 TUs and 2 in-tree headers (include/dispatch.h,
	// lib/shared_utils.h). System headers must be filtered out.
	wantPaths := []string{
		"app1/main.c",
		"app1/strong_override.c",
		"app2/main.c",
		"include/dispatch.h",
		"lib/dispatch_impls.c",
		"lib/icf_pair.c",
		"lib/shared_utils.c",
		"lib/shared_utils.h",
		"lib/weak_impl.c",
	}
	if sum.Files != len(wantPaths) {
		t.Errorf("Files = %d, want %d", sum.Files, len(wantPaths))
	}
	if sum.Generated != 0 {
		t.Errorf("Generated = %d, want 0 (fixture has no generated sources)", sum.Generated)
	}
	if sum.Duplicates != 0 {
		t.Errorf("Duplicates = %d, want 0 (all files have unique content)", sum.Duplicates)
	}

	got := listSources(t, db)
	if !equalSorted(got, wantPaths) {
		t.Errorf("packed paths mismatch\n got: %v\nwant: %v", got, wantPaths)
	}

	// System headers must not appear.
	for _, p := range got {
		if strings.HasPrefix(p, "/") {
			t.Errorf("unexpected absolute path in sources: %q", p)
		}
	}

	// Round-trip: content stored under app1/main.c must decompress back
	// to the on-disk file byte-for-byte.
	want, err := os.ReadFile(filepath.Join(proot, "app1", "main.c"))
	if err != nil {
		t.Fatalf("read fixture app1/main.c: %v", err)
	}
	if got := fetchBlob(t, db, "app1/main.c"); !bytes.Equal(got, want) {
		t.Errorf("app1/main.c round-trip mismatch (got %d bytes, want %d)", len(got), len(want))
	}
}

// TestSourcesRefusesMissingSource covers the appendix-line-584 rule:
// if Meson names a source that is not on disk under --project-root,
// packing must fail loudly rather than silently produce a hollow index.
//
// The fixture's real Meson JSON records absolute paths under the real
// checkout, so we can't test this by copying the checkout to a fresh
// tempdir. We synthesize a minimal Introspection whose source list
// points at a file we control, then delete that file.
func TestSourcesRefusesMissingSource(t *testing.T) {
	proot := t.TempDir()
	// Create the parent dir but NOT the file — Meson listed it, Sources
	// must complain.
	if err := os.MkdirAll(filepath.Join(proot, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	intro := &mesonintrospect.Introspection{
		Targets: []mesonintrospect.Target{{
			Name:    "app",
			Kind:    "executable",
			Sources: []string{filepath.Join(proot, "src", "missing.c")},
		}},
	}

	db := freshDB(t)
	pr := mustResolver(t, filepath.Join(proot, "builddir"), proot)
	bd := &builddir.BuildDir{Root: filepath.Join(proot, "builddir")}
	_, err := ingest.Sources(db, bd, intro, pr, ingest.SourcesOptions{})
	if err == nil {
		t.Fatal("Sources succeeded; want error naming missing src/missing.c")
	}
	if !strings.Contains(err.Error(), "src/missing.c") {
		t.Errorf("error does not name missing file: %s", err.Error())
	}
}

// TestSourcesStrictMtime covers herbarium-plan.md line 539: --strict
// refuses to pack a source whose mtime is newer than any consuming .o.
// Uses the real fixture (strict looks up per-TU consumers via
// .ninja_deps, which the fixture has) and touches one source's mtime to
// the future; a t.Cleanup restores it so parallel tests aren't affected.
func TestSourcesStrictMtime(t *testing.T) {
	repo := repoRoot(t)
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")

	target := filepath.Join(proot, "app1", "main.c")
	origInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat %s: %v", target, err)
	}
	origMtime := origInfo.ModTime()
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(target, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chtimes(target, origMtime, origMtime)
	})

	db := freshDB(t)
	pr := mustResolver(t, bdir, proot)
	_, err = ingest.Sources(db, mustCrawl(t, bdir), mustIntrospect(t, bdir), pr, ingest.SourcesOptions{Strict: true})
	if err == nil {
		t.Fatal("Sources succeeded with --strict and future-dated source; want error")
	}
	if !strings.Contains(err.Error(), "strict") {
		t.Errorf("strict error message missing 'strict' tag: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "app1/main.c") {
		t.Errorf("strict error does not name offending source: %s", err.Error())
	}

	// Same setup without --strict must succeed — strict is opt-in.
	db2 := freshDB(t)
	pr2 := mustResolver(t, bdir, proot)
	if _, err := ingest.Sources(db2, mustCrawl(t, bdir), mustIntrospect(t, bdir), pr2, ingest.SourcesOptions{}); err != nil {
		t.Errorf("non-strict Sources failed on future-dated source: %v", err)
	}
}

// --- helpers --------------------------------------------------------------

func freshDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.hbr")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Init(db); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	return db
}

func mustResolver(t *testing.T, bdir, proot string) *ingest.PathResolver {
	t.Helper()
	pr, err := ingest.NewPathResolver(bdir, proot)
	if err != nil {
		t.Fatalf("NewPathResolver: %v", err)
	}
	return pr
}

func mustIntrospect(t *testing.T, bdir string) *mesonintrospect.Introspection {
	t.Helper()
	intro, err := mesonintrospect.Load(bdir)
	if err != nil {
		t.Fatalf("mesonintrospect.Load: %v", err)
	}
	return intro
}

func mustCrawl(t *testing.T, bdir string) *builddir.BuildDir {
	t.Helper()
	bd, err := builddir.Crawl(bdir)
	if err != nil {
		t.Fatalf("builddir.Crawl: %v", err)
	}
	return bd
}

func listSources(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT path FROM sources ORDER BY path`)
	if err != nil {
		t.Fatalf("query sources: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return out
}

func fetchBlob(t *testing.T, db *sql.DB, path string) []byte {
	t.Helper()
	var compressed []byte
	if err := db.QueryRow(
		`SELECT b.content FROM blobs b JOIN sources s ON s.blob_hash = b.hash WHERE s.path = ?`,
		path,
	).Scan(&compressed); err != nil {
		t.Fatalf("fetch blob %q: %v", path, err)
	}
	dec, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer dec.Close()
	raw, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("zstd read: %v", err)
	}
	return raw
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// TestSourcesGeneratedOutOfTree covers the failure the user reported:
// with builddir OUTSIDE --project-root, t.Generated entries (typical for
// configure_file() output) must land in generated_sources instead of
// aborting ingest or being silently skipped. Uses a hand-built minimal
// project so we can control the layout directly — the shipped fixture
// puts builddir inside project-root, which never exercises this branch.
func TestSourcesGeneratedOutOfTree(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "project")
	bdir := filepath.Join(root, "build")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One project source and one generated header living under builddir.
	if err := os.WriteFile(filepath.Join(proj, "main.c"), []byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	confAbs := filepath.Join(bdir, "config.h")
	confBody := []byte("#define GENERATED 1\n")
	if err := os.WriteFile(confAbs, confBody, 0o644); err != nil {
		t.Fatal(err)
	}

	intro := &mesonintrospect.Introspection{
		Targets: []mesonintrospect.Target{{
			Name:      "app",
			Kind:      "executable",
			Sources:   []string{filepath.Join(proj, "main.c")},
			Generated: []string{confAbs},
		}},
	}
	bd := &builddir.BuildDir{Root: bdir}

	db := freshDB(t)
	pr, err := ingest.NewPathResolver(bdir, proj)
	if err != nil {
		t.Fatalf("NewPathResolver: %v", err)
	}
	sum, err := ingest.Sources(db, bd, intro, pr, ingest.SourcesOptions{})
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}

	// main.c goes into sources (project-relative key).
	if got := listSources(t, db); !equalSorted(got, []string{"main.c"}) {
		t.Errorf("sources = %v, want [main.c]", got)
	}
	// config.h goes into generated_sources with builddir-relative key.
	if sum.GeneratedFiles != 1 {
		t.Errorf("sum.GeneratedFiles = %d, want 1", sum.GeneratedFiles)
	}
	var genKey string
	var genBody []byte
	if err := db.QueryRow(`
		SELECT gs.builddir_rel, b.content
		FROM generated_sources gs JOIN blobs b ON b.hash = gs.blob_hash
	`).Scan(&genKey, &genBody); err != nil {
		t.Fatalf("query generated: %v", err)
	}
	if genKey != "config.h" {
		t.Errorf("generated key = %q, want config.h", genKey)
	}
	dec, err := zstd.NewReader(bytes.NewReader(genBody))
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	defer dec.Close()
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("zstd read: %v", err)
	}
	if !bytes.Equal(got, confBody) {
		t.Errorf("generated content mismatch:\n  got:  %q\n  want: %q", got, confBody)
	}
}

// TestSourcesGeneratedOutsideBoth: a t.Generated entry that lives outside
// both --project-root AND builddir is a genuinely broken introspection —
// hard-error rather than silently dropping it.
func TestSourcesGeneratedOutsideBoth(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "project")
	bdir := filepath.Join(root, "build")
	stray := filepath.Join(root, "stray", "orphan.h")
	for _, d := range []string{proj, bdir, filepath.Dir(stray)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(proj, "main.c"), []byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte("// orphan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	intro := &mesonintrospect.Introspection{
		Targets: []mesonintrospect.Target{{
			Name:      "app",
			Kind:      "executable",
			Sources:   []string{filepath.Join(proj, "main.c")},
			Generated: []string{stray},
		}},
	}
	bd := &builddir.BuildDir{Root: bdir}
	db := freshDB(t)
	pr, err := ingest.NewPathResolver(bdir, proj)
	if err != nil {
		t.Fatalf("NewPathResolver: %v", err)
	}
	_, err = ingest.Sources(db, bd, intro, pr, ingest.SourcesOptions{})
	if err == nil {
		t.Fatalf("expected error for generated file outside both roots; got nil")
	}
	if !strings.Contains(err.Error(), "outside both") {
		t.Errorf("expected 'outside both' error; got: %v", err)
	}
}

// TestSourcesExternalGlob verifies that --include-external '/usr/include/**'
// packs system headers transitively pulled in by the fixture (via <stdio.h>)
// into external_sources rather than skipping them. Requires /usr/include/stdio.h
// on the host — a linux distro invariant that CI already relies on.
func TestSourcesExternalGlob(t *testing.T) {
	if _, err := os.Stat("/usr/include/stdio.h"); err != nil {
		t.Skip("host lacks /usr/include/stdio.h; skipping external-glob test")
	}
	repo := repoRoot(t)
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")

	db := freshDB(t)
	pr := mustResolver(t, bdir, proot)
	glob, err := ingest.NewExternalGlob("/usr/include/**")
	if err != nil {
		t.Fatalf("NewExternalGlob: %v", err)
	}
	sum, err := ingest.Sources(db, mustCrawl(t, bdir), mustIntrospect(t, bdir), pr,
		ingest.SourcesOptions{ExternalGlobs: []ingest.ExternalGlob{glob}})
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if sum.ExternalFiles == 0 {
		t.Fatalf("expected external_sources rows; got 0")
	}

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM external_sources WHERE abs_path = '/usr/include/stdio.h'`,
	).Scan(&n); err != nil {
		t.Fatalf("query external_sources: %v", err)
	}
	if n != 1 {
		t.Errorf("expected /usr/include/stdio.h in external_sources; got %d rows", n)
	}

	// Blob content of the packed header must byte-match the on-disk file.
	live, err := os.ReadFile("/usr/include/stdio.h")
	if err != nil {
		t.Fatalf("read live stdio.h: %v", err)
	}
	packed := fetchExternalBlob(t, db, "/usr/include/stdio.h")
	if !bytes.Equal(packed, live) {
		t.Errorf("packed /usr/include/stdio.h differs from on-disk copy (%d vs %d bytes)", len(packed), len(live))
	}
}

// TestSourcesExternalGlobZeroMatch: a glob that matches nothing is a hard
// error, on the same rationale as an unknown --target name.
func TestSourcesExternalGlobZeroMatch(t *testing.T) {
	repo := repoRoot(t)
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")

	db := freshDB(t)
	pr := mustResolver(t, bdir, proot)
	glob, err := ingest.NewExternalGlob("/definitely/not/a/real/path/**")
	if err != nil {
		t.Fatalf("NewExternalGlob: %v", err)
	}
	_, err = ingest.Sources(db, mustCrawl(t, bdir), mustIntrospect(t, bdir), pr,
		ingest.SourcesOptions{ExternalGlobs: []ingest.ExternalGlob{glob}})
	if err == nil {
		t.Fatalf("expected zero-match glob to error; got nil")
	}
	if !strings.Contains(err.Error(), "matched zero headers") {
		t.Errorf("expected 'matched zero headers' error, got: %v", err)
	}
}

func fetchExternalBlob(t *testing.T, db *sql.DB, absPath string) []byte {
	t.Helper()
	var compressed []byte
	if err := db.QueryRow(
		`SELECT b.content FROM blobs b JOIN external_sources es ON es.blob_hash = b.hash WHERE es.abs_path = ?`,
		absPath,
	).Scan(&compressed); err != nil {
		t.Fatalf("fetch external blob %q: %v", absPath, err)
	}
	dec, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer dec.Close()
	raw, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("zstd read: %v", err)
	}
	return raw
}
