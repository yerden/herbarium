package ingest_test

import (
	"database/sql"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yerden/herbarium/internal/builddir"
	"github.com/yerden/herbarium/internal/ingest"
	"github.com/yerden/herbarium/internal/store"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func TestCompilerIngestFixture(t *testing.T) {
	root := repoRoot(t)
	bdir := filepath.Join(root, "testdata", "fixture", "builddir")
	proot := filepath.Join(root, "testdata", "fixture")

	bd, err := builddir.Crawl(bdir)
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	pr, err := ingest.NewPathResolver(bdir, proot)
	if err != nil {
		t.Fatalf("NewPathResolver: %v", err)
	}

	path := filepath.Join(t.TempDir(), "test.hbr")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := store.Init(db); err != nil {
		t.Fatalf("Init: %v", err)
	}

	sum, err := ingest.Compiler(db, bd, pr)
	if err != nil {
		t.Fatalf("Compiler: %v", err)
	}
	if sum.Symbols == 0 || sum.CallEdges == 0 {
		t.Fatalf("empty summary: %+v", sum)
	}
	dwarfSum, err := ingest.DWARF(db, bd, pr, sum.IDByUSR)
	if err != nil {
		t.Fatalf("DWARF: %v", err)
	}
	if dwarfSum.Signatures == 0 {
		t.Errorf("DWARF pass produced 0 signatures: %+v", dwarfSum)
	}
	if dwarfSum.IndirectSites == 0 {
		t.Errorf("DWARF pass produced 0 indirect sites")
	}

	// Expected symbols (by name) that should exist in the DB.
	t.Run("core symbols", func(t *testing.T) {
		for _, want := range []struct {
			name    string
			kind    string
			linkage string
		}{
			{"main", "function", "external"},
			{"compute", "function", "external"},
			{"add_ints", "function", "external"},
			{"mul_ints", "function", "external"},
			{"hook", "function", "external"}, // strong override wins over weak
			{"g_ops", "variable", "external"},
			{"never_called", "function", "external"},
			{"use_dispatch", "function", "internal"}, // static in app1/main.c
		} {
			var kind, linkage string
			if err := db.QueryRow(
				`SELECT kind, linkage FROM symbols WHERE name = ?`, want.name,
			).Scan(&kind, &linkage); err != nil {
				t.Errorf("symbol %q: %v", want.name, err)
				continue
			}
			if kind != want.kind {
				t.Errorf("%s.kind = %q, want %q", want.name, kind, want.kind)
			}
			if linkage != want.linkage {
				t.Errorf("%s.linkage = %q, want %q", want.name, linkage, want.linkage)
			}
		}
	})

	// Multi-def symbols should have one row per def in symbol_definitions.
	t.Run("multi-def main", func(t *testing.T) {
		// The fixture defines main() twice (app1/main.c and app2/main.c).
		// Both defs must appear in symbol_definitions.
		var n int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM symbol_definitions sd
			JOIN symbols s ON sd.symbol_id = s.id
			WHERE s.name = 'main'
		`).Scan(&n); err != nil {
			t.Fatalf("query: %v", err)
		}
		if n != 2 {
			t.Errorf("main defs = %d, want 2 (one per executable)", n)
		}
		// Check the actual files.
		rows, err := db.Query(`
			SELECT sd.file FROM symbol_definitions sd
			JOIN symbols s ON sd.symbol_id = s.id
			WHERE s.name = 'main' ORDER BY sd.file
		`)
		if err != nil {
			t.Fatalf("query: %v", err)
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
		wantFiles := []string{"app1/main.c", "app2/main.c"}
		if len(files) != len(wantFiles) {
			t.Errorf("main defs files = %v, want %v", files, wantFiles)
		} else {
			for i, f := range wantFiles {
				if files[i] != f {
					t.Errorf("main def[%d] = %q, want %q", i, files[i], f)
				}
			}
		}
	})

	// hook is defined weak in lib/weak_impl.c and strong in
	// app1/strong_override.c — both must show up, with is_weak flags.
	t.Run("multi-def hook (weak + strong)", func(t *testing.T) {
		rows, err := db.Query(`
			SELECT sd.file, sd.is_weak FROM symbol_definitions sd
			JOIN symbols s ON sd.symbol_id = s.id
			WHERE s.name = 'hook' ORDER BY sd.file
		`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var strong, weak int
		for rows.Next() {
			var file string
			var isWeak int
			if err := rows.Scan(&file, &isWeak); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if isWeak == 1 {
				weak++
			} else {
				strong++
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iter: %v", err)
		}
		if strong != 1 {
			t.Errorf("hook strong defs = %d, want 1 (strong_override.c)", strong)
		}
		if weak != 1 {
			t.Errorf("hook weak defs = %d, want 1 (weak_impl.c)", weak)
		}
	})

	// External-only symbols (printf) have zero def rows.
	t.Run("printf has no defs", func(t *testing.T) {
		var n int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM symbol_definitions sd
			JOIN symbols s ON sd.symbol_id = s.id
			WHERE s.name = 'printf'
		`).Scan(&n); err != nil {
			t.Fatalf("query: %v", err)
		}
		if n != 0 {
			t.Errorf("printf defs = %d, want 0 (external ref only)", n)
		}
	})

	t.Run("address_taken", func(t *testing.T) {
		// add_ints and mul_ints are stored in the const g_ops table →
		// address-taken.
		for _, name := range []string{"add_ints", "mul_ints"} {
			var addr int
			if err := db.QueryRow(
				`SELECT address_taken FROM symbols WHERE name = ?`, name,
			).Scan(&addr); err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if addr != 1 {
				t.Errorf("%s.address_taken = %d, want 1", name, addr)
			}
		}
		// main and never_called are NOT address-taken.
		for _, name := range []string{"main", "never_called"} {
			var addr int
			if err := db.QueryRow(
				`SELECT address_taken FROM symbols WHERE name = ?`, name,
			).Scan(&addr); err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if addr != 0 {
				t.Errorf("%s.address_taken = %d, want 0", name, addr)
			}
		}
	})

	t.Run("clone linkage names", func(t *testing.T) {
		// use_dispatch is static in app1/main.c; the USR is file-scoped.
		// Its linkage_names should include the clone forms observed by
		// GCC's IPA passes (use_dispatch.constprop, use_dispatch.constprop.0).
		var linkageNames sql.NullString
		if err := db.QueryRow(
			`SELECT linkage_names FROM symbols WHERE name = 'use_dispatch'`,
		).Scan(&linkageNames); err != nil {
			t.Fatalf("query: %v", err)
		}
		if !linkageNames.Valid {
			t.Fatal("use_dispatch.linkage_names is NULL")
		}
		got := linkageNames.String
		for _, want := range []string{"use_dispatch.constprop", "use_dispatch"} {
			if !strings.Contains(got, want) {
				t.Errorf("use_dispatch.linkage_names missing %q; got %s", want, got)
			}
		}
	})

	t.Run("call edges", func(t *testing.T) {
		// main → compute must exist.
		var n int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM call_edges ce
			JOIN symbols c1 ON ce.caller_id = c1.id
			JOIN symbols c2 ON ce.callee_id = c2.id
			WHERE c1.name = 'main' AND c2.name = 'compute'
			  AND ce.source = 'compiler_cgraph'
		`).Scan(&n); err != nil {
			t.Fatalf("query: %v", err)
		}
		if n < 1 {
			t.Errorf("no call_edge main → compute; total edges = %d", sum.CallEdges)
		}
		// compute → add_ints and compute → mul_ints from shared_utils.c.
		for _, callee := range []string{"add_ints", "mul_ints"} {
			var m int
			if err := db.QueryRow(`
				SELECT COUNT(*) FROM call_edges ce
				JOIN symbols c1 ON ce.caller_id = c1.id
				JOIN symbols c2 ON ce.callee_id = c2.id
				WHERE c1.name = 'compute' AND c2.name = ?
			`, callee).Scan(&m); err != nil {
				t.Fatalf("query: %v", err)
			}
			if m < 1 {
				t.Errorf("no call_edge compute → %s", callee)
			}
		}
	})

	t.Run("indirect call sites", func(t *testing.T) {
		// use_dispatch has 2 indirect callsites at main.c:6:13 (g_ops.add)
		// and main.c:7:13 (g_ops.mul). Even though use_dispatch gets
		// inlined into main at codegen time, DWARF's inlined-subroutine
		// chain lets us attribute the sites back to their source caller.
		rows, err := db.Query(`
			SELECT ics.file, ics.line, ics.column, ics.callee_type, ics.field_hint
			FROM indirect_call_sites ics
			JOIN symbols s ON ics.caller_id = s.id
			WHERE s.name = 'use_dispatch'
			ORDER BY ics.line
		`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var got [][3]any
		var typesAndHints [][2]string
		for rows.Next() {
			var f, calleeType, fieldHint string
			var line, col int
			if err := rows.Scan(&f, &line, &col, &calleeType, &fieldHint); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, [3]any{f, line, col})
			typesAndHints = append(typesAndHints, [2]string{calleeType, fieldHint})
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iter: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("indirect_call_sites for use_dispatch = %d, want 2\ngot: %v", len(got), got)
		}
		want := [][3]any{
			{"app1/main.c", 6, 13}, // g_ops.add(a, b)
			{"app1/main.c", 7, 13}, // g_ops.mul(a, b)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("indirect site[%d] = %v, want %v", i, got[i], want[i])
			}
		}
		// callee_type is rendered like symbols.signature so
		// resolve_indirect_call can narrow by equality join.
		wantTH := [][2]string{
			{"int (int, int)", "ops.add"},
			{"int (int, int)", "ops.mul"},
		}
		for i := range wantTH {
			if typesAndHints[i] != wantTH[i] {
				t.Errorf("indirect site[%d] type/hint = %v, want %v", i, typesAndHints[i], wantTH[i])
			}
		}
	})

	t.Run("signatures from DWARF", func(t *testing.T) {
		for _, want := range []struct {
			name string
			sig  string
		}{
			{"main", "int (int, char **)"},
			{"compute", "int (int, int)"},
			{"add_ints", "int (int, int)"},
			{"hook", "int (int)"},
			{"use_dispatch", "int (int, int)"},
			// Declared `void never_called(void)` — DW_AT_prototyped is
			// set, so the empty list renders "(void)", not the "()" of a
			// non-prototyped declaration.
			{"never_called", "void (void)"},
		} {
			var got string
			if err := db.QueryRow(
				`SELECT signature FROM symbols WHERE name = ?`, want.name,
			).Scan(&got); err != nil {
				t.Errorf("signature for %s: %v", want.name, err)
				continue
			}
			if got != want.sig {
				t.Errorf("%s.signature = %q, want %q", want.name, got, want.sig)
			}
		}
	})

	t.Run("decl_file from DWARF", func(t *testing.T) {
		// compute is declared in lib/shared_utils.h line 6, defined in
		// lib/shared_utils.c line 12. DWARF fills decl_file/decl_line
		// on the def rows.
		rows, err := db.Query(`
			SELECT sd.decl_file, sd.decl_line
			FROM symbol_definitions sd
			JOIN symbols s ON sd.symbol_id = s.id
			WHERE s.name = 'compute'
		`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var declFile string
		var declLine int
		for rows.Next() {
			if err := rows.Scan(&declFile, &declLine); err != nil {
				t.Fatalf("scan: %v", err)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iter: %v", err)
		}
		if declFile != "lib/shared_utils.h" || declLine != 6 {
			t.Errorf("compute decl = %s:%d, want lib/shared_utils.h:6", declFile, declLine)
		}
	})

	t.Run("def location for functions with no .ci node", func(t *testing.T) {
		// GCC emits a callgraph-info node only for a function that reached
		// the assembler, so one inlined at every call site leaves the
		// Compiler pass with nothing but the TU fallback at line 0. DWARF's
		// abstract instance root repairs it. hdr_clamp is the case that
		// matters: its body is written in a header, so without this repair
		// no row in the database would ever name lib/hdr_inline.h and a
		// "what is defined in this header" query comes back empty.
		for _, tc := range []struct {
			name, file string
			line       int
		}{
			{"hdr_clamp", "lib/hdr_inline.h", 11},
			{"scale_by_two", "lib/shared_utils.c", 23},
		} {
			var file string
			var line int
			if err := db.QueryRow(`
				SELECT sd.file, sd.line
				FROM symbol_definitions sd
				JOIN symbols s ON s.id = sd.symbol_id
				WHERE s.name = ?`, tc.name).Scan(&file, &line); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if file != tc.file || line != tc.line {
				t.Errorf("%s def = %s:%d, want %s:%d", tc.name, file, line, tc.file, tc.line)
			}
		}
	})

	t.Run("inline decisions", func(t *testing.T) {
		// At least one row with inlined=1 (use_dispatch.constprop into main).
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM inline_decisions WHERE inlined = 1`,
		).Scan(&n); err != nil {
			t.Fatalf("query: %v", err)
		}
		if n < 1 {
			t.Errorf("no inline_decisions with inlined=1")
		}
	})

	t.Run("FTS lookup by name and signature", func(t *testing.T) {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM symbols_fts WHERE symbols_fts MATCH 'add_ints'`,
		).Scan(&n); err != nil {
			t.Fatalf("fts name: %v", err)
		}
		if n < 1 {
			t.Errorf("FTS lookup for add_ints returned %d rows", n)
		}
		// After DWARF pass, FTS index knows signatures too.
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM symbols_fts WHERE symbols_fts MATCH 'signature:char'`,
		).Scan(&n); err != nil {
			// column-qualified match may not match; try a plain query
			if err2 := db.QueryRow(
				`SELECT COUNT(*) FROM symbols_fts WHERE symbols_fts MATCH 'char'`,
			).Scan(&n); err2 != nil {
				t.Fatalf("fts signature: %v / %v", err, err2)
			}
		}
		if n < 1 {
			t.Errorf("FTS lookup for char (in main's signature) returned %d rows", n)
		}
	})

	// The early inliner folds scale_by_two into scaled_compute before any
	// IPA pass runs, so this fact exists only in the optimization record
	// and in DWARF — inline_decisions cannot carry it.
	t.Run("early inline is recorded", func(t *testing.T) {
		var pass string
		var inlined int
		if err := db.QueryRow(`
			SELECT r.pass, r.inlined FROM inline_records r
			JOIN symbols cr ON cr.id = r.caller_id
			JOIN symbols ce ON ce.id = r.callee_id
			WHERE cr.name = 'scaled_compute' AND ce.name = 'scale_by_two'`,
		).Scan(&pass, &inlined); err != nil {
			t.Fatalf("inline_records for scale_by_two: %v", err)
		}
		if pass != "einline" || inlined != 1 {
			t.Errorf("pass/inlined = %q/%d, want einline/1", pass, inlined)
		}

		var n int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM inline_decisions d
			JOIN symbols ce ON ce.id = d.callee_id
			WHERE ce.name = 'scale_by_two' AND d.inlined = 1`).Scan(&n); err != nil {
			t.Fatalf("inline_decisions: %v", err)
		}
		if n != 0 {
			t.Errorf("inline_decisions rows for scale_by_two = %d, want 0 (IPA never saw it)", n)
		}

		var caller, file string
		var line, depth int
		if err := db.QueryRow(`
			SELECT cr.name, i.file, i.line, i.depth FROM inline_instances i
			JOIN symbols cr ON cr.id = i.caller_id
			JOIN symbols ce ON ce.id = i.callee_id
			WHERE ce.name = 'scale_by_two'`,
		).Scan(&caller, &file, &line, &depth); err != nil {
			t.Fatalf("inline_instances for scale_by_two: %v", err)
		}
		if caller != "scaled_compute" || file != "lib/shared_utils.c" || depth != 1 {
			t.Errorf("instance = %s/%s:%d depth %d, want scaled_compute/lib/shared_utils.c depth 1", caller, file, line, depth)
		}
	})

	// A rejection carries GCC's own reason, and the same call can be
	// weighed twice — once before IPA analysis and once after.
	t.Run("rejections keep their reason", func(t *testing.T) {
		rows, err := db.Query(`
			SELECT r.pass, r.reason FROM inline_records r
			JOIN symbols cr ON cr.id = r.caller_id
			JOIN symbols ce ON ce.id = r.callee_id
			WHERE cr.name = 'compute' AND ce.name = 'add_ints' AND r.inlined = 0
			ORDER BY r.pass`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		byPass := map[string]string{}
		for rows.Next() {
			var pass, reason string
			if err := rows.Scan(&pass, &reason); err != nil {
				t.Fatalf("scan: %v", err)
			}
			byPass[pass] = reason
		}
		for _, pass := range []string{"einline", "inline"} {
			if byPass[pass] != "function body can be overwritten at link time" {
				t.Errorf("%s reason = %q", pass, byPass[pass])
			}
		}
	})

	// The IPA inliner names clones; the row must land on the parent USR.
	t.Run("clone record resolves to its parent", func(t *testing.T) {
		var n int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM inline_records r
			JOIN symbols cr ON cr.id = r.caller_id
			JOIN symbols ce ON ce.id = r.callee_id
			WHERE cr.name = 'main' AND ce.name = 'use_dispatch'
			  AND r.pass = 'inline' AND r.inlined = 1`).Scan(&n); err != nil {
			t.Fatalf("query: %v", err)
		}
		if n != 1 {
			t.Errorf("use_dispatch.constprop -> main rows = %d, want 1", n)
		}
	})

	// icf_add_one is inlined into icf_bump_by_one and then folded away —
	// a decision with no surviving body. The two planes must disagree
	// here; that disagreement is the point of keeping both.
	t.Run("decision without a surviving body", func(t *testing.T) {
		var decided, instances int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM inline_records r
			JOIN symbols cr ON cr.id = r.caller_id
			JOIN symbols ce ON ce.id = r.callee_id
			WHERE cr.name = 'icf_bump_by_one' AND ce.name = 'icf_add_one' AND r.inlined = 1`,
		).Scan(&decided); err != nil {
			t.Fatalf("records: %v", err)
		}
		if decided == 0 {
			t.Error("no inline_records row for icf_add_one -> icf_bump_by_one")
		}
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM inline_instances i
			JOIN symbols cr ON cr.id = i.caller_id
			JOIN symbols ce ON ce.id = i.callee_id
			WHERE cr.name = 'icf_bump_by_one' AND ce.name = 'icf_add_one'`,
		).Scan(&instances); err != nil {
			t.Fatalf("instances: %v", err)
		}
		if instances != 0 {
			t.Errorf("inline_instances for icf_add_one = %d, want 0 (the copy folded away)", instances)
		}
	})
}

func TestPathResolver(t *testing.T) {
	pr, err := ingest.NewPathResolver(
		"/home/x/proj/build",
		"/home/x/proj",
	)
	if err != nil {
		t.Fatalf("NewPathResolver: %v", err)
	}

	cases := []struct {
		in      string
		wantRel string
		inProj  bool
	}{
		{"../src/main.c", "src/main.c", true},
		{"/home/x/proj/src/main.c", "src/main.c", true},
		{"/usr/include/stdio.h", "/usr/include/stdio.h", false},
	}
	for _, tc := range cases {
		got := pr.ToProjectRelative(tc.in)
		if got.Rel != tc.wantRel {
			t.Errorf("ToProjectRelative(%q).Rel = %q, want %q", tc.in, got.Rel, tc.wantRel)
		}
		if got.InProject != tc.inProj {
			t.Errorf("ToProjectRelative(%q).InProject = %v, want %v", tc.in, got.InProject, tc.inProj)
		}
	}
}
