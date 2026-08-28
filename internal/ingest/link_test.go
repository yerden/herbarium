package ingest_test

import (
	"path/filepath"
	"testing"

	"github.com/yerden/herbarium/internal/builddir"
	"github.com/yerden/herbarium/internal/ingest"
	"github.com/yerden/herbarium/internal/mesonintrospect"
	"github.com/yerden/herbarium/internal/store"
)

// runFullIngest runs the whole ingest pipeline (Compiler + DWARF +
// Targets + Link) against the fixture and returns the produced DB path.
func runFullIngest(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	bdir := filepath.Join(root, "testdata", "fixture", "builddir")
	proot := filepath.Join(root, "testdata", "fixture")

	intro, err := mesonintrospect.Load(bdir)
	if err != nil {
		t.Fatalf("mesonintrospect.Load: %v", err)
	}
	bd, err := builddir.Crawl(bdir)
	if err != nil {
		t.Fatalf("builddir.Crawl: %v", err)
	}
	pr, err := ingest.NewPathResolver(bdir, proot)
	if err != nil {
		t.Fatalf("NewPathResolver: %v", err)
	}

	path := filepath.Join(t.TempDir(), "test.hbr")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Init(db); err != nil {
		t.Fatalf("store.Init: %v", err)
	}

	sum, err := ingest.Compiler(db, bd, pr)
	if err != nil {
		t.Fatalf("Compiler: %v", err)
	}
	if _, err := ingest.DWARF(db, bd, pr, sum.IDByUSR); err != nil {
		t.Fatalf("DWARF: %v", err)
	}
	targetIDs, err := ingest.Targets(db, intro, pr)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if _, err := ingest.Link(db, bd, intro, pr, targetIDs); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

func TestLinkResolutionsWeakStrong(t *testing.T) {
	path := runFullIngest(t)
	db, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()

	// app1: hook resolves STRONG to app1/app1.p/strong_override.c.o
	var linkage, winner, archive string
	if err := db.QueryRow(`
		SELECT lr.linkage_kind, lr.winning_object, lr.archive
		FROM link_resolutions lr
		JOIN targets t ON lr.target_id = t.id
		JOIN symbols s ON lr.usr = s.usr
		WHERE t.name = 'app1' AND s.name = 'hook'
	`).Scan(&linkage, &winner, &archive); err != nil {
		t.Fatalf("query app1.hook: %v", err)
	}
	if linkage != "strong" {
		t.Errorf("app1.hook linkage = %q, want strong", linkage)
	}
	if winner != "app1/app1.p/strong_override.c.o" {
		t.Errorf("app1.hook winning_object = %q, want app1/app1.p/strong_override.c.o", winner)
	}
	if archive != "" {
		t.Errorf("app1.hook archive = %q, want empty (compiled directly, not via .a)", archive)
	}

	// app2: hook resolves WEAK to lib/libshared.a.p/weak_impl.c.o with archive lib/libshared.a
	if err := db.QueryRow(`
		SELECT lr.linkage_kind, lr.winning_object, lr.archive
		FROM link_resolutions lr
		JOIN targets t ON lr.target_id = t.id
		JOIN symbols s ON lr.usr = s.usr
		WHERE t.name = 'app2' AND s.name = 'hook'
	`).Scan(&linkage, &winner, &archive); err != nil {
		t.Fatalf("query app2.hook: %v", err)
	}
	if linkage != "weak" {
		t.Errorf("app2.hook linkage = %q, want weak", linkage)
	}
	if winner != "lib/libshared.a.p/weak_impl.c.o" {
		t.Errorf("app2.hook winning_object = %q", winner)
	}
	if archive != "lib/libshared.a" {
		t.Errorf("app2.hook archive = %q, want lib/libshared.a", archive)
	}
}

func TestObjdumpCallEdges(t *testing.T) {
	path := runFullIngest(t)
	db, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()

	// Each target should have main → hook as an objdump edge.
	for _, target := range []string{"app1", "app2"} {
		var n int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM call_edges ce
			JOIN targets t ON ce.target_id = t.id
			JOIN symbols c1 ON ce.caller_id = c1.id
			JOIN symbols c2 ON ce.callee_id = c2.id
			WHERE t.name = ? AND c1.name = 'main' AND c2.name = 'hook'
			  AND ce.source = 'objdump'
		`, target).Scan(&n); err != nil {
			t.Fatalf("query %s: %v", target, err)
		}
		if n != 1 {
			t.Errorf("%s: main → hook objdump edges = %d, want 1", target, n)
		}
	}

	// compute → add_ints in each target.
	for _, target := range []string{"app1", "app2"} {
		var n int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM call_edges ce
			JOIN targets t ON ce.target_id = t.id
			JOIN symbols c1 ON ce.caller_id = c1.id
			JOIN symbols c2 ON ce.callee_id = c2.id
			WHERE t.name = ? AND c1.name = 'compute' AND c2.name = 'add_ints'
			  AND ce.source = 'objdump'
		`, target).Scan(&n); err != nil {
			t.Fatalf("query %s compute→add_ints: %v", target, err)
		}
		if n != 1 {
			t.Errorf("%s: compute → add_ints edges = %d, want 1", target, n)
		}
	}
}

func TestReachability(t *testing.T) {
	path := runFullIngest(t)
	db, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()

	cases := []struct {
		target       string
		symbol       string
		wantReachable int
	}{
		{"app1", "main", 1},
		{"app1", "compute", 1},
		{"app1", "hook", 1},
		{"app1", "never_called", 0}, // dead-stripped via -ffunction-sections + -Wl,--gc-sections
		// printf is dynamically linked — no defined symbol in the binary.
		{"app1", "printf", 0},
		// use_dispatch is fully inlined into main and left no symbol.
		{"app1", "use_dispatch", 0},
		{"app2", "hook", 1}, // weak def wins
	}
	for _, tc := range cases {
		var got int
		if err := db.QueryRow(`
			SELECT sr.reachable FROM symbol_reachability sr
			JOIN targets t ON sr.target_id = t.id
			JOIN symbols s ON sr.symbol_id = s.id
			WHERE t.name = ? AND s.name = ?
		`, tc.target, tc.symbol).Scan(&got); err != nil {
			t.Errorf("%s.%s: %v", tc.target, tc.symbol, err)
			continue
		}
		if got != tc.wantReachable {
			t.Errorf("%s.%s reachable = %d, want %d", tc.target, tc.symbol, got, tc.wantReachable)
		}
	}
}

func TestTargetsPopulated(t *testing.T) {
	path := runFullIngest(t)
	db, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM targets`).Scan(&n); err != nil {
		t.Fatalf("count targets: %v", err)
	}
	if n != 3 {
		t.Errorf("targets rows = %d, want 3 (shared, app1, app2)", n)
	}
	// app1 sources should include main.c and strong_override.c.
	rows, err := db.Query(`
		SELECT ts.file FROM target_sources ts
		JOIN targets t ON ts.target_id = t.id
		WHERE t.name = 'app1' ORDER BY ts.file
	`)
	if err != nil {
		t.Fatalf("query target_sources: %v", err)
	}
	defer rows.Close()
	var files []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			t.Fatalf("scan: %v", err)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iter: %v", err)
	}
	want := []string{"app1/main.c", "app1/strong_override.c"}
	if len(files) != 2 || files[0] != want[0] || files[1] != want[1] {
		t.Errorf("app1 target_sources = %v, want %v", files, want)
	}
}
