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

-- External headers (system, vendored, third-party) that the .ninja_deps log
-- referenced from outside --project-root. Only populated when collect was
-- invoked with one or more --include-external globs; empty otherwise.
-- abs_path is the verbatim absolute path from .ninja_deps — no rewriting.
-- Shares the blobs table with sources so byte-identical content dedups.
CREATE TABLE external_sources (
  abs_path  TEXT PRIMARY KEY,
  blob_hash TEXT REFERENCES blobs(hash)
);

-- Build-tree files: anything under the builddir that isn't a source in
-- --project-root. Typically configure_file() output (config.h and friends)
-- and custom_target outputs. Keyed by builddir-relative path so the value
-- is stable across machines even when the absolute builddir differs.
-- Populated automatically whenever ingest sees a t.Generated entry or a
-- .ninja_deps header rooted under builddir — no flag required.
CREATE TABLE generated_sources (
  builddir_rel TEXT PRIMARY KEY,
  blob_hash    TEXT REFERENCES blobs(hash)
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
  -- callee_type is the callee's signature rendered like symbols.signature
  -- ("int (int, int)"), so the two join by equality. field_hint names what
  -- the call dispatches through: 'struct_t.field', a global fn-ptr's name,
  -- or a fn-pointer parameter's name. Both are '' when the compiler left
  -- no trace of the target (see internal/dwarfingest/calltarget.go).
  callee_type TEXT,
  field_hint  TEXT
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

-- Every inlining decision GCC logged, from -fsave-optimization-record.
-- Wider than inline_decisions in two ways: it covers the early inliner
-- (which folds always_inline and trivial callees before any IPA pass
-- runs, leaving no trace in .cgraph or .inline), and it keeps the
-- rejections with the compiler's own reason. A decision here is not a
-- guarantee the code survived to the binary — a callee that folds to a
-- constant afterwards leaves a row here and none in inline_instances.
CREATE TABLE inline_records (
  caller_id INTEGER REFERENCES symbols(id),
  callee_id INTEGER REFERENCES symbols(id),
  pass      TEXT NOT NULL,      -- 'einline' (pre-IPA) | 'inline' (IPA)
  inlined   INTEGER NOT NULL,   -- 0/1
  reason    TEXT,               -- GCC's explanation when inlined = 0
  file      TEXT,
  line      INTEGER,
  column    INTEGER,
  object    TEXT                -- .o the record came from, builddir-relative
);
CREATE INDEX idx_ir_caller ON inline_records(caller_id, inlined);
CREATE INDEX idx_ir_callee ON inline_records(callee_id, inlined);

-- Inlined bodies that survived into the object, from DWARF's
-- DW_TAG_inlined_subroutine. Outcome rather than decision: this is what
-- the shipped code actually contains, whichever pass put it there, but a
-- callee whose code folded away entirely leaves no DIE and so no row.
-- depth 1 is inlined straight into caller_id; deeper rows name the
-- inlined body they landed inside in parent_callee_id.
CREATE TABLE inline_instances (
  callee_id        INTEGER REFERENCES symbols(id),
  caller_id        INTEGER REFERENCES symbols(id),
  parent_callee_id INTEGER REFERENCES symbols(id),  -- NULL at depth 1
  depth            INTEGER NOT NULL,
  file             TEXT,
  line             INTEGER,
  column           INTEGER,
  object           TEXT
);
CREATE INDEX idx_ii_caller ON inline_instances(caller_id);
CREATE INDEX idx_ii_callee ON inline_instances(callee_id);

-- Identical-code folding groups from GCC's -fdump-ipa-icf. IPA-ICF only:
-- linker-level ICF (gold/lld --icf=all) is a different pass and is not
-- tracked. One row per non-singular class per TU; a group's winner is
-- the surviving symbol, losers had their bodies rewritten to tail-call
-- winner.localalias.
CREATE TABLE icf_groups (
  id               INTEGER PRIMARY KEY,
  winner_symbol_id INTEGER NOT NULL REFERENCES symbols(id),
  object_file      TEXT    NOT NULL   -- .o where the fold was recorded, builddir-relative
);
CREATE INDEX idx_icfg_winner ON icf_groups(winner_symbol_id);

-- Loser members of an ICF group. Winner is on the group row; only losers
-- appear here so a "was this folded away" query is one WHERE clause.
CREATE TABLE icf_group_members (
  group_id  INTEGER NOT NULL REFERENCES icf_groups(id),
  symbol_id INTEGER NOT NULL REFERENCES symbols(id),
  PRIMARY KEY (group_id, symbol_id)
);
CREATE INDEX idx_icfm_symbol ON icf_group_members(symbol_id);

-- Linkage-time truth
CREATE TABLE link_resolutions (
  target_id      INTEGER REFERENCES targets(id),
  usr            TEXT,
  winning_object TEXT,
  linkage_kind   TEXT,          -- 'strong' | 'weak' | 'unique_global' | 'common'
  losing_objects TEXT,          -- JSON array
  archive        TEXT           -- containing archive if pulled from .a, else ''
);
CREATE INDEX idx_lr_usr        ON link_resolutions(usr);
CREATE INDEX idx_lr_target     ON link_resolutions(target_id);
CREATE INDEX idx_lr_target_usr ON link_resolutions(target_id, usr);

-- Reachability from entry points (link-time GC). Derived view: a symbol is
-- reachable in a target iff link_resolutions records a definition for it in
-- that target's final binary. section_kept is a placeholder — parsing
-- 'Discarded input sections' from the map file is future work; until then
-- every kept symbol has section_kept=1 by definition.
CREATE VIEW symbol_reachability AS
SELECT
  lr.target_id AS target_id,
  s.id         AS symbol_id,
  1            AS reachable,
  1            AS section_kept
FROM link_resolutions lr
JOIN symbols s ON s.usr = lr.usr;
