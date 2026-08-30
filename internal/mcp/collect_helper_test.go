package mcp_test

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/yerden/herbarium/internal/builddir"
	"github.com/yerden/herbarium/internal/ingest"
	"github.com/yerden/herbarium/internal/mesonintrospect"
	"github.com/yerden/herbarium/internal/preflight"
	"github.com/yerden/herbarium/internal/store"
)

// collectForTest drives a full collect pipeline against a fixture
// builddir and writes an .hbr at out. Duplicates the orchestration in
// cmd/herbarium/collect.go so mcp tests don't need to shell out to the
// binary. Returns a nonzero exit-code-style int on any failure so the
// caller can t.Fatalf uniformly.
func collectForTest(bdir, proot, out string) int {
	intro, err := mesonintrospect.Load(bdir)
	if err != nil {
		return 1
	}
	bd, err := builddir.Crawl(bdir)
	if err != nil {
		return 1
	}
	if !preflight.Check(intro, bd).Ok {
		return 1
	}
	db, err := store.Open(out)
	if err != nil {
		return 1
	}
	defer db.Close()
	if err := store.Init(db); err != nil {
		return 1
	}
	stamps := [][2]string{
		{"herbarium_version", "test"},
		{"gcc_version", intro.CCompiler.Version},
		{"meson_version", intro.MesonVersion},
		{"indexed_at", time.Now().UTC().Format(time.RFC3339)},
		{"project_root_hint", proot},
	}
	for _, kv := range stamps {
		if _, err := db.Exec(
			`INSERT INTO meta(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv[0], kv[1],
		); err != nil {
			return 1
		}
	}
	pr, err := ingest.NewPathResolver(bdir, proot)
	if err != nil {
		return 1
	}
	sum, err := ingest.Compiler(db, bd, pr)
	if err != nil {
		return 1
	}
	if _, err := ingest.DWARF(db, bd, pr, sum.IDByUSR); err != nil {
		return 1
	}
	tids, err := ingest.Targets(db, intro, pr)
	if err != nil {
		return 1
	}
	if _, err := ingest.Link(db, bd, intro, pr, tids, sum.ObjectToSource); err != nil {
		return 1
	}
	if _, err := ingest.Sources(db, bd, intro, pr, ingest.SourcesOptions{}); err != nil {
		return 1
	}
	return 0
}

// collectForTestWithGlobs is collectForTest plus --include-external globs.
// Kept as a separate function so the plain collectForTest signature (used
// by every existing MCP test) stays unchanged.
func collectForTestWithGlobs(bdir, proot, out string, globs []string) error {
	intro, err := mesonintrospect.Load(bdir)
	if err != nil {
		return err
	}
	bd, err := builddir.Crawl(bdir)
	if err != nil {
		return err
	}
	if !preflight.Check(intro, bd).Ok {
		return errPreflight
	}
	db, err := store.Open(out)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Init(db); err != nil {
		return err
	}
	stamps := [][2]string{
		{"herbarium_version", "test"},
		{"gcc_version", intro.CCompiler.Version},
		{"meson_version", intro.MesonVersion},
		{"indexed_at", time.Now().UTC().Format(time.RFC3339)},
		{"project_root_hint", proot},
	}
	for _, kv := range stamps {
		if _, err := db.Exec(
			`INSERT INTO meta(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv[0], kv[1],
		); err != nil {
			return err
		}
	}
	pr, err := ingest.NewPathResolver(bdir, proot)
	if err != nil {
		return err
	}
	sum, err := ingest.Compiler(db, bd, pr)
	if err != nil {
		return err
	}
	if _, err := ingest.DWARF(db, bd, pr, sum.IDByUSR); err != nil {
		return err
	}
	tids, err := ingest.Targets(db, intro, pr)
	if err != nil {
		return err
	}
	if _, err := ingest.Link(db, bd, intro, pr, tids, sum.ObjectToSource); err != nil {
		return err
	}
	parsed := make([]ingest.ExternalGlob, 0, len(globs))
	for _, raw := range globs {
		g, err := ingest.NewExternalGlob(raw)
		if err != nil {
			return err
		}
		parsed = append(parsed, g)
	}
	if _, err := ingest.Sources(db, bd, intro, pr, ingest.SourcesOptions{ExternalGlobs: parsed}); err != nil {
		return err
	}
	return nil
}

var errPreflight = errors.New("collectForTest: preflight failed")

// runSyntheticSourcesIngest runs only the Sources pass against a
// hand-built out-of-tree layout — bypasses Meson introspection and the
// compiler/link passes entirely. Used by the generated_sources
// fall-through tests where we need control over builddir vs. project-root
// geometry that the shipped fixture doesn't provide.
func runSyntheticSourcesIngest(bdir, proot, out string) error {
	db, err := store.Open(out)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Init(db); err != nil {
		return err
	}
	pr, err := ingest.NewPathResolver(bdir, proot)
	if err != nil {
		return err
	}
	// Single target with one project source + one out-of-tree generated header.
	intro := &mesonintrospect.Introspection{
		Targets: []mesonintrospect.Target{{
			Name:      "app",
			Kind:      "executable",
			Sources:   []string{filepath.Join(proot, "main.c")},
			Generated: []string{filepath.Join(bdir, "config.h")},
		}},
	}
	bd := &builddir.BuildDir{Root: bdir}
	_, err = ingest.Sources(db, bd, intro, pr, ingest.SourcesOptions{})
	return err
}
