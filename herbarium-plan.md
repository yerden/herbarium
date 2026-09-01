# Herbarium — a GCC-native C code index for AI agents

## What it is

Herbarium is a post-build ingestion tool for C projects. It reads an already-built Meson build directory — one the user configured with a specific set of GCC diagnostic flags — collects the artifacts GCC and the linker wrote to disk, and runs standard binutils inspectors (`nm`, `objdump`, `readelf`) over the same artifacts to derive link-plane facts. It packages the result together with the source itself into a single portable SQLite file, and serves it to an AI assistant over MCP.

The design goal: give an agent an answer that is grounded in what the compiler and linker actually did, not what a second parser guesses they would do. No parallel semantic analysis; no LSP server; no reparse of the source code; no rerunning of the compile or the link. The compiler emits structured facts as a normal side effect of the user's build; binutils inspectors read the finished objects; herbarium stitches both into an index.

## Design principles

1. **Compiler truth over source guesses.** Every fact in the DB traces back to a compiler dump, DWARF, an object symbol table, or the linker map — not to a re-parser.
2. **No plugin, no CC wrapper, no build interception.** Herbarium never runs GCC, `ld`, `meson`, or `ninja`. The user prepares the build directory with the required flags and runs `meson compile` themselves. Herbarium reads the resulting compiler dump files and DWARF from `.o` files, and — as part of ingest — invokes read-only binutils inspectors (`nm`, `objdump`, `readelf`) against the already-built objects and linked artifacts to derive the link-plane facts. Nothing herbarium invokes modifies or rebuilds any file in the build directory.
3. **One artifact.** A single `herbarium.hbr` contains schema, facts, and source blobs. Portable across machines. No live checkout needed to answer questions.
4. **Source is a first-class product, not just for portability.** The DB embeds every source and header touched by the build, addressable by project-relative path. Every location the tool returns is anchored to a path that is guaranteed to exist in the blob store, so an agent can quote real code and hand the user file:line references that also resolve in their live checkout.
5. **Static separation.** Two subcommands: `herbarium collect` ingests a builddir into `herbarium.hbr`; `herbarium serve` reads `herbarium.hbr` and exposes MCP. Serve mode has zero compiler dependency.
6. **Only stable contracts.** The DB schema exposes only facts that GCC and the linker report deterministically. No source-syntactic categorization until/unless a source walker is added as a distinct future phase.

## Architecture

```
    User's responsibility                    Herbarium's responsibility
 ┌──────────────────────────┐              ┌──────────────────────────────┐
 │ meson setup builddir     │              │                              │
 │   with required flags    │              │   herbarium collect          │
 │                          │              │     --builddir <path>        │
 │ meson compile -C builddir│─────────────▶│     --project-root <path>    │
 │                          │  builddir/   │     --out herbarium.hbr      │
 │ (dumps land alongside    │  artifacts   │                              │
 │  objects; linker map     │              │  ┌────────────────────────┐  │
 │  emitted per target)     │              │  │ meson introspect       │  │
 └──────────────────────────┘              │  │ crawl builddir         │  │
                                           │  │ read .ci/.cgraph/...   │  │
                                           │  │ read DWARF from .o     │  │
                                           │  │ nm/objdump on exes     │  │
                                           │  │ pack source blobs      │  │
                                           │  └────────────────────────┘  │
                                           │             │                │
                                           │             ▼                │
                                           │     ┌────────────────┐       │
                                           │     │ herbarium.hbr  │       │
                                           │     └────────────────┘       │
                                           │             │                │
                                           │             ▼                │
                                           │      herbarium serve         │
                                           │             │                │
                                           └─────────────┼────────────────┘
                                                         ▼
                                                     MCP → agent
```

Herbarium never invokes GCC, `ld`, `meson`, or `ninja`, and never modifies any file in the build directory. It does invoke standard binutils inspectors (`nm`, `objdump`, `readelf`) against the already-built objects and linked binaries as part of `herbarium collect`, purely to read out linkage and disassembly facts. If the user's build has not produced the required compiler dumps, herbarium fails loudly at index time.

## Prerequisites: preparing the Meson build

The user configures Meson and runs the build themselves before invoking herbarium. All of the following must be arranged in the build directory herbarium will read.

### Minimum GCC version

GCC 10 or later (required for `-fcallgraph-info`). Verified at index time via the compiler line in Meson's build log; herbarium refuses to index against older GCC.

### Compile-side flags (mandatory)

Add all of the following to `c_args`. The simplest way is on the `meson setup` command line so `meson.build` doesn't have to change:

```bash
meson setup builddir \
  --buildtype=debugoptimized \
  -Dc_args="-g -gcolumn-info -fcallgraph-info=su,da -fdump-ipa-cgraph -fdump-ipa-inline -fdump-ipa-devirt -fdump-ipa-icf -fsave-optimization-record -fno-inline-functions-called-once"
```

What each flag contributes:

| Flag | Required by herbarium for |
|---|---|
| `-g -gcolumn-info` | Symbol identity, decl/def locations, signatures, typedef chains, struct field names |
| `-fcallgraph-info=su,da` | Direct call edges, stack usage, data-area sizes |
| `-fdump-ipa-cgraph` | Post-IPA callgraph with `address_taken` flags and indirect call site records |
| `-fdump-ipa-inline` | IPA-stage inlining decisions (source vs runtime edge reconciliation) |
| `-fdump-ipa-devirt` | Speculative devirtualization hints for indirect calls |
| `-fdump-ipa-icf` | Identical-code-folded function groups |
| `-fsave-optimization-record` | Every inliner's decisions — including `pass_early_inline`, which folds `always_inline` and trivial callees before any IPA pass runs and therefore leaves no trace in any `-fdump-ipa-*` dump — plus the rejections with GCC's own reason |
| `-fno-inline-functions-called-once` | Preserves distinct nodes in the post-IPA `.cgraph` for single-caller helpers even when `-O2` still inlines them out of the `.ci` direct-edge view |

Herbarium enumerates each TU's transitive header set by parsing ninja's consolidated `.ninja_deps` binary file, which Meson/ninja generate as part of build tracking. Ninja folds per-object Makefile-style dependency output into this single file and deletes the per-object sidecars; no extra flag is required.

Buildtype `debugoptimized` gives `-O2 -g`; `-O1` is the minimum for IPA passes (devirt, ICF, inline) to fire meaningfully. `--buildtype=debug` (`-O0`) will produce a valid index but with empty devirt hints and reduced IPA data.

### Link-side flags (recommended)

For every executable and shared library the user wants herbarium to fully profile, add a linker map by specifying the map filename verbatim per target:

```meson
executable('myapp', myapp_srcs, link_args: ['-Wl,-Map=myapp.map'])
executable('other', other_srcs, link_args: ['-Wl,-Map=other.map'])
```

With the ninja backend, the link command runs from the top of the build directory, so `-Wl,-Map=myapp.map` produces `<builddir>/myapp.map`. Herbarium looks for `<target-name>.map` at the top of the builddir first, then alongside the target binary.

Herbarium degrades gracefully if maps are missing: `link_resolutions` falls back to `nm`-based inference, which correctly identifies winning weak symbols in most cases but cannot report losing definitions or archive-member selection.

