package store_test

import (
	"path/filepath"
	"testing"

	"github.com/yerden/herbarium/internal/store"
)

func TestInitAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hbr")

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Init(db); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Every table listed in the plan schema must exist.
	wantTables := []string{
		"meta", "targets", "target_sources",
		"blobs", "sources",
		"symbols", "symbols_fts", "symbol_definitions",
		"call_edges", "indirect_call_sites", "devirt_hints",
		"inline_decisions", "link_resolutions", "symbol_reachability",
	}
	for _, name := range wantTables {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name,
		).Scan(&n); err != nil {
			t.Fatalf("query for %s: %v", name, err)
		}
		if n == 0 {
			t.Errorf("missing table/vtable: %s", name)
		}
	}

	// linkage_names is the plan touch-up we added — verify the column
	// landed on the symbols table.
	rows, err := db.Query(`PRAGMA table_info(symbols)`)
	if err != nil {
		t.Fatalf("PRAGMA: %v", err)
	}
	defer rows.Close()
	var found bool
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, dflt, pk any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma: %v", err)
		}
		if name == "linkage_names" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iter pragma: %v", err)
	}
	if !found {
		t.Error("symbols.linkage_names column not present")
	}

	// FTS5 must be usable — insert a symbol and search for it via the
	// contentless-mirror table symbols_fts.
	if _, err := db.Exec(
		`INSERT INTO symbols(usr, name, kind, signature) VALUES
		 ('c:@F@add_ints', 'add_ints', 'function', 'int(int, int)')`,
	); err != nil {
		t.Fatalf("insert symbol: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO symbols_fts(rowid, name, signature)
		 SELECT id, name, signature FROM symbols WHERE name='add_ints'`,
	); err != nil {
		t.Fatalf("insert fts row: %v", err)
	}
	var hits int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM symbols_fts WHERE symbols_fts MATCH 'add_ints'`,
	).Scan(&hits); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if hits != 1 {
		t.Errorf("fts hits = %d, want 1", hits)
	}

	// Schema version stamped.
	var ver string
	if err := db.QueryRow(
		`SELECT value FROM meta WHERE key='schema_version'`,
	).Scan(&ver); err != nil {
		t.Fatalf("meta lookup: %v", err)
	}
	if ver != store.SchemaVersion {
		t.Errorf("schema_version = %q, want %q", ver, store.SchemaVersion)
	}

	// Second Init on a populated DB must refuse (never silently overwrite).
	if err := store.Init(db); err == nil {
		t.Error("Init on populated DB returned nil, want refusal")
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen read-only and verify writes are rejected.
	ro, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	if _, err := ro.Exec(`INSERT INTO meta(key, value) VALUES ('probe', 'x')`); err == nil {
		t.Error("write via read-only handle succeeded, want failure")
	}
}
