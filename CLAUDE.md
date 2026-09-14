# CLAUDE.md

Working notes for Claude when editing this repo. Read `herbarium-plan.md` first — it is the design contract; this file is orientation on top of it. `SCHEMA.md` is the third piece: a table-by-table reference for what a row *means*, which pass writes it, and which compiler artifact the fact came from.

**Check `SCHEMA.md` before proposing a schema change.** It exists because "herbarium doesn't know X" is usually answerable from data already in the index, and the wrong reflex is to add a column each time an agent reports a miss. It also records which corners are inert and — in § 7 — the one question the schema genuinely cannot answer, which is whether an internal-linkage symbol's code is in a given target. Regenerate it from the code when the schema does move; do not hand-patch it.

## What this is

`herbarium` ingests an already-built Meson C project into a single SQLite artifact (`.hbr`) and serves it over MCP for AI agents. Every fact in the index traces back to a compiler dump (GCC's `-fcallgraph-info`, `-fdump-ipa-*`), DWARF, or a binutils inspector (`nm`, `objdump`) — never to a re-parser. Two subcommands:

- `herbarium collect` — reads a builddir + project-root, writes an `.hbr`.
- `herbarium serve` — opens an `.hbr` read-only, exposes 29 MCP tools over stdio or streamable HTTP.

## Non-negotiables (from `herbarium-plan.md § Design principles`)

1. **Compiler truth over source guesses.** No AST re-parse. No CC wrapper. No plugin. Herbarium never invokes GCC, `ld`, `meson`, or `ninja`.
2. **One artifact.** Everything (schema, facts, source blobs) lives inside `<name>.hbr`.
3. **USR is the only join key.** Any new schema table joins through an existing USR or amends the appendix (schema-breaking → bump `store.SchemaVersion`).
4. **Serve mode has zero external subprocess deps.** All `nm`/`objdump` runs happen at collect time only.
5. **Read-only at query time.** `store.OpenReadOnly` uses `mode=ro + query_only(1)`; the driver rejects writes. `sql_query` relies on this — do not add application-side SQL parsing.

## Repo layout

```
SCHEMA.md               table-by-table reference: meaning, writing pass, provenance
cmd/herbarium/          collect + serve subcommands, tiny glue
internal/
  mesonintrospect/      reads builddir/meson-info/*.json (no meson invocation)
  builddir/             enumerates .o files + sidecar dumps
  preflight/            refuses to index under-flagged builddirs
  store/                schema.sql + open/init/ro helpers
  blobstore/            zstd + SHA-256 content-addressed blob writer
  ninjadeps/            hand-rolled parser for ninja's binary .ninja_deps log
  gccdump/              per-dump-kind parsers (ci, cgraph, inline, icf, optrecord)
  dwarfingest/          DWARF reader (subprograms, signatures, call sites, inlined bodies)
  linkplane/            nm + objdump + map file parsers; runTool wraps exec
  usr/                  USR synthesis per herbarium-plan.md appendix
  ingest/               pipeline orchestrator: Compiler, DWARF, Targets, Link, Sources
  mcp/                  MCP server + 29 tools; tests build fixture .hbr in-process
testdata/
  fixture/              minimal Meson project the tests build against
  samples/gcc-16/       pinned parser fixtures (dump files, map files, .ninja_deps)
```

## Pipeline order (`cmd/herbarium/collect.go`)

Passes run sequentially, each in its own SQL transaction. The index is built into a scratch file beside `--out`, sealed out of WAL (`store.SealForReading`), and `os.Rename`d into place only after the last pass, so a mid-pipeline failure leaves **no** `.hbr` rather than a half-populated one — and `--replace` never exposes a partial file to a `serve` process holding the same path (see Reloading a live session).

The seal is not housekeeping. Collect writes under WAL for throughput, but `journal_mode` lives in the file header and outlives the writer, and SQLite cannot open a WAL database *even read-only* without a `-shm` beside it — so an unsealed `.hbr` demands write access to its own directory from every reader and fails with `SQLITE_READONLY_DIRECTORY` where it does not have it. Sealing also unlinks the sidecars, which is what stops a `--replace` from leaving one inode's `-wal`/`-shm` sitting at the next inode's path.

1. `mesonintrospect.Load` → parse `meson-info/*.json`.
2. `builddir.Crawl` → find `.o` files + their sidecar dumps.
3. `preflight.Check` → refuse if required dumps missing; report the exact `meson setup` fix.
4. `store.Open` + `store.Init` → apply schema, stamp meta.
5. `ingest.Compiler` → parse dumps, build `symbols` + `symbol_definitions` + `call_edges(compiler_cgraph)` + `inline_decisions` + `inline_records`. Returns `IDByUSR` for later passes.
   Before inserting, `pruneUnreferenced` drops symbol records nothing else in the index points at — internal linkage, no def row, not address-taken, and absent from the edge / inline-record / ICF planes. That shape is the unused `static inline` from a widely-included header: GCC parses it, removes the body, and leaves a bare cgraph node, which the per-TU USR scheme then replicates once per including TU. On a 165-TU project this was 75,660 of 86,912 `symbols` rows across just 1,015 distinct names, and with their index and FTS entries **82% of the whole `.hbr`**. Dropping them also de-noises `list_unreachable_symbols`, which is literally `symbols` minus `link_resolutions`. A header inline that *was* used keeps its row in the TU that used it, via its cgraph edge.
6. `ingest.DWARF` → UPSERT signatures + `decl_file`/`decl_line`; repairs `symbol_definitions.file`/`line` for functions with no `.ci` node (see Known gaps); owns `indirect_call_sites` and `inline_instances`; rebuilds `symbols_fts`.
7. `ingest.Targets` → populate `targets` + `target_sources`, return name→id map.
8. `ingest.Link` → `nm` + `objdump` + `.map` files → `link_resolutions` + `call_edges(objdump)` + `symbol_reachability`.
9. `ingest.Sources` (Phase 5) → pack target sources + `.ninja_deps` headers via `blobstore`.

Phase 7 (incremental re-ingest) is **deferred by user decision** — every collect rebuilds from scratch. `collect` refuses to overwrite an existing `.hbr` unless `--replace` is passed.

**Scope-limiting flag:** `collect --target NAME[,NAME...]` filters `intro.Targets` to the requested set before any ingest pass runs, so `ingest.Targets` and `ingest.Link` skip work for other targets. This is the main lever on collect time, and the reason is in `ingest.Link`'s shape: it loops over linked binaries (static libraries are skipped — no binary to inspect) and runs `linkplane.RunObjdump` over each one *in full*. Cost therefore scales with binaries × their whole size, not with how much distinct code exists, so N executables statically linking one library disassemble that library's code N times. `RunObjdump` streams rather than buffers precisely because a single binary's disassembly can exceed 100 MB.

This is a *fast slice*, not a partial index: compiler-plane ingest processes every `.o` under the builddir and `ingest.Sources` packs from `.ninja_deps`, so `symbols`, `symbol_definitions`, `call_edges(compiler_cgraph)`, the DWARF planes, the inlining planes and **every packed source file** stay complete — a `--target app1` slice of the fixture still packs `app2/main.c`. What narrows is the post-link view only: `targets` rows, `link_resolutions`, `symbol_reachability`, and `call_edges(objdump)`. Unknown target names are a hard error that lists what is available. Every `nm`/`objdump` invocation prints `$ <cmd> <args>` + elapsed time + payload size to stderr via `linkplane.runTool`; tests silence it in a `TestMain` via `linkplane.SetLogWriter(nil)`.

## MCP tools (Phase 6, landed)

29 tools grouped by file under `internal/mcp/`. Every location-returning tool wraps its position in a uniform `Location{path, line?, column?, blob_hash, snippet?, absolute_path?}` shape (see `location.go`). Response payloads land as both `text` (JSON pretty-printed) and `StructuredContent` on the `CallToolResult` — an agent can consume either. Tool descriptions are the user-facing contract; edit them if behavior changes.

Groups:

- **Escape hatches:** `describe_schema`, `sql_query`.
- **Source:** `read_source`, `list_source_files`, `verify_source`, `list_source_drift`, `search_source` (literal + RE2 grep across every indexed blob). The live-hash mode of `verify_source` and `list_source_drift` require `serve --project-root`.

  `search_source` answers summary-first like the inlining tools, but the split it needs is different and worth understanding before editing it. There is no SQL to aggregate over: matches come from decompressing and scanning blobs, so a true total can only come from finishing the scan. `limit` therefore caps *what is returned*, never *what is counted* — `searchInBlob` counts every hit and appends only while under the cap. `searchSourceScanLimit` bounds the scan itself for the pathological case and sets `scan_truncated` when it bites, the same shape as `explain_call`'s `verdictScanLimit`. Per-file constants (`blob_hash`, `absolute_path`) live once each in `files` rather than on every match row, and `match_text` is emitted only for a regex — on a literal search it is the pattern, echoed once per row. Together those cut a fixture 73-match payload from 27.1 KB to 20.7 KB; the win is larger in production, where `serve --project-root` would otherwise repeat a full absolute path on every row.
- **Targets:** `list_targets`, `describe_target`.
- **Symbols:** `find_symbol` (FTS5 with prefix tokens by default; `exact=true` switches to a literal lookup over `symbols.name` **and** `linkage_names`, which is the only way to resolve a clone name like `use_dispatch.constprop.0` — FTS indexes source names only), `describe_symbol` (multi-def + linkage_names + reachability + link_resolutions).

  A zero-hit `find_symbol` carries a `note` saying *which kind of nothing* it is. This is not a courtesy string: an empty `hits` array is otherwise identical whether the name is absent from the code, filtered out by `target`, or of a kind this index has no row shape for — and agents have read all three as "absent from the codebase". `emptyFindNote` is the one place that wording lives.
- **Call graph, source view:** `list_callers`, `list_callees`, `list_call_paths` (in-memory DFS, cycle-in-path guard, max_depth cap).
- **Call graph, runtime view:** `list_linked_callers`, `list_linked_callees`, `describe_inlining` (three planes: `records`, `instances`, `cgraph_edges`), `list_inline_instances`, `explain_call` (one verdict for one call, with its evidence).

**Response-size contract.** Every tool whose result set scales with the project — `describe_inlining`, `list_inline_instances`, `explain_call`, `list_indirect_call_sites`, `list_unreachable_symbols`, `list_entry_points`, `search_source` — takes `limit` (`rowLimit`, max 2000) and reports `truncated`, and every location-returning one takes `include_snippets` (`wantSnippets`, default **off**). Declare both with `limitArg(default)` / `snippetArg()` from `location.go` so the wording stays identical. The inlining tools additionally answer summary-first: `summary` (exact totals, by pass, by inline depth) is computed over every matching row, the row arrays are capped at 50 (`limit`, max 1000) with a `truncated` flag, and snippets are off unless `include_snippets=true`. This is not tidiness — a row costs ~500 bytes without a snippet and ~700 with one, three arrays ship in one response, and an aggressively inlined caller produced enough rows to exceed an MCP client's output limit and have the *whole* payload truncated by the harness, which is worse than any cap. `explain_call` is the exception that proves the rule: its verdict is always decided from the full row set (`verdictScanLimit`) and only the echoed evidence is capped, because a verdict computed from truncated rows could be flatly wrong.
- **Indirect:** `list_indirect_call_sites`, `list_address_taken_functions`, `resolve_indirect_call`. There is no devirtualization plane: GCC's ipa-devirt pass acts on polymorphic calls, which only C++ produces, so `-fdump-ipa-devirt` reported `0 polymorphic calls, 0 devirtualized` on every TU herbarium has ever seen. The table, the tool and the parser were removed in schema v9 — see `store.SchemaVersion`'s v8 → v9 note. `resolve_indirect_call` returns *candidates*, never a resolution.
- **Linkage + reachability:** `describe_link_resolution`, `list_weak_symbols`, `list_undefined_symbols`, `list_icf_groups`, `list_unreachable_symbols`, `list_entry_points`.
- **Session:** `reload_index` (see below).

## Reloading a live session

An agent that edits C code mid-session needs the index rebuilt, and on the stdio transport it cannot restart its own MCP server — the client owns the process lifetime. So the loop is: the agent runs `ninja`, then `herbarium collect … --out <the served .hbr> --replace`, then calls `reload_index`.

The split is forced by the design principles, not by taste. Serve cannot re-ingest (`ingest.Link` shells out to `nm`/`objdump`, and serve has zero subprocess deps) and herbarium never runs the build itself, so re-ingest stays a separate process and the only serve-side capability is swapping the handle.

Two pieces make that swap safe, and both are load-bearing:

- **`collect --replace` builds elsewhere, seals, and renames.** `os.Rename` is atomic within a filesystem (hence the scratch file in `--out`'s own directory), and a reader holding the old path keeps its inode until it reopens — so the live server answers from the old index right up to the reload, and never sees a half-written file. The `db.Close()` before the rename is required, not tidiness: the `-wal`/`-shm` sidecars are only folded back in when the last connection drops.
- **`Server.mu` guards `s.db`, and `pinIndex` holds it read-locked for the whole of every tool call.** That is why the 61 direct `s.db` reads across the tool files need no locking of their own — and why any future code path touching `s.db` from outside a tool handler must take `RLock` itself. `reload_index` is exempt from the middleware by name: it takes the write lock, `sync.RWMutex` is not reentrant, and routing it through `pinIndex` would deadlock the session it exists to keep alive.

Every failure path in `Reload` (missing file, not an index, `schema_version` mismatch after a herbarium upgrade) closes the new handle and leaves the old one serving. A reload that cannot produce a usable index is a no-op, never an outage. `changed=false` in the response means the file on disk still carries the `indexed_at` the server already had — almost always a re-collect that didn't happen, or wrote somewhere else.

`store.DiagnosePath` runs before every read-only open — `Reload`'s and `serve`'s at startup — because SQLite collapses distinct causes into one errno. **`unable to open database file (14)` means either "no such file" or "you may not read this file"**, and both have bitten real sessions: a re-collect that wrote elsewhere, and a `collect` run under `sudo` leaving a root-owned artifact the serve process cannot open. (That second one used to be worse than it needed to be: `reserveTempIndex` built the scratch file with `os.CreateTemp`, which hardcodes 0600 and ignores umask, and `os.Rename` carried that mode onto every finished `.hbr`. It now passes 0666 to `open(2)` so the kernel applies the caller's umask — a root-owned 0644 index is at least still readable. The diagnosis stays necessary regardless, since ownership alone can still deny the read.) Stat alone cannot tell them apart (stat succeeds on an unreadable file — it needs only directory execute permission), so the diagnosis opens the file too. On the serve path this matters more than it looks: a failed startup takes the MCP transport with it and clients report only `-32000: Connection closed`, so that one stderr line is all the evidence anyone gets.

The reload flavour appends the `collect … --replace` line for the exact served path, which is also why the tool description tells agents to read the path out of `reload_index`'s own `path` field rather than guess at `--out`.

## Known gaps

Documented in tool descriptions and in `herbarium-plan.md § Phase 6`:

- `indirect_call_sites.callee_type` / `.field_hint` resolve for the common shapes but not all of them: a computed pointer, a non-x86-64 object, or a parameter list whose SysV register assignment can't be replayed all leave both columns empty. Both routes are gated on x86-64, not just the relocation one — DWARF register numbers are per-architecture, so `argRegOrder` applied to AArch64 wouldn't fail, it would silently name the wrong parameter, and `resolve_indirect_call` falls back to the full address-taken pool there. `internal/dwarfingest/calltarget.go` has two routes — `DW_AT_call_target`'s register forms (calls through a fn-pointer parameter) and, where GCC emits no `call_target` at all (the `g_ops.add` dispatch-table shape), the `R_X86_64_PC32` relocation at `return_pc-4`. Both decline rather than guess when the chain doesn't bottom out at a pointer-to-subroutine; a wrong `callee_type` is worse than an empty one, since it narrows `resolve_indirect_call` to confidently wrong candidates.
- **Three inlining planes, and they disagree on purpose.** `inline_decisions` (from `.cgraph`'s `(inlined)` tag) is IPA-stage only: GCC's early inliner folds `always_inline` and trivial callees in `pass_early_inline`, before any IPA pass runs, so those edges are already gone by the time the dump is written. `inline_records` (from `-fsave-optimization-record`) is the decision plane and does see the early pass — but GCC omits the source location on some records (`file=''`, `line=0`), and a decision there is not proof the code survived. `inline_instances` (from DWARF `DW_TAG_inlined_subroutine`) is the outcome plane — what is really in the object — but a callee whose copy folds to a constant afterwards leaves no DIE, so it under-reports (the fixture's `icf_add_one` → `icf_bump_by_one` is exactly this: a record with no instance). Never answer "was X inlined" from one plane alone; for the post-link answer on a specific target, `list_callees` − `list_linked_callees` is still the definitive diff.
- The opt-record parser pins GCC's record format (`gccdump.SupportedOptRecordFormat`, element `[0]`'s `"format"`, currently `"1"`) and refuses anything else — a lenient parse of a changed layout would return zero inline records with nothing saying the plane went empty. If a GCC major bumps it, the parser needs updating, not the guard relaxing. Note also that `-fsave-optimization-record` arrived in GCC 9, below the GCC 10 floor herbarium already requires for `-fcallgraph-info`, so it adds no new version constraint.
- A function GCC inlines at **every** call site gets no `.ci` node — callgraph-info describes only what reached the assembler — so the compiler plane records no location for it. `ingest.DWARF` recovers one from the abstract instance root, which is why `symbol_definitions.file` can name a `.h`. Two consequences remain: the repair is per-object, so a callee with no DWARF (a TU built without `-g`) keeps the Phase 2 fallback of the including TU at line 0; and because the USR scheme anchors a static at its TU, one header inline pulled into N TUs is up to N distinct symbols, only those whose TU actually called it carrying a def row — and since `pruneUnreferenced`, a TU that includes the header without calling it contributes no row at all.
- **`symbols.kind` is a closed two-value enum: `function` | `variable`.** It is the first token of GCC cgraph's `Type:` line (`gccdump/cgraph.go`), and the cgraph describes only what reached the assembler — so types, macros and enum constants have no `symbols` row at any kind, and `find_symbol` cannot find one however it is queried. They are reachable only through the source plane (`search_source` / `read_source`). `herbarium-plan.md`'s appendix reserves `typedef`/struct kinds for a future walker phase; nothing populates them, and `internal/usr/usr.go`'s `Typedef` helper has no caller. The tool docs, `schema.sql`, and `describe_schema`'s glossary all advertised `typedef` as a live value until an agent filtered on it, got an empty result, and read it as "no such identifier in the codebase". `TestSymbolKindEnumIsExhaustive` now pins the enum against the fixture in both directions, and `TestSchemaEnumsMatchIndexedValues` does the one-way check for every other documented enum.
- **`target` means two different planes, and an empty result reads as absence on both.** On `find_symbol` / the linkage tools it joins `link_resolutions` — so an internal-linkage symbol (a `static`, a `static inline` in a header) is filtered out even when its code is in the binary, and "0 hits" means *nothing resolved under that name*, not *no such code*. On the source tools (`list_source_files`, `search_source`, `list_source_drift`) it joins `target_sources` — the TUs meson compiles into that target — so an executable that links a `lib/` archive lists only its `app/` files, and the archive's sources answer under the *library* target's name. Both caveats now live on the `target` argument descriptions (`targetSourcesArg` in `source_tools.go` centralises the source-plane wording); a report of "I read the empty result as 'not linked'" means that wording failed, not the filter.
- `list_icf_groups` covers IPA-ICF only (from GCC's `.icf` dumps). Linker-level ICF (gold/lld `--icf=all`) is a separate pass and not tracked — if the linker folds further, this tool underreports.
- `list_entry_points` covers `main` + externally-visible symbols. Constructor-attributed (`__attribute__((constructor))`) and `.init_array` entries are not classified — would need an additional DWARF pass.
- `link_resolutions.losing_objects` is derived from an nm scan across every .o in the builddir, so it lists "other .o's that also define this symbol" — broader than a strict map-file impl, which would list only candidates ld actually weighed. Archive members ld never pulled in still show up here.
- `link_resolutions.winning_object` reflects the linker's actual choice when a `.map` file is present; without one, it is picked heuristically from the same nm scan (strong > weak > local, deterministic tie-break). Same-named statics in two TUs of the same target with no map file cannot be disambiguated by address alone and fall back to name lookup.
- `symbol_reachability.section_kept` is always 1 — parsing "Discarded input sections" from the map file is future work.

## Fixture (Phase 8)

`testdata/fixture/` is a 3-target Meson project intentionally constructed to exercise every schema-level behavior:

| Feature | Where |
|---|---|
| Multi-executable sharing a lib | `app1/`, `app2/`, `lib/` |
| Weak override | `lib/weak_impl.c` (weak) + `app1/strong_override.c` (strong) |
| Const dispatch table | `include/dispatch.h` + `lib/dispatch_impls.c` (`g_ops`) |
| Indirect call | `app1/main.c` `use_dispatch` calls `g_ops.add/.mul` |
| ICF fold | `lib/icf_pair.c` (`icf_add_one` + `icf_bump_by_one`) |
| Dead-strip | `never_called` in `lib/shared_utils.c` under `-Wl,--gc-sections` |
| GCC clone | `use_dispatch.constprop.0` (single-caller specialization) |
| Early (pre-IPA) inline | `scale_by_two` (`always_inline`) folded into `scaled_compute` in `lib/shared_utils.c` |
| Header static inline, no `.ci` node | `hdr_clamp` in `lib/hdr_inline.h`, called twice from `scaled_compute` |

Build the fixture via `bash testdata/fixture/scripts/build.sh` — this pins the dump flags. If dumps change shape across GCC majors, refresh `testdata/samples/gcc-<version>/` from the fresh builddir.

## Testing

- `go test ./...` covers everything. The MCP tests build a fresh `.hbr` in-process via `collectForTest` in `internal/mcp/collect_helper_test.go` — no test needs `go run ./cmd/herbarium`.
- `TestE2EFixtureContract` in `internal/mcp/e2e_test.go` walks 16 tools and asserts the fixture's full contract; a regression in either ingest or MCP surface trips it.
- `TestSchemaJoinRecipesExecute` runs every `SchemaJoinRecipes` entry through `sql_query` (named params rewritten to `?`). Agents copy those recipes verbatim, so one that does not parse reads as a herbarium bug from the other side of the transport — and one shipped that way, referencing `json_each.value` with `json_each` only in a subquery's `FROM`. Adding a recipe means the test runs it.
- Parser tests use pinned samples under `testdata/samples/gcc-16/`. The opt-record samples there are gzipped JSON copied straight out of a fresh fixture builddir (`lib/shared_utils.c.c.opt-record.json.gz`, `app1/main.c.c.opt-record.json.gz`); refresh them the same way if GCC changes the record shape. If the pinned GCC version changes, update `store.SchemaVersion` only if the on-disk schema also changes — sample-format drift alone doesn't warrant a bump.
- `linkplane/exec_test.go` covers the subprocess-error wrapping contract (nm/objdump errors must surface stderr, not just an exit code).

## Style conventions the reviewer will call out

- **Comments explain WHY, not WHAT.** If a `//` comment restates what the next line does, delete it. Comments earn their keep by naming a hidden constraint, an invariant, a workaround for a known bug, or a decision that would surprise the reader.
- **Errors from external tools must include stderr.** Use `linkplane.runTool` for `exec.Cmd` invocations — do not roll `cmd.Output()` directly.
- **New MCP tools must register from `New()` in `internal/mcp/mcp.go`.** Add a `registerXxxTools` method and one line at the bottom of `New()`.
- **Always use `newTool(name, opts...)` — never `mcp.NewTool(name, ...)` directly.** The helper sets `readOnly=true`, `destructive=false`, `openWorld=false` up front. `mcp.NewTool` defaults `destructiveHint=true`, which contradicts `readOnly=true` and makes some MCP clients (opencode) drop the transport with `MCP error -32000: Connection closed`. `TestToolAnnotationsAreConsistent` guards this.
- **No backwards-compat shims.** If you rename a schema column or tool argument, bump `store.SchemaVersion` and change every call site in the same PR.
- **Feature flags are not a design pattern here.** The plan is the contract; either the plan changes or the code matches it.

## When something breaks after a fixture change

`testdata/fixture/` changes ripple through several tests. In order of blast radius:

1. `internal/builddir/builddir_test.go` — object count.
2. `internal/mesonintrospect/mesonintrospect_test.go` — target source counts.
3. `internal/ingest/{link_test,sources_test}.go` — expected symbols, files, reachability.
4. `internal/linkplane/{nm_test,mapfile_test}.go` — per-symbol expectations against pinned map/nm outputs.
5. `internal/mcp/source_tools_test.go` — packed file count.
6. `internal/mcp/e2e_test.go` — the end-to-end assertion set.

Also refresh `testdata/samples/gcc-16/{app1.map,app2.map,lib/icf_pair.c.c.089i.icf}` if the map file structure changed.