### Build

```bash
meson compile -C builddir
```

Every `.o` will now be accompanied by `<obj>.ci`, `<source>.NNNi.cgraph`, `<source>.NNNi.inline`, `<source>.NNNi.devirt`, `<source>.NNNi.icf`, `<source>.opt-record.json.gz`, and a `<obj>.d` dependency file (which Meson/ninja emit unconditionally). Linked executables and shared libraries will exist as usual, with DWARF preserved.

### Validation before indexing

`herbarium collect` performs a preflight check on the builddir and refuses to proceed if any of the following are missing:

- `meson-info/intro-targets.json` (Meson has been configured).
- `<obj>.ci` for at least one `.o` (`-fcallgraph-info` was supplied).
- `<source>.NNNi.cgraph` (IPA cgraph dump was supplied).
- `<source>.opt-record.json.gz` (`-fsave-optimization-record` was supplied). Without it the index sees only IPA-stage inlining, and nothing in the index records that the view is partial.
- `-g` was in effect (checked by inspecting DWARF sections of a sample `.o`).

The preflight report tells the user exactly which flag is missing and prints the corrected `meson setup` command.

### One-shot invocation

Once the build is complete:

```bash
herbarium collect --builddir path/to/builddir \
             --project-root path/to/source \
             --out herbarium.hbr
```

## What GCC and the linker give us (concrete)

Files the user's build produces, which herbarium ingests:

| Enabled by | Output | What herbarium extracts |
|---|---|---|
| `-fcallgraph-info=su,da` | `<obj>.ci` | Direct call edges, stack usage, data-area sizes per function |
| `-fdump-ipa-cgraph` | `<obj>.NNNi.cgraph` | Post-IPA callgraph; `address_taken`, `only_called_directly`, linkage flags; indirect call site list |
| `-fdump-ipa-inline` | `<obj>.NNNi.inline` | IPA-stage inlining decisions |
| `-fsave-optimization-record` | `<obj>.opt-record.json.gz` | Gzipped JSON: every pass's inlining decisions, early inliner included, rejections with reasons |
| `-fdump-ipa-devirt` | `<obj>.NNNi.devirt` | Speculative devirtualization (limited in C, useful when it fires) |
| `-fdump-ipa-icf` | `<obj>.NNNi.icf` | Identical-code-folded functions (shared linkage address) |
| `-g -gcolumn-info` | DWARF in `.o` | Symbol identity, decl file/line, def file/line, signatures, typedef chains, struct field names, `DW_TAG_call_site`, `DW_TAG_inlined_subroutine` |
| Meson/ninja default | `<obj>.d` | Make-format dependency file — transitive header list per TU (no extra flag needed) |

