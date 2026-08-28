// Package store owns the SQLite connection lifecycle and schema init.
// The schema itself lives in schema.sql and mirrors herbarium-plan.md.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// SchemaVersion is written into the meta table on Init and checked on Open.
// Any change to schema.sql must bump this and — per the plan appendix —
// document a migration path for existing .hbr files.
//
// v1 → v2: symbols.file/line/decl_file/decl_line moved to a new
// symbol_definitions table so that a single USR can carry multiple defs
// (weak/strong overrides, multi-executable `main`, static-inline in
// headers). No migration path is provided — .hbr files are single-shot
// build artifacts, so callers just re-collect.
const SchemaVersion = "2"

//go:embed schema.sql
var schemaSQL string

// Schema exposes the embedded DDL for callers that want to inspect it
// (e.g., the MCP describe_schema tool in Phase 6).
func Schema() string { return schemaSQL }

// Open opens an .hbr file read-write with WAL journalling — the mode used
// during `herbarium collect`. Callers must Close.
func Open(path string) (*sql.DB, error) {
	// modernc.org/sqlite accepts _pragma=… query params to set pragmas on
	// every new connection.
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %q: %w", path, err)
	}
	return db, nil
}

// OpenReadOnly opens an .hbr file read-only — the mode used during
// `herbarium serve`. Read-only is enforced at the driver via mode=ro so
// even a bug in a tool cannot mutate a shipped index.
func OpenReadOnly(path string) (*sql.DB, error) {
	dsn := "file:" + url.PathEscape(path) +
		"?mode=ro" +
		"&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open ro %q: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping ro %q: %w", path, err)
	}
	return db, nil
}

// Init applies the embedded schema and stamps meta.schema_version. Safe
// to call on an empty DB; refuses on a non-empty DB to avoid overwrite.
func Init(db *sql.DB) error {
	var haveSchema int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='meta'`,
	).Scan(&haveSchema); err != nil {
		return fmt.Errorf("store: probe schema: %w", err)
	}
	if haveSchema > 0 {
		return fmt.Errorf("store: refusing to init: schema already present")
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("store: apply schema: %w", err)
	}
	if _, err := db.Exec(
		`INSERT INTO meta(key, value) VALUES ('schema_version', ?)`,
		SchemaVersion,
	); err != nil {
		return fmt.Errorf("store: stamp schema_version: %w", err)
	}
	return nil
}
