package mcp_test

import (
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
	if _, err := ingest.Link(db, bd, intro, pr, tids); err != nil {
		return 1
	}
	if _, err := ingest.Sources(db, bd, intro, pr, ingest.SourcesOptions{}); err != nil {
		return 1
	}
	return 0
}
