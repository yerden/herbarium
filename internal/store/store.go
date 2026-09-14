// Package store owns the SQLite connection lifecycle and schema init.
// The schema itself lives in schema.sql and mirrors herbarium-plan.md.
package store

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"

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
//
// v7: adds inline_records (every inliner's decisions, from
// -fsave-optimization-record) and inline_instances (inlined bodies that
// survived into the object, from DWARF DW_TAG_inlined_subroutine).
// Before v7 the only inlining fact was inline_decisions, sourced from
// the .cgraph (inlined) tag — an IPA-stage-only view that misses
// everything the early inliner folded before IPA ran, with nothing in
// the index recording that the view was partial. Collect now requires
// -fsave-optimization-record; a v6-era builddir fails preflight.
//
// v7 → v8: no structural change, but symbol_definitions.file/line changed
// meaning for one class of row. A function GCC inlined at every call site
// gets no callgraph-info node, and v7 recorded it at the including TU with
// line 0 — so a static inline whose body is written in a header claimed to
// be defined in the .c that included it, and no query on the header could
// find it. v8 recovers the real location from DWARF's abstract instance
// root, which is why `file` may now name a .h. The bump exists because
// serve cannot otherwise tell the two apart: a v7 artifact answers "what
// is defined in this header" with zero rows and nothing marks the answer
// as stale. Re-collect to pick up the corrected locations.
//
// v8 -> v9: drops devirt_hints, and with it the list_devirt_hints tool
// and the .devirt parser. The table never had a writer, so no index ever
// held a row -- but the deeper reason is that it never could. GCC's
// ipa-devirt pass acts on polymorphic calls, which come from C++
// OBJ_TYPE_REF nodes; C emits none, so every .devirt dump reports "0
// polymorphic calls, 0 devirtualized, 0 speculatively devirtualized".
// The one section carrying real C data -- "Noted function pointers
// stored in records" -- appears only for a static const dispatch table,
// is absent from real-world code that assigns its function pointers at
// registration time, and where it does appear DWARF already answers
// better: a field name via dwarfingest/calltarget.go, not a byte offset
// that would need DWARF to resolve anyway. Everything else .devirt
// carried is a strict subset of .cgraph, which ingest already requires
// and parses. Dropping it also removes -fdump-ipa-devirt from the
// required c_args, so a v9 collect needs one flag fewer than v8 and GCC
// writes one dump fewer per TU.
// v9 -> v10: adds the type plane — types, type_fields, enum_constants
// and types_fts. All three come from DWARF the reader was already
// walking: dwarfingest populated Structs and Typedefs for several
// versions and ingest discarded both. Enumerators are the only new
// extraction, and they need no new build flag — DW_TAG_enumerator with
// DW_AT_const_value is present at plain -g, which preflight already
// requires. The USR forms were specified in the plan's appendix from the
// start and internal/usr has implemented them, uncalled, since then.
//
// A v9 index answers "where is this typedef" with nothing, and so does a
// v10 index for a type no TU uses -- DWARF omits unused types at every -g
// level. The difference the bump marks is that v10 can distinguish the
// two, and v9 could not tell the caller which it was looking at.
const SchemaVersion = "10"

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

// DiagnosePath explains, in terms a caller can act on, why a path is not
// a usable index. SQLite answers both "no such file" and "you may not
// read this file" with the same `unable to open database file (14)`,
// which sends the reader hunting for corruption when the real cause is a
// wrong --out or a collect that ran under sudo. Returns nil when the
// file is present and readable — whether it is a *herbarium* index is
// then the caller's schema check to make.
func DiagnosePath(path string) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("there is no file at %s", path)
	case err != nil:
		return fmt.Errorf("cannot stat %s: %w", path, err)
	case info.IsDir():
		return fmt.Errorf("%s is a directory, not an .hbr index", path)
	case info.Size() == 0:
		return fmt.Errorf("%s is empty — a collect was probably interrupted before it finished", path)
	}

	// Stat succeeds on a file the caller cannot read: it needs only
	// execute permission on the directory. Opening is the only way to
	// tell the two apart before handing the path to SQLite.
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf(
				"%s is mode %s and this process runs as uid %d, which cannot read it — "+
					"the index was written by another user, typically a collect run under sudo. "+
					"Re-collect as this user, or chown the file to it",
				path, info.Mode().Perm(), os.Getuid())
		}
		return fmt.Errorf("cannot read %s: %w", path, err)
	}
	return f.Close()
}

// SealForReading takes a finished index out of WAL journalling. Collect
// writes under WAL for throughput, but the mode lives in the file header
// and outlives the writer: a WAL database cannot be opened even
// read-only without a -shm beside it, so a shipped .hbr would demand
// write access to its own directory from every reader and fail with
// SQLITE_READONLY_DIRECTORY wherever it does not have it. Sealing also
// unlinks the -wal/-shm sidecars, which matters for `collect --replace`
// — a serve process reopening the path must not find sidecars left over
// from a different inode.
//
// Takes a path rather than a handle because Open pins journal_mode(WAL)
// on every connection it hands out, and because the switch needs to be
// the only connection to the file.
func SealForReading(path string) error {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path))
	if err != nil {
		return fmt.Errorf("store: seal %q: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		return fmt.Errorf("store: seal %q: %w", path, err)
	}
	// SQLite reports the mode still in force rather than failing when it
	// cannot make the switch, so the return value is the only evidence
	// the seal took.
	if !strings.EqualFold(mode, "delete") {
		return fmt.Errorf("store: seal %q: journal_mode is %q after the switch, want \"delete\"", path, mode)
	}
	return db.Close()
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
