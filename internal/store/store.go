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
//
// v2 → v3: symbol_reachability is no longer a physical table. It became
// a view derived from link_resolutions ∩ symbols (via usr). Consumers
// that previously filtered WHERE reachable = 0 must switch to a NOT
// EXISTS check against link_resolutions — the view only emits reachable=1
// rows. Motivation: the old table stored a full target×symbol cross
// product (~13M rows, ~75% of the .hbr's disk footprint) that was
// entirely derivable from link_resolutions.
//
// v3 → v4: added external_sources(abs_path, blob_hash) for headers packed
// via `collect --include-external <glob>`. Empty when the flag wasn't
// passed, so the on-disk footprint is unchanged for existing use cases.
// New join surface: symbol_definitions.decl_file → external_sources.abs_path
// (documented in describe_schema).
//
// v4 → v5: added generated_sources(builddir_rel, blob_hash) for files
// under the builddir that fall outside --project-root (typical when a
// project is configured out-of-tree). Before v5, t.Generated entries
// aborted ingest with "outside project-root" and .ninja_deps entries
// under builddir were silently skipped — so configure_file() output
// like config.h never made it into the .hbr. Key is builddir-relative
// so the value is portable.
//
// v6: adds icf_groups + icf_group_members tables. Before v6, the .icf
// dump was parsed but folded groups were not persisted, so
// list_icf_groups always returned empty and ICF-folded losers surfaced
// as false-positive dead symbols in list_unreachable_symbols.
const SchemaVersion = "6"

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