Per `.o` and per linked target, herbarium invokes the following read-only inspectors during `herbarium collect` (never during the user's build, never during `herbarium serve`):

| Tool herbarium invokes | What we get |
|---|---|
| `nm --defined-only --format=posix` | Symbols with linkage kind (T/W/V/C) |
| `nm -u` | Undefined symbols (dep discovery) |
| `objdump -d --demangle --no-show-raw-insn` | Every direct/indirect call in the shipped binary, symbolicated via relocations |
| `readelf --debug-dump=info` | DWARF from `.o` files (symbol identity, decl/def, signatures, `DW_TAG_call_site`) |

Plus, when the user has enabled linker maps via `-Wl,-Map=`, herbarium reads (does not generate) the `.map` files:

| File herbarium reads | What we get |
|---|---|
| `<target>.map` | Which object supplied each symbol, weak-symbol winners, archive-member selection, dead sections |

Build-system introspection — herbarium reads persisted files under `builddir/meson-info/`; it does not invoke `meson` or `ninja`:

| File herbarium reads | What we get |
|---|---|
| `meson-info/intro-targets.json` | Executables/libs, kind, source list per target, link command |
| `meson-info/intro-dependencies.json` | External library deps |
| `meson-info/intro-buildoptions.json` | Build config for reproducibility |
| `.ninja_deps` (parsed directly) | Object → header dependency graph for incremental re-ingest |

## Database schema

Single SQLite file, `mode=ro` at serve time, opened WAL at build time.

```sql
-- Reproducibility & versioning
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
-- schema_version, gcc_version, meson_version, build_config_hash,
-- project_root_hint, indexed_at, herbarium_version

-- Build-system view
CREATE TABLE targets (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE,
  kind TEXT,            -- 'executable' | 'static_library' | 'shared_library'
  link_command TEXT
);

CREATE TABLE target_sources (
  target_id INTEGER REFERENCES targets(id),
  file TEXT             -- project-relative
);
CREATE INDEX idx_ts_target ON target_sources(target_id);
CREATE INDEX idx_ts_file ON target_sources(file);

-- Source content, content-addressed and deduplicated
CREATE TABLE blobs (
  hash TEXT PRIMARY KEY,   -- SHA-256 hex of raw bytes
  size INTEGER,
  content BLOB             -- zstd-compressed
);

CREATE TABLE sources (
  path TEXT PRIMARY KEY,   -- project-relative
  blob_hash TEXT REFERENCES blobs(hash),
  is_generated INTEGER
);

-- Symbols: identity only. One row per USR. See Appendix:
-- Symbols and definitions for why identity is split from definitions.
CREATE TABLE symbols (
  id INTEGER PRIMARY KEY,
  usr TEXT UNIQUE,         -- 'c:@F@name' (external) | 'c:<path>@F@name' (static)
  name TEXT,
  kind TEXT,               -- 'function' | 'variable' | 'typedef' | ...
  linkage TEXT,            -- 'external' | 'internal' | 'weak' | 'common' (aggregated)
  signature TEXT,          -- reconstructed from DWARF
  address_taken INTEGER,   -- 0/1 aggregated across all TUs where defined
  linkage_names TEXT       -- JSON array of every link-time name that resolves to this USR,
                           -- including GCC clone suffixes like '.constprop.0', '.isra.1'.
                           -- Objdump-side symbolication looks up by these names; USR stays
                           -- source-anchored. See Appendix: GCC-generated clones.
);
CREATE INDEX idx_sym_name ON symbols(name);
CREATE INDEX idx_sym_kind ON symbols(kind);
CREATE INDEX idx_sym_addr_taken ON symbols(address_taken);
CREATE VIRTUAL TABLE symbols_fts USING fts5(
  name, signature, content='symbols', content_rowid='id',
  tokenize='unicode61 separators _'
);

-- One row per observed definition of a symbol. A single USR may have
-- multiple defs across TUs: weak/strong overrides, multi-executable
-- `main`, static-inline in headers. Declarations without a def in any
-- indexed TU (e.g., libc's `printf`) contribute zero rows here — the
-- `symbols` row (the identity) is still one and edges resolve to it.
CREATE TABLE symbol_definitions (
  id INTEGER PRIMARY KEY,
  symbol_id INTEGER REFERENCES symbols(id),
  file TEXT,               -- def file, project-relative
  line INTEGER,
  decl_file TEXT,          -- from DWARF; '' if same as file
  decl_line INTEGER,       -- 0 if same as line
  is_weak INTEGER,         -- 0/1 — this specific def has weak linkage
  linkage_name TEXT        -- link-time name of this def (mostly = symbols.name;
                           -- differs for GCC clones like 'foo.constprop.0')
);
CREATE INDEX idx_sd_symbol ON symbol_definitions(symbol_id);
CREATE INDEX idx_sd_file ON symbol_definitions(file);

-- Direct call edges, from two independent sources
CREATE TABLE call_edges (
  caller_id INTEGER REFERENCES symbols(id),
  callee_id INTEGER REFERENCES symbols(id),
  source TEXT,             -- 'compiler_cgraph' | 'objdump'
  target_id INTEGER        -- NULL for compiler_cgraph; set for objdump
);
CREATE INDEX idx_ce_caller ON call_edges(caller_id, source);
CREATE INDEX idx_ce_callee ON call_edges(callee_id, source);

-- Indirect call sites, seen by the compiler
CREATE TABLE indirect_call_sites (
  id INTEGER PRIMARY KEY,
  caller_id INTEGER REFERENCES symbols(id),
  file TEXT,
  line INTEGER,
  column INTEGER,          -- when available from DWARF
  callee_type TEXT,        -- callee signature from DWARF, rendered like symbols.signature
  field_hint TEXT          -- 'struct_t.field' / global / param name, else ''
);
CREATE INDEX idx_ics_caller ON indirect_call_sites(caller_id);
CREATE INDEX idx_ics_type ON indirect_call_sites(callee_type);

-- Compiler-reported speculative resolutions of indirect calls
CREATE TABLE devirt_hints (
  site_id INTEGER REFERENCES indirect_call_sites(id),
  callee_id INTEGER REFERENCES symbols(id),
  confidence TEXT          -- 'speculative' | 'resolved'
);

-- Inlining, three planes that answer three different questions.
-- inline_decisions is the .cgraph per-edge tag: IPA-stage only.
CREATE TABLE inline_decisions (
  caller_id INTEGER REFERENCES symbols(id),
  callee_id INTEGER REFERENCES symbols(id),
  inlined INTEGER          -- 0/1
);

-- inline_records is the decision plane, from -fsave-optimization-record:
-- every pass's verdict, rejections and reasons included. 'einline' rows
-- come from the early inliner, which runs before IPA and is invisible to
-- every -fdump-ipa-* dump. A row here is not proof the code survived.
CREATE TABLE inline_records (
  caller_id INTEGER REFERENCES symbols(id),
  callee_id INTEGER REFERENCES symbols(id),
  pass TEXT NOT NULL,      -- 'einline' | 'inline'
  inlined INTEGER NOT NULL,-- 0/1
  reason TEXT,             -- GCC's own words when inlined = 0
  file TEXT, line INTEGER, column INTEGER,
  object TEXT
);
CREATE INDEX idx_ir_caller ON inline_records(caller_id, inlined);
CREATE INDEX idx_ir_callee ON inline_records(callee_id, inlined);

-- inline_instances is the outcome plane, from DWARF
-- DW_TAG_inlined_subroutine: bodies that actually made it into the
-- object's code, whichever pass put them there. A callee that folds to a
-- constant after inlining leaves no DIE, so this plane under-reports
-- rather than over-reports.
CREATE TABLE inline_instances (
  callee_id INTEGER REFERENCES symbols(id),
  caller_id INTEGER REFERENCES symbols(id),
  parent_callee_id INTEGER REFERENCES symbols(id), -- NULL at depth 1
  depth INTEGER NOT NULL,
  file TEXT, line INTEGER, column INTEGER,
  object TEXT
);
CREATE INDEX idx_ii_caller ON inline_instances(caller_id);
CREATE INDEX idx_ii_callee ON inline_instances(callee_id);

-- Linkage-time truth
CREATE TABLE link_resolutions (
  target_id INTEGER REFERENCES targets(id),
  usr TEXT,
  winning_object TEXT,
  linkage_kind TEXT,       -- 'strong' | 'weak' | 'unique_global' | 'common'
  losing_objects TEXT,     -- JSON array
  archive TEXT             -- containing archive if pulled from .a, else ''
);
CREATE INDEX idx_lr_usr ON link_resolutions(usr);
CREATE INDEX idx_lr_target ON link_resolutions(target_id);

-- Reachability from entry points (link-time GC)
CREATE TABLE symbol_reachability (
  target_id INTEGER REFERENCES targets(id),
  symbol_id INTEGER REFERENCES symbols(id),
  reachable INTEGER,       -- 0 = dead-stripped
  section_kept INTEGER
);
```

Only facts a compiler or linker deterministically emits appear as columns. No categorization we cannot back with a compiler-issued flag.

## MCP tools

Grouped by workflow. Every tool's contract is derived from schema-level facts, so descriptions can be precise and stable.

### Convention: locations and snippets

Every tool response that references a code location returns a uniform shape:

```
location: {
  path: "src/net/conn.c",       // project-relative; always exists in the blob store
  line: 142,
  column: 9,                    // when available from DWARF
  blob_hash: "sha256:…",        // hash of the file's indexed content
  snippet: {                    // optional; on by default for describe_* and list_* tools
    start_line: 138,
    end_line: 148,
    text: "…"                   // ±5 lines of surrounding source
  }
}
```

The `path` is always the exact string an agent should quote back to the user, and it resolves in the user's live checkout when rooted at the same project directory. The `blob_hash` lets the agent (or the user) compare against a live-file hash to detect drift. The agent is expected to hand the user `path:line` references it received here as its citation format.

When `herbarium serve` is launched with `--project-root <path>`, responses additionally include `absolute_path`, and a set of drift-detection tools become available (see below). Without `--project-root`, the serve process has no filesystem access outside the DB — appropriate for shipping an `herbarium.hbr` as a self-contained artifact.

### Source reference

**`read_source(path, start_line?, end_line?)`** — retrieve source content from the embedded blob store, optionally sliced by line range. Returns `{path, blob_hash, line_count, content}`.
*Benefit:* the agent can pull any indexed file into context to reason about it or to quote it back to the user. Because every location returned elsewhere in the tool set points at a path that is guaranteed to exist here, the agent can walk from any query result to the surrounding source with zero indirection.

**`list_source_files(target?, path_prefix?, kind?)`** — enumerate every file the index has content for; filter by target membership, path prefix, or file kind (source/header/generated). Returns `{path, blob_hash, size, targets: [name, …]}`.
*Benefit:* the agent's directory browser. Combined with `read_source`, it can navigate the project as it existed at index time even from a machine that never had the checkout.

**`verify_source(path, expected_hash?)`** — check whether a file's indexed content matches an expected hash. When `expected_hash` is supplied, returns `{path, matches: bool, indexed_hash}`. When omitted and the server was launched with `--project-root`, herbarium hashes the live file and reports the comparison directly.
*Benefit:* before the agent recommends an edit at `foo.c:142`, it can confirm the user's live `foo.c` still matches what herbarium collected. Prevents the agent from citing a line number that has shifted in the meantime.

**`list_source_drift(target?, path_prefix?)`** — only when `--project-root` is set. Walks the live checkout and returns every file whose live content differs from the indexed blob. Returns `[{path, indexed_hash, live_hash, live_missing: bool}, …]`.
*Benefit:* the agent can announce "I'm working from an index that predates changes to these 12 files; results in that region may be stale" — turning a hidden failure mode into a visible one.

### Target navigation

**`list_targets`** — enumerate every executable/library in the project with kind, source count, and link status.
*Benefit:* the agent's first move — knowing what actually gets built and what "the codebase" is partitioned into. Answers "how many binaries does this project produce and which sources are shared."

**`describe_target(name)`** — full profile: sources (each with `path` + `blob_hash`), link command, dependencies, linked-in archives, entry points.
*Benefit:* the agent can scope any later query to one binary and reason about that binary's link closure specifically. Source list uses the same `path` values the rest of the tool set returns.

### Symbol lookup

**`find_symbol(query, kind?, target?)`** — FTS over name and signature; supports substrings and identifier-boundary matches; scopeable by target.
*Benefit:* fast fuzzy entry point when the agent only knows part of a name or is looking by signature shape.

**`describe_symbol(usr)`** — canonical profile: name, kind, linkage, decl location, def location, signature, address-taken flag, which targets it's linked into, which linker resolution won.
*Benefit:* one-shot understanding of a symbol including its link-time reality, not just its source declaration.

### Call graph — source view

**`list_callers(callee_usr, target?)`** — who calls this symbol, from GCC's callgraph info (pre-optimization, source-truth).
*Benefit:* answers "who calls X in the source as written," independent of what the optimizer did.

**`list_callees(caller_usr, target?)`** — direct calls out of this function per GCC's callgraph.
*Benefit:* mirror of the above; understand what a function reaches at source level.

**`list_call_paths(from_usr, to_usr, max_depth=6, target?)`** — enumerate direct-call paths between two symbols.
*Benefit:* reachability answers. Useful for security ("can untrusted input reach the crypto routine?") and refactoring ("what's between these two functions?").

### Call graph — runtime view

**`list_linked_callers(callee_usr, target)`** — callers per `objdump` of the shipped binary; post-inlining, post-optimization.
*Benefit:* different answer than `list_callers` when inlining is aggressive. Ground truth for "what actually calls X at runtime."

**`list_linked_callees(caller_usr, target)`** — outgoing calls per `objdump`.
*Benefit:* symmetric ground truth.

**`describe_inlining(caller_usr, limit?, include_snippets?)`** — what happened to this function's calls, across all three inlining planes: `records` (every pass's decision, rejections and reasons included, from the optimization record), `instances` (the inlined bodies DWARF says survived into the object), `cgraph_edges` (the older `.cgraph` per-edge tag, IPA-stage only, kept as a cross-check rather than an answer). Summary-first: `summary` aggregates every matching row (totals, by pass, by inline depth) while the arrays are capped, because a heavily inlined caller has enough rows to exceed an MCP client's output limit and get the whole response truncated — a cap the tool controls beats a truncation it does not.
*Benefit:* reconciles the two callgraph views so the agent knows whether a source-level edge exists in the binary — and, when the planes disagree, says which stage lost it.

