package store_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
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
		"external_sources", "generated_sources",
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

// TestSealForReadingLeavesNoWAL covers the property a shipped .hbr
// needs and WAL denies it: openable read-only from a directory the
// reader cannot write. Collect writes under WAL, so without the seal
// every reader has to create a -shm beside the index.
func TestSealForReadingLeavesNoWAL(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the directory mode this test relies on is not enforced")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sealed.hbr")

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Init(db); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.SealForReading(path); err != nil {
		t.Fatalf("SealForReading: %v", err)
	}

	// Byte 18 of the header is the write version: 2 means WAL, 1 means
	// the rollback journal a read-only reader needs no sidecar for.
	header := make([]byte, 20)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open sealed index: %v", err)
	}
	if _, err := io.ReadFull(f, header); err != nil {
		t.Fatalf("read header: %v", err)
	}
	f.Close()
	if header[18] != 1 || header[19] != 1 {
		t.Errorf("header write/read version = %d/%d, want 1/1 (still WAL?)", header[18], header[19])
	}

	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
			t.Errorf("os.Stat(%s) = %v, want IsNotExist", filepath.Base(sidecar), err)
		}
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	ro, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly from an unwritable directory: %v", err)
	}
	defer ro.Close()
	var version string
	if err := ro.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil {
		t.Errorf("query sealed index: %v", err)
	}
	if version != store.SchemaVersion {
		t.Errorf("schema_version = %q, want %q", version, store.SchemaVersion)
	}
}

// TestDiagnosePath pins the distinctions SQLite refuses to make: a
// missing file and an unreadable one both come back as "unable to open
// database file (14)", which reads as corruption and sent a real
// session hunting in the wrong direction.
func TestDiagnosePath(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "absent.hbr")
	if err := store.DiagnosePath(missing); err == nil ||
		!strings.Contains(err.Error(), "no file at") {
		t.Errorf("missing file: err = %v, want a 'no file at' diagnosis", err)
	}

	if err := store.DiagnosePath(dir); err == nil ||
		!strings.Contains(err.Error(), "is a directory") {
		t.Errorf("directory: err = %v, want an 'is a directory' diagnosis", err)
	}

	empty := filepath.Join(dir, "empty.hbr")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := store.DiagnosePath(empty); err == nil ||
		!strings.Contains(err.Error(), "is empty") {
		t.Errorf("empty file: err = %v, want an 'is empty' diagnosis", err)
	}

	good := filepath.Join(dir, "good.hbr")
	if err := os.WriteFile(good, []byte("not really sqlite, but present and readable"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := store.DiagnosePath(good); err != nil {
		t.Errorf("readable file: err = %v, want nil (content is the schema check's problem)", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("running as root: file modes are not enforced, so the unreadable case cannot be built")
	}
	locked := filepath.Join(dir, "locked.hbr")
	if err := os.WriteFile(locked, []byte("owned by someone else"), 0o000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := store.DiagnosePath(locked)
	if err == nil {
		t.Fatal("unreadable file: err = nil, want a permission diagnosis")
	}
	for _, want := range []string{"cannot read it", "uid", "sudo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("permission diagnosis does not mention %q: %v", want, err)
		}
	}
}
