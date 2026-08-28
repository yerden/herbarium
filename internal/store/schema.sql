-- herbarium schema — mirror of `herbarium-plan.md § Database schema`.
-- Any change here must match the plan; the plan is the contract.

-- Reproducibility & versioning
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT
);
-- Expected keys: schema_version, gcc_version, meson_version,
-- build_config_hash, project_root_hint, indexed_at, herbarium_version.

-- Build-system view
CREATE TABLE targets (
  id           INTEGER PRIMARY KEY,
  name         TEXT UNIQUE,
  kind         TEXT,            -- 'executable' | 'static_library' | 'shared_library'
  link_command TEXT
);

CREATE TABLE target_sources (
  target_id INTEGER REFERENCES targets(id),
  file      TEXT                -- project-relative
);
CREATE INDEX idx_ts_target ON target_sources(target_id);
CREATE INDEX idx_ts_file   ON target_sources(file);

-- Source content, content-addressed and deduplicated
CREATE TABLE blobs (
  hash    TEXT PRIMARY KEY,     -- SHA-256 hex of raw bytes
  size    INTEGER,
  content BLOB                  -- zstd-compressed
);

CREATE TABLE sources (
  path         TEXT PRIMARY KEY,-- project-relative
  blob_hash    TEXT REFERENCES blobs(hash),
  is_generated INTEGER
);

-- Symbols: identity only. One row per USR. Def locations live in
-- symbol_definitions — a symbol may have multiple defs across TUs
-- (weak/strong overrides, multi-executable `main`, static-inline in
-- headers). See herbarium-plan.md § Symbols and definitions.
CREATE TABLE symbols (
  id            INTEGER PRIMARY KEY,
  usr           TEXT UNIQUE,    -- 'c:@F@name' (external) | 'c:<path>@F@name' (static)
  name          TEXT,
  kind          TEXT,           -- 'function' | 'variable' | 'typedef' | ...
  linkage       TEXT,           -- 'external' | 'internal' | 'weak' | 'common' (aggregated)
  signature     TEXT,           -- reconstructed from DWARF (Phase 3)
  address_taken INTEGER,        -- 0/1 aggregated across all TUs where defined
  linkage_names TEXT            -- JSON array of every link-time name (incl. GCC clones)
);
CREATE INDEX idx_sym_name       ON symbols(name);
CREATE INDEX idx_sym_kind       ON symbols(kind);
CREATE INDEX idx_sym_addr_taken ON symbols(address_taken);
CREATE VIRTUAL TABLE symbols_fts USING fts5(
  name, signature,
  content='symbols', content_rowid='id',
  tokenize='unicode61 separators _'
);

-- One row per observed definition of a symbol. External symbols with
-- multi-def (e.g., `main` in a multi-executable project, `hook` with a
-- strong override plus a weak fallback) get multiple rows here — the
-- `symbols` row (the identity) is still one. Decls with no observed
-- def contribute zero rows (external references like libc's `printf`).
CREATE TABLE symbol_definitions (
  id           INTEGER PRIMARY KEY,
  symbol_id    INTEGER REFERENCES symbols(id),
  file         TEXT,            -- def file, project-relative
  line         INTEGER,         -- def line
  decl_file    TEXT,            -- from DWARF; '' if same as file
  decl_line    INTEGER,         -- 0 if same as line
  is_weak      INTEGER,         -- 0/1 — this specific def is weak
  linkage_name TEXT             -- link-time name of this def (mostly = symbol name;
                                -- differs for GCC clones like 'use_dispatch.constprop.0')
);
CREATE INDEX idx_sd_symbol ON symbol_definitions(symbol_id);
CREATE INDEX idx_sd_file   ON symbol_definitions(file);

-- Direct call edges, from two independent sources
CREATE TABLE call_edges (
  caller_id INTEGER REFERENCES symbols(id),
  callee_id INTEGER REFERENCES symbols(id),
  source    TEXT,               -- 'compiler_cgraph' | 'objdump'
  target_id INTEGER             -- NULL for compiler_cgraph; set for objdump
);
CREATE INDEX idx_ce_caller ON call_edges(caller_id, source);
CREATE INDEX idx_ce_callee ON call_edges(callee_id, source);

-- Indirect call sites, seen by the compiler
CREATE TABLE indirect_call_sites (
  id          INTEGER PRIMARY KEY,
  caller_id   INTEGER REFERENCES symbols(id),
  file        TEXT,
  line        INTEGER,
  column      INTEGER,          -- when available from DWARF
  callee_type TEXT,             -- canonical fn-ptr type from DWARF
  field_hint  TEXT              -- 'struct_t.field' when DWARF resolves it, else ''
);
CREATE INDEX idx_ics_caller ON indirect_call_sites(caller_id);
CREATE INDEX idx_ics_type   ON indirect_call_sites(callee_type);

-- Compiler-reported speculative resolutions of indirect calls
CREATE TABLE devirt_hints (
  site_id    INTEGER REFERENCES indirect_call_sites(id),
  callee_id  INTEGER REFERENCES symbols(id),
  confidence TEXT               -- 'speculative' | 'resolved'
);

-- Inlining record (for source-vs-runtime reconciliation)
CREATE TABLE inline_decisions (
  caller_id INTEGER REFERENCES symbols(id),
  callee_id INTEGER REFERENCES symbols(id),
  inlined   INTEGER             -- 0/1
);

-- Linkage-time truth
CREATE TABLE link_resolutions (
  target_id      INTEGER REFERENCES targets(id),
  usr            TEXT,
  winning_object TEXT,
  linkage_kind   TEXT,          -- 'strong' | 'weak' | 'unique_global' | 'common'
  losing_objects TEXT,          -- JSON array
  archive        TEXT           -- containing archive if pulled from .a, else ''
);
CREATE INDEX idx_lr_usr    ON link_resolutions(usr);
CREATE INDEX idx_lr_target ON link_resolutions(target_id);

-- Reachability from entry points (link-time GC)
CREATE TABLE symbol_reachability (
  target_id    INTEGER REFERENCES targets(id),
  symbol_id    INTEGER REFERENCES symbols(id),
  reachable    INTEGER,         -- 0 = dead-stripped
  section_kept INTEGER
);