**`list_inline_instances(callee_usr)`** — the reverse: everywhere this function's body was inlined into another.
*Benefit:* answers "where did this helper go?" for a symbol with no runtime callers. A function with instances but no linked callers was inlined everywhere, not dead.

**`explain_call(caller_usr, callee_usr, target?, limit?, include_snippets?)`** — one verdict for one call, plus the evidence: `inlined_and_present`, `inlined_then_folded`, `declined` (with GCC's own reason), `no_decision_logged`, or `mixed` when a USR's TUs decided differently.
The verdict is decided from the full row set even when the echoed evidence is capped; a verdict derived from truncated rows could name the wrong outcome.
*Benefit:* the three planes are the right shape for storage and the wrong shape for a consumer — an agent asking about a single call should not have to know GCC's pass pipeline to route its own question. Verdicts are per-object, joined on the `.o`, so "inlined then folded" is never confused with "the body landed in another TU".

### Indirect calls and function-pointer dispatch

**`list_indirect_call_sites(caller_usr?, callee_type?, target?)`** — sites GCC recorded as indirect, with location, callee type, and DWARF-derived field hint when available.
*Benefit:* locates every dispatcher and callback invocation in the codebase. Answers "where does this code do dynamic dispatch."

**`list_address_taken_functions(fn_ptr_type?, target?)`** — functions whose address is taken somewhere, filterable by canonical fn-pointer type.
*Benefit:* candidate set for what an indirect call could reach. Combined with a type filter, this narrows dispatch resolution dramatically for well-typed callback tables.

**`resolve_indirect_call(site_id)`** — combines type-compatibility narrowing (from `symbols` + `fn_ptr_type`) with GCC's `-fdump-ipa-devirt` hints (from `devirt_hints`) into a ranked candidate list, tagged by evidence source.
*Benefit:* the direct answer to "what could this indirect call be calling," using only what the compiler already knows.

**`list_devirt_hints(target?)`** — everywhere GCC's speculative devirtualization pass resolved an indirect call to a specific target.
*Benefit:* high-confidence indirect resolutions the agent can trust without heuristics.

### Linkage and weak symbols

**`describe_link_resolution(usr, target)`** — which object won, which lost, which archive it came from, linkage kind.
*Benefit:* the only reliable way to know which `malloc` (libc's or jemalloc's) actually runs in this binary.

**`list_weak_symbols(target?)`** — every weak-linkage definition with its resolution status.
*Benefit:* security and correctness audits where symbol interposition matters (LD_PRELOAD candidates, override points).

**`list_undefined_symbols(target)`** — externals this target imports but does not define; grouped by likely providing library.
*Benefit:* dependency mapping without reading Makefiles; enables "what does this binary rely on from libc" queries.

**`list_icf_groups(target?)`** — functions merged by identical-code folding, keyed by winning symbol.
*Benefit:* explains why two source-distinct functions share an address at runtime — surprises the agent otherwise misdiagnoses.

### Reachability and dead code

**`list_unreachable_symbols(target)`** — symbols the linker garbage-collected (or would, under `--gc-sections`).
*Benefit:* dead-code review; refactoring signal.

**`list_entry_points(target)`** — `main`, exported symbols, initializer arrays, constructor-attributed functions.
*Benefit:* the root set for any reachability question.

### Introspection escape hatch

**`describe_schema`** — full schema with join recipes and enum meanings.
*Benefit:* lets the agent construct arbitrary queries via `sql_query` when the tool set doesn't cover its question directly.

**`sql_query(sql)`** — read-only SQL against the index. Enforcement at the driver level via `?mode=ro`.
*Benefit:* future-proof escape hatch. The agent can answer questions the tool designer never anticipated as long as the underlying facts are in the schema.

## Implementation phases

### Phase 0 — Validation (1–2 days, before any production code)

- On a real Meson build, hand-add the dump flags to `c_args` per the Prerequisites section for one target.
- Verify each dump file lands with the expected format on the pinned GCC version.
- Sample a TU: confirm `.cgraph` lists indirect call sites with types; confirm DWARF has field names for one struct-of-fn-pointers; confirm `.ci` matches expected direct edges; confirm `-Wl,-Map=` output is well-formed for at least one linked target.
- Measure build-time overhead of the extra dumps.
- **Preserve samples as fixtures.** Copy one representative `<obj>.ci`, `<source>.NNNi.cgraph`, `<source>.NNNi.inline`, `<source>.NNNi.devirt`, `<source>.NNNi.icf`, `<obj>.d`, and `<target>.map` from the validation build into `testdata/samples/gcc-<version>/` and check them into the repo. Phases 2 and 4 develop parsers against these fixtures instead of re-collecting samples on every parser change; the samples also anchor the pinned GCC version at code-review time.
- **Exit criterion:** every planned dump is present, parseable, adds under ~25% build overhead, and sample fixtures are checked in.

### Phase 1 — Scaffolding (~2 days)

- `cmd/herbarium` binary skeleton: `collect`, `serve` subcommands.
- `internal/mesonintrospect/` — read `meson-info/intro-targets.json` and `intro-dependencies.json` from the builddir (no invocation of `meson introspect` needed; Meson persists these on `setup`).
- `internal/builddir/` — builddir crawler: given a builddir, enumerate `.o` files and locate the sibling `.ci`, `.NNNi.cgraph`, `.NNNi.inline`, `.NNNi.devirt`, `.NNNi.icf`, `.i`, and any `.map` files. Skip `builddir/meson-private/` — Meson drops sanity-check compile artifacts there that must not be ingested as real TUs. Dump filenames on GCC 16 land as `<obj-basename>.c.NNNi.<pass>`; parsers glob on the suffix rather than hardcoding pass numbers.
- `internal/preflight/` — validate that all required artifacts are present; report specific missing flags with a suggested `meson setup` command line.
- `internal/store/` — schema init, connection lifecycle, `mode=ro` for serve.
- `internal/blobstore/` — content-addressed blob writer with zstd.

### Phase 2 — Compiler-side ingest (~4 days)

- `internal/gccdump/ci.go` — parse `-fcallgraph-info` output.
- `internal/gccdump/cgraph.go` — parse `-fdump-ipa-cgraph`: functions, flags, direct edges, indirect call sites (count only — no source location; that comes from DWARF in Phase 3), clone attribution to parent USR.
- `internal/gccdump/inline.go` — parse `-fdump-ipa-inline` for inline decisions (supplementary; `.cgraph`'s `(inlined)` tag on the `Called:` line is the primary source). Both are IPA-stage views.
- `internal/gccdump/optrecord.go` — parse `<obj>.opt-record.json.gz` (`-fsave-optimization-record`). Keeps the records whose pass carries the `inline` optgroup — the same selector `-fopt-info-inline` uses — so the vectorizer records in the same file are dropped without naming them. The two node references in a message are ordered by the message's own wording (`Inlined <callee> into <caller>` vs `not inlinable: <caller> -> <callee>`), so the parser reads the separator rather than assuming a position, and drops any record matching neither: a reversed inline edge would be worse than a missing one.
- `internal/gccdump/icf.go` — parse `-fdump-ipa-icf` for folded groups (returns empty on fixtures without folding; Phase 8's fixture forces ICF to fire).
- `internal/gccdump/devirt.go` — parse `-fdump-ipa-devirt` for speculative resolutions. Almost always empty in pure C — `devirt_hints` table stays empty until we see a firing case.
- `internal/usr/` — USR synthesis per the appendix. Handles GCC clone suffixes (`.constprop.N`, `.isra.N`) by aliasing to the parent's USR and recording the linkage name on the parent.
- `internal/ingest/` orchestrator: two-phase per-TU aggregation (non-clones first, then clones), cross-TU merge, edge resolution via per-TU local-id → USR maps, cross-TU edge dedup (multi-executable `main` collapses to one identity — see Appendix: Symbols and definitions).
- Populate `symbols`, `symbol_definitions` (identity + per-def location), `call_edges (source='compiler_cgraph')`, `inline_decisions`, `inline_records`. The optimization record joins by cgraph node order — the same local ids `.cgraph` uses — which is what lets a clone (`use_dispatch.constprop/12`) resolve to its parent's USR; a name lookup could not. `indirect_call_sites` and `devirt_hints` are populated in Phase 3 (DWARF adds file/line/column that `.cgraph` lacks).

### Phase 3 — DWARF ingest (~3 days)

- `internal/dwarfingest/` — read DWARF from each `.o` via `debug/elf` + `debug/dwarf`. Extracts subprograms with signatures (walking DW_AT_type refs for return + params), struct/typedef/variable DIEs, `DW_TAG_call_site` entries with source-caller attribution via the inlined-subroutine chain, and `DW_TAG_inlined_subroutine` entries as inline instances in their own right (callee via `DW_AT_abstract_origin`, caller and nesting depth from the DIE stack, call site from `DW_AT_call_file/line/column`).
- Populate `symbols.signature` and `symbol_definitions.decl_file/decl_line` (UPSERT — Phase 2 already inserted the identity row; Phase 3 enriches). Owns `indirect_call_sites` insertion with `file`/`line`/`column` resolved via `LineReader.SeekPC(call_return_pc-1)` — GCC 16 puts `DW_AT_call_file/line/column` on the enclosing `DW_TAG_inlined_subroutine`, not on the `DW_TAG_call_site`. Also owns `inline_instances`: the same `DW_TAG_inlined_subroutine` DIEs, read as facts rather than as traversal, which is the only plane that sees the early inliner's work.
- Struct field DIEs are parsed (name + rendered type per member) but not persisted — the plan schema has no fields table currently. Add one when a downstream tool needs to query by struct field.
- Typedef DIEs are captured with their target type as a rendered string; no full canonicalization to underlying base types (a `size_t` stays `size_t` rather than becoming `unsigned long`).
- Signature rendering keeps C's three empty-ish parameter lists apart, since `callee_type` is matched against `symbols.signature` by equality: `(void)` for a prototyped no-argument function, `()` for a non-prototyped declaration (`DW_TAG_unspecified_parameters` without `DW_AT_prototyped`, whose arguments are unchecked and default-promoted), and a trailing `, ...` for a genuine variadic. `symbols.signature` and a fn-pointer's rendering share one code path so the two always agree.
- `internal/dwarfingest/calltarget.go` resolves `indirect_call_sites.callee_type` and `.field_hint` from two sources, because GCC emits only one of them per site. Where the callee address is still describable at the return PC — a call through a function-pointer *parameter* — GCC emits `DW_AT_call_target`; the decoder handles its register forms (`DW_OP_reg*`, `DW_OP_regx`, and the `DW_OP_entry_value` wrapper) and maps the register back to a formal parameter by *replaying* the x86-64 SysV integer-register assignment. The register's ordinal is not a parameter index — an SSE-class argument consumes no integer register, so `long f(double, unary, binary, long)` puts `second` in `rsi` where naive indexing would name `first` — so the replay stops at the first parameter that isn't a single-integer-register type and the site resolves to nothing. A large aggregate return (hidden `sret` pointer in `rdi`) declines the whole shape for the same reason. Where it doesn't — the dispatch-table shape `g_ops.add(...)`, whose loaded pointer is dead by the return PC — there is no `DW_AT_call_target` at all, so the resolver reads the call instruction's relocation instead: on x86-64 `call *disp(%rip)` ends with the 4-byte displacement, so the `R_X86_64_PC32` entry sits at `return_pc-4` and `addend+4` is the exact byte offset into the table symbol, which the struct's `DW_AT_data_member_location` turns into a member. Both routes end at a type DIE, rendered in `symbols.signature` form so `callee_type` joins by equality. Both routes are gated on x86-64 — `argRegOrder` is an x86-64 ABI table and DWARF register numbers are per-architecture, so applying it to AArch64 (whose `x1` is DWARF register 1, `rdx`'s slot) would name the wrong parameter rather than fail — and both self-check: anything that doesn't bottom out at a pointer-to-subroutine leaves both columns empty rather than guessing.

### Phase 4 — Link-plane ingest (~4 days)

- `internal/linkplane/` — invoke `nm` (POSIX defined + undefined) and `objdump -d --demangle --no-show-raw-insn` as subprocesses against each linked target discovered via Meson introspection; parse their stdout. Also parse any `.map` files the user configured. (`readelf` isn't currently needed — Phase 3 already reads DWARF directly via `debug/dwarf`.)
- `internal/ingest/targets.go` — populates the `targets` and `target_sources` tables from Meson introspection. Runs before the link pass so `link_resolutions.target_id` and `call_edges.target_id` have valid references.
- `internal/ingest/link.go` — per-target orchestrator. For each executable and shared library: nm on the binary + map file (when present) + objdump for direct-call edges. Populates `link_resolutions`, `call_edges (source='objdump')`, `symbol_reachability`.
- Resolve weak-symbol winners and archive-member selection via the map file's section table (symbol → contributing `.o`) and top-level load list (which `.o` was pulled from which archive). Archive attribution uses Meson's convention: an object at `<dir>/<name>.a.p/*.o` was bundled into `<dir>/<name>.a`. Without a map, `winning_object` is picked heuristically from a per-.o nm scan across the builddir (strong > weak > local, deterministic tie-break) — same-named statics across TUs of one target cannot be disambiguated by address without a map, so those fall back to name lookup.
- Symbolicate direct calls from objdump to USRs via linkage-name lookup: build a `nameToID` map from `symbols.name` plus every entry in the JSON `symbols.linkage_names` array. PLT/version decorations (`printf@plt`, `printf@GLIBC_2.2.5`) get stripped before lookup.
- `link_resolutions.losing_objects` is populated from the per-.o nm scan (candidates minus winner), so it lists every other .o in the builddir that also defines the symbol. Broader than a strict map-file impl: includes archive members ld may never have pulled in — read it as "other .o's that could have supplied this symbol", not "candidates ld weighed and rejected".
- `symbol_reachability.section_kept` is always 1 in the current pass. Populating it correctly requires parsing the "Discarded input sections" block of the map file; deferred until Phase 8's fixture exercises `-ffunction-sections -Wl,--gc-sections`.
- `symbol_reachability.reachable` is derived from nm on the binary: a symbol is reachable iff `nm --defined-only` lists it. That catches fully-inlined-away symbols (like the fixture's `use_dispatch`) and dynamic-only externals (`printf`) as unreachable.
- Every invocation is read-only against already-built artifacts; nothing is rebuilt or modified.

### Phase 5 — Source packing (~2 days)

- `internal/blobstore/` — already exists from Phase 1 with `Put(path, content, isGenerated) → (PutResult, error)`: SHA-256 keyed, zstd-compressed, dedup by content hash, transactional. Phase 5 wires it up; do not rewrite it.
- Iterate every source file listed in `meson-info/intro-targets.json` (paths in the target's `sources` array plus its `generated_sources` array — mesonintrospect already surfaces both). Then enumerate every header referenced per-TU by parsing `builddir/.ninja_deps` — ninja's binary dep log; the plan says the tool never invokes ninja, so `ninja -t deps` is off the table.
- **`.ninja_deps` format:** binary record-oriented log documented in ninja's `src/deps_log.{h,cc}`. Header magic `# ninjadeps\n` + version u32 + records. Two record types: paths (variable-length name + checksum) and deps (target-path-id + mtime + dep-path-ids). No Go stdlib parser exists; hand-roll. Reference: <https://github.com/ninja-build/ninja/blob/master/src/deps_log.h>.
- Read each file from disk (rooted at `--project-root`), content-hash, dedup via `blobstore.Put`, and insert one `sources` row per project-relative path pointing at its blob. Set `is_generated=1` for anything that came from mesonintrospect's `generated_sources` array or lives under the builddir; `0` otherwise.
- Refuse to index if a referenced source file is not present under `--project-root` — catches builddir/checkout mismatches at ingest time rather than at query time. This is the same rule the USR appendix (line 550) applies for identity coverage.
- Optional `herbarium collect --strict` (per Risks section) refuses to package sources whose `mtime` is newer than the corresponding `.o`; catches the "user edited after building but before indexing" case.
- **Pipeline hook point:** add `internal/ingest/sources.go` with a `Sources(db, bd, intro, pr) (SourcesSummary, error)` function. Call it from `cmd/herbarium/collect.go` after `ingest.Link(...)` and before the summary print, so the .hbr contains blobs + links + everything else in one transaction-per-pass sequence. Runs in its own transaction like the other passes.

### Phase 6 — MCP layer (~3 days) — landed

- `internal/mcp/` — every tool from § MCP tools registered via `github.com/mark3labs/mcp-go`. Tool groups live in sibling files (`schema_tool.go`, `sql_tool.go`, `source_tools.go`, `target_tools.go`, `symbol_tools.go`, `callgraph_source_tools.go`, `callgraph_runtime_tools.go`, `indirect_tools.go`, `linkage_tools.go`) with a shared `Location`/`Snippet` helper in `location.go`.
- Stdio and Streamable HTTP transports (`serve --transport stdio|http --http-addr`); stdio prints banners to stderr so JSON-RPC framing on stdout stays clean.
- Snippet extraction on demand from the blob store for every location-returning tool (±5 lines, uniform `Location` shape carrying `path`, `line`, `column`, `blob_hash`, `snippet`, and `absolute_path` when `--project-root` is set).
- `--project-root` option on `serve` enables `verify_source` live-hashing and `list_source_drift`; adds `absolute_path` to responses. Without it, responses are self-contained against the .hbr.
- `describe_schema` returns embedded schema + closed-vocabulary enum glossary + canonical join recipes; `sql_query` uses the read-only driver mode (`mode=ro + query_only(1)`) so writes are rejected at the SQLite layer with a "readonly" error rather than by application-side parsing.
- Known gaps left for later work (documented in the tool descriptions themselves): `list_entry_points` doesn't yet classify constructor-attributed or `.init_array` entries; `resolve_indirect_call` still falls back to the full address-taken pool for sites where neither `DW_AT_call_target` nor a call-instruction relocation names a typed target (computed pointers, non-x86-64 objects). On inlining, no single plane is complete: `inline_records` misses the source location on some records and cannot say whether the inlined code survived, and `inline_instances` misses any callee whose copy folded to a constant afterwards. They are kept separate rather than merged because the disagreement is the signal.

### Phase 7 — Incremental re-ingest — **deferred**

Skipped by user decision after Phase 6. `herbarium collect` always writes a fresh index and refuses to overwrite an existing `.hbr`; users delete + re-run for a rebuild. If reopened later, the intended shape is:

- `internal/incremental/` — compare `mtime` and content hash of each `.o` against the previous run's meta entry.
- `herbarium collect --incremental` — only re-ingest TUs whose `.o` changed; preserve untouched rows.
- Blob store dedup handles source changes automatically; sources whose hash is unchanged skip re-compression.

### Phase 8 — Tests and fixtures — landed

- Fixture C project under `testdata/fixture/` exercises:
  - Multiple executables sharing a library (`app1`, `app2`, `shared`).
  - A weak-symbol override (`lib/weak_impl.c` fallback + `app1/strong_override.c` strong).
  - A dispatch table (`struct ops g_ops` in `include/dispatch.h` + `lib/dispatch_impls.c`).
  - An indirect call through a `const` dispatch table (`use_dispatch` calls `g_ops.add` and `g_ops.mul`).
  - A pair of syntactically-different but semantically-identical helpers (`lib/icf_pair.c`) that GCC's `-fipa-icf` folds (dump reports `Equal symbols: 1` + `<name>.localalias` node).
  - A dead-stripped symbol (`never_called`) under `-ffunction-sections -fdata-sections -Wl,--gc-sections` (`reachable=0` after linking).
  - A GCC clone (`use_dispatch.constprop.0`) recorded in `symbols.linkage_names`.
  - An early-inlined helper (`scale_by_two`, `always_inline`, folded into `scaled_compute` in `lib/shared_utils.c`) — the pre-IPA fold that appears in `inline_records`/`inline_instances` and in neither `inline_decisions` nor any `-fdump-ipa-*` dump.
- Golden-file tests for each ingest module land in the module's own `_test.go` under `internal/gccdump/`, `internal/dwarfingest/`, `internal/linkplane/`, and `internal/ninjadeps/`. Sample dumps live under `testdata/samples/gcc-<version>/`.
- End-to-end coverage: `TestE2EFixtureContract` in `internal/mcp/e2e_test.go` boots an in-process MCP client against a freshly-collected `.hbr` and walks 16 tools, asserting the fixture's full contract (multi-def hook, clone linkage names, source-vs-runtime call graph asymmetry from inlining, all three inlining planes including the early-inline fold, dead-strip + inline reachability=0, undefined externals, per-target link resolution).

**Total estimated effort:** ~3.5 weeks of focused work.

## Out of scope (deliberate)

- **Source-syntactic address-take classification** (e.g., "compared vs stored-in-field vs passed-as-argument"). Recovering this requires parsing the C AST, which pulls in either a compiler plugin or a source parser. Both compromise the "no reparse, no plugin" principle. Herbarium exposes the compiler's `address_taken` fact and the fn-pointer type; type-based candidate narrowing covers most dispatcher-discovery workflows without it. If the categorization becomes essential later, it can be added as a distinct phase with its own contract.
- **Incremental in-editor updates.** Herbarium is build-anchored: source changes don't affect the index until the source rebuilds. This is a deliberate trade for compiler-truth semantics.
- **Non-GCC compilers.** Clang, MSVC, and IAR are out of scope by design. A parallel tool for a different compiler is a separate project.
- **Non-Meson build systems.** CMake, Bazel, plain Make each require their own introspection layer. Meson-first ships; other build systems are follow-on work with the same shape (introspect → wrap CC → post-link).
- **VCS-aware or symbol-blame features.** The index is a build-time snapshot; git history is a separate axis.

## Risks and mitigations

**GCC dump format drift across versions.** Textual dumps are stable within a major version but can change across them.
*Mitigation:* pin a supported GCC version range in `meta`. Parser version-gates on GCC major. CI tests against every supported major.

**Very old GCC lacks `-fcallgraph-info` (needs GCC ≥ 10).**
*Mitigation:* preflight fails loudly against older GCC; document minimum version as GCC 10 in the Prerequisites section.

**User forgets one of the required flags.** The builddir is silently missing dumps for some or all TUs, and the index quietly degrades.
*Mitigation:* preflight scans a representative sample of `.o` files for expected sibling artifacts and refuses to index if any are missing. The error names the specific flag and reprints the recommended `meson setup` command. No indexing proceeds against an under-flagged builddir.

**Live checkout drift from the indexed builddir.** The user modifies source after the build finished but before running herbarium, so blobs no longer match `.o` DWARF line numbers.
*Mitigation:* Phase 5's source packing hashes each file at read time and stores the hash. At serve time `verify_source` and `list_source_drift` let the agent detect and surface this. Optionally, `herbarium collect --strict` refuses to package sources whose mtime is newer than the corresponding `.o`.

**Dump volume on large builds.** IPA dumps and `.ci` files add disk usage in the builddir.
*Mitigation:* files are small per TU (typically a few KB to tens of KB); a mid-size project adds well under 100 MB total. Herbarium ingests once and leaves the files in place for incremental re-runs. If disk pressure matters, they can be deleted after `herbarium collect` completes with no loss of index correctness — the next full ingest simply requires rebuilding.

**DWARF stripped from release builds.** A release build without `-g` loses most of the identity layer.
*Mitigation:* preflight verifies `.debug_info` is present in a sample `.o`. Documentation is explicit: herbarium collects against a build with debug info; the shipped artifact can be stripped independently.

**Weak-symbol resolution without a linker map.** Not all users will add `-Wl,-Map`.
*Mitigation:* herbarium degrades gracefully — `link_resolutions` falls back to a per-.o `nm` scan across the builddir for both `winning_object` (strong > weak > local heuristic) and `losing_objects` (candidates minus winner). Weak-vs-strong resolution is correctly identified. The single remaining ambiguous case is two same-named statics in two TUs of the same target with no map file — those fall back to name lookup and may misattribute.

**IPA dumps require an optimization level.** `-fdump-ipa-cgraph` runs at any `-O`, but devirt and ICF fire meaningfully only at `-O1+`.
*Mitigation:* Prerequisites recommend `--buildtype=debugoptimized` (`-O2 -g`). At `-O0` the index is still valid but `devirt_hints` and `icf` groups will be empty; the affected MCP tools return empty results and their descriptions note this.

**Builddir and source root out of sync.** User points herbarium at a builddir whose sources have been checked out to a different commit than the source root passed in.
*Mitigation:* every source file listed by Meson introspection must exist under `--project-root` at ingest time; herbarium refuses if any are missing. This bounds the failure mode to files the user has moved outright, not merely modified.

## Build and distribution

- Single Go binary. `herbarium collect` shells out to `nm`, `objdump`, and `readelf` from standard binutils on the indexing host — these must be on `PATH`. GCC and Meson are prerequisites for producing the builddir but are never invoked by herbarium itself. `herbarium serve` has no external subprocess dependencies whatsoever.
- Pinned GCC version range documented per herbarium release for dump format compatibility.
- Serve mode has zero external dependencies and can run anywhere Go runs; the DB is fully self-contained.
- Docker image published for reproducible indexing environments, with a pinned GCC and standard binutils.

## Handoff notes (project-specific inputs)

Fill in before Phase 0 begins. None of these gate implementation — Phase 0 will surface most of them anyway — but stating them up front saves the fresh agent a round-trip.

- **GCC version:** (from `gcc --version`) — pins the sample fixture directory name and the parser's version gate.
- **Meson version:** (from `meson --version`) — affects `meson-info/` JSON schema on very old Meson.
- **Rough project size:** N translation units, M linked executables, K shared libraries. Determines whether the naive "walk every `.o` sequentially" approach is fine or needs to become parallel from day one.
- **Build layout:** single executable / multiple executables sharing a lib / library-only. Multi-executable projects exercise `link_resolutions` and per-target objdump most.
- **GCC extensions in use:** nested functions? VLAs inside structs? Computed goto? Non-standard inline asm? Only relevant if a source walker is added later; not needed for Phases 1–8.
- **Warning discipline:** does the project build with `-Werror`? Any warnings expected to fire under `-fdump-ipa-*` that need to be suppressed for the indexing build variant.
- **Reproducibility expectations:** does the project pin a compiler? Reproducible builds enabled (`SOURCE_DATE_EPOCH`)? Herbarium's determinism inherits the build's determinism.
- **Preferred artifact filename convention:** the plan defaults to `herbarium.hbr`; if the project has strong opinions (e.g., must sit under `dist/` or be named after the target), pick now.

## Appendix: USR scheme

Herbarium synthesizes stable, deterministic identity strings ("USRs") for every C entity, so cross-plane joins (source ↔ link ↔ runtime) hold across builds and across runs. The scheme below is the contract. Any change is schema-breaking and requires a `meta.schema_version` bump.

### Conventions

- **Path** components are always source paths **relative to `--project-root`**, with forward slashes, no leading `./`, UTF-8 as-is, no case folding.
- **Name** components are the unqualified C identifier as written.
- Sources outside `--project-root` are refused at collect time (same rule Phase 5 applies for the blob store, keeping identity and blob coverage in lockstep).

### Functions

| Case | USR form | Example |
|---|---|---|
| External linkage | `c:@F@<name>` | `c:@F@main` |
| Static in a `.c` file | `c:<path>@F@<name>` | `c:src/net/conn.c@F@helper` |
| Static inline in a header | `c:<header-path>@F@<name>` | `c:include/utils.h@F@inline_swap` |

Static-inline in a header instantiated across multiple TUs shares one USR, keyed on the header, not the including TU.

### Variables

| Case | USR form |
|---|---|
| External | `c:@V@<name>` |
| Static file-scope | `c:<path>@V@<name>` |
| `extern` declaration | resolves to the defining symbol's USR |

### Types

| Case | USR form |
|---|---|
| Typedef | `c:<path>@T@<name>` |
| Struct tag | `c:<path>@S@<name>` |
| Union tag | `c:<path>@U@<name>` |
| Enum tag | `c:<path>@E@<name>` |
| Anonymous struct/union/enum | `c:<path>@S@__anon_<line>_<column>` (respectively `@U@`, `@E@`) |
| Enum member | `c:<path>@E@<enum-name>@<member>` (or anonymous form) |

Typedefs and record types are file-scoped even when declared in headers, because they have no linkage.

### Fields

Struct or union field: `c:<path>@S@<struct>@F@<field>` (using the containing type's USR path, including for anonymous containers).

No table stores a field USR yet — `usr.Field` defines the form for a future walker's `stored_in:T.f`, but has no caller. `indirect_call_sites.field_hint` is deliberately *not* one: it is a human-readable `struct_t.field` (or a bare global / parameter name), because it is a hint for the reader rather than a join key — the join that matters there is `callee_type` against `symbols.signature`. Elsewhere a field name is carried as a plain string alongside the containing type's USR.

### Generated sources

Files under `<builddir>/…` are assigned paths relative to `<project-root>/<builddir-name>/`. Their USRs are stable only if the generator is deterministic; nondeterministic code generators produce USR churn across builds. Herbarium does not attempt to canonicalize past this.

### Symbols and definitions

A single external symbol may have **multiple definitions** in an indexed project:

- A multi-executable project defines `main` once per binary. Both `app1/main.c:main` and `app2/main.c:main` share USR `c:@F@main` — they are the same source-level entity as far as identity is concerned; they differ per-target at link time.
- Weak/strong override pairs give the same USR two definitions: the weak fallback in a library and a strong override in an executable.
- Static-inline functions declared in a header are instantiated in every including TU. All instantiations share the header-keyed USR.

**Rule:** `symbols` holds identity (one row per USR), and `symbol_definitions` holds one row per observed def with its own file/line/decl_file/decl_line/is_weak/linkage_name. Declarations without a def in any indexed TU (external refs to libc, etc.) contribute zero rows to `symbol_definitions`.

**Consequences:**
- `describe_symbol(usr)` returns `definitions: [{file, line, is_weak, linkage_name}, …]`. UI expected to show all defs when there is more than one.
- `find_symbol` responses are unchanged in shape — still one entry per USR — but the entry now carries a defs array.
- `list_callers`/`list_callees`/`list_call_paths` key on `symbol_id` (identity), not on any specific def. Edges are between identities; the plan's principle that source-view queries answer at the identity level is preserved.
- `link_resolutions` in Phase 4 joins on `(target_id, symbol_id)` and identifies which def won via `winning_object`, which maps to a specific `symbol_definitions.file` when the linker's chosen object corresponds to one indexed TU.
- The "USRs are the only join key between planes" invariant (below) is preserved: nothing depends on splitting a USR just because a symbol has multiple defs.

### GCC-generated clones

GCC's IPA passes may emit specialized variants of a source function: `<name>.constprop.<N>` (constant-propagated), `<name>.isra.<N>` (interprocedural scalar-replacement of aggregates), `<name>.part.<N>` (partial inlining split), and similar. These variants appear as distinct entries in `.cgraph`, `.inline`, the optimization record, and in the linked binary's symbol table. They share the source identity of their parent — same file, same source line, same signature — but have distinct linkage names and distinct runtime addresses.

**Rule:** clones alias to the parent's USR. There is exactly one row in `symbols` per source function regardless of how many clones the optimizer produces. The `symbols.linkage_names` column carries the JSON array of every linkage name that resolves to this USR at link time (`["use_dispatch", "use_dispatch.constprop.0"]`), so objdump/nm-based symbolication remains lossless.

**Consequences:**
- `list_callers` and `list_linked_callers` for a source function return the union across all clones; the response tags each edge with the linkage name of the clone that actually made the call so an agent can distinguish specialized from unspecialized dispatch when it matters.
- `describe_inlining` reports per-clone decisions under the parent USR.
- `link_resolutions` rows key on linkage name (not USR), because two clones of the same source function can resolve to different objects.

### Explicitly out of scope for USRs

- Labels and `goto` targets.
- Macro names.
- Local variables and parameters.
- Function-scoped statics (deferred; if added later: `c:<path>@F@<enclosing>@V@<name>`).

### Invariant

USRs are the **only** join key between planes. Any future contributor extending the schema either (a) joins through an existing USR or (b) proposes an amendment to this appendix. Renaming or reshaping any USR form is a schema-breaking change requiring a bump of `meta.schema_version` and a documented migration for existing `.hbr` files.
