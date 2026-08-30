# CLAUDE.md

Working notes for Claude when editing this repo. Read `herbarium-plan.md` first — it is the design contract; this file is orientation on top of it.

## What this is

`herbarium` ingests an already-built Meson C project into a single SQLite artifact (`.hbr`) and serves it over MCP for AI agents. Every fact in the index traces back to a compiler dump (GCC's `-fcallgraph-info`, `-fdump-ipa-*`), DWARF, or a binutils inspector (`nm`, `objdump`) — never to a re-parser. Two subcommands:

- `herbarium collect` — reads a builddir + project-root, writes an `.hbr`.
- `herbarium serve` — opens an `.hbr` read-only, exposes 23 MCP tools over stdio or streamable HTTP.

## Non-negotiables (from `herbarium-plan.md § Design principles`)

1. **Compiler truth over source guesses.** No AST re-parse. No CC wrapper. No plugin. Herbarium never invokes GCC, `ld`, `meson`, or `ninja`.
2. **One artifact.** Everything (schema, facts, source blobs) lives inside `<name>.hbr`.
3. **USR is the only join key.** Any new schema table joins through an existing USR or amends the appendix (schema-breaking → bump `store.SchemaVersion`).
4. **Serve mode has zero external subprocess deps.** All `nm`/`objdump` runs happen at collect time only.
5. **Read-only at query time.** `store.OpenReadOnly` uses `mode=ro + query_only(1)`; the driver rejects writes. `sql_query` relies on this — do not add application-side SQL parsing.

## Repo layout

```
cmd/herbarium/          collect + serve subcommands, tiny glue
internal/
  mesonintrospect/      reads builddir/meson-info/*.json (no meson invocation)
  builddir/             enumerates .o files + sidecar dumps
  preflight/            refuses to index under-flagged builddirs
  store/                schema.sql + open/init/ro helpers
  blobstore/            zstd + SHA-256 content-addressed blob writer
  ninjadeps/            hand-rolled parser for ninja's binary .ninja_deps log
  gccdump/              per-dump-kind parsers (ci, cgraph, inline, icf, devirt)
  dwarfingest/          DWARF reader (subprograms, signatures, call sites)
  linkplane/            nm + objdump + map file parsers; runTool wraps exec
  usr/                  USR synthesis per herbarium-plan.md appendix
  ingest/               pipeline orchestrator: Compiler, DWARF, Targets, Link, Sources
  mcp/                  MCP server + 23 tools; tests build fixture .hbr in-process
testdata/
  fixture/              minimal Meson project the tests build against
  samples/gcc-16/       pinned parser fixtures (dump files, map files, .ninja_deps)
```

## Pipeline order (`cmd/herbarium/collect.go`)

Passes run sequentially, each in its own SQL transaction, so a mid-pipeline failure leaves an empty `.hbr` rather than a half-populated one:

1. `mesonintrospect.Load` → parse `meson-info/*.json`.
2. `builddir.Crawl` → find `.o` files + their sidecar dumps.
3. `preflight.Check` → refuse if required dumps missing; report the exact `meson setup` fix.
4. `store.Open` + `store.Init` → apply schema, stamp meta.
5. `ingest.Compiler` → parse dumps, build `symbols` + `symbol_definitions` + `call_edges(compiler_cgraph)` + `inline_decisions`. Returns `IDByUSR` for later passes.
6. `ingest.DWARF` → UPSERT signatures + `decl_file`/`decl_line`; owns `indirect_call_sites`; rebuilds `symbols_fts`.
7. `ingest.Targets` → populate `targets` + `target_sources`, return name→id map.
8. `ingest.Link` → `nm` + `objdump` + `.map` files → `link_resolutions` + `call_edges(objdump)` + `symbol_reachability`.
9. `ingest.Sources` (Phase 5) → pack target sources + `.ninja_deps` headers via `blobstore`.

Phase 7 (incremental re-ingest) is **deferred by user decision** — every collect rebuilds from scratch. `collect` refuses to overwrite an existing `.hbr`.

**Scope-limiting flag:** `collect --target NAME[,NAME...]` filters `intro.Targets` to the requested set before any ingest pass runs, so `ingest.Targets` and `ingest.Link` skip work for other targets. This is a *fast slice*: compiler-plane ingest (symbols, cgraph edges, DWARF) still processes every `.o` under the builddir, so the `symbols`/`call_edges` view stays complete. Unknown target names are a hard error that lists what is available. Every `nm`/`objdump` invocation prints `$ <cmd> <args>` + elapsed time + payload size to stderr via `linkplane.runTool`; tests silence it in a `TestMain` via `linkplane.SetLogWriter(nil)`.

## MCP tools (Phase 6, landed)

27 tools grouped by file under `internal/mcp/`. Every location-returning tool wraps its position in a uniform `Location{path, line?, column?, blob_hash, snippet?, absolute_path?}` shape (see `location.go`). Response payloads land as both `text` (JSON pretty-printed) and `StructuredContent` on the `CallToolResult` — an agent can consume either. Tool descriptions are the user-facing contract; edit them if behavior changes.

Groups:

- **Escape hatches:** `describe_schema`, `sql_query`.
- **Source:** `read_source`, `list_source_files`, `verify_source`, `list_source_drift`, `search_source` (literal + RE2 grep across every indexed blob). The live-hash mode of `verify_source` and `list_source_drift` require `serve --project-root`.
- **Targets:** `list_targets`, `describe_target`.
- **Symbols:** `find_symbol` (FTS5 with prefix tokens), `describe_symbol` (multi-def + linkage_names + reachability + link_resolutions).
- **Call graph, source view:** `list_callers`, `list_callees`, `list_call_paths` (in-memory DFS, cycle-in-path guard, max_depth cap).
- **Call graph, runtime view:** `list_linked_callers`, `list_linked_callees`, `describe_inline_decisions`.
- **Indirect:** `list_indirect_call_sites`, `list_address_taken_functions`, `resolve_indirect_call`, `list_devirt_hints`.
- **Linkage + reachability:** `describe_link_resolution`, `list_weak_symbols`, `list_undefined_symbols`, `list_icf_groups` (empty until persistence lands), `list_unreachable_symbols`, `list_entry_points`.

## Known gaps

Documented in tool descriptions and in `herbarium-plan.md § Phase 6`:

- `indirect_call_sites.callee_type` and `.field_hint` are always empty — Phase 3 parses the DIEs but doesn't yet walk `DW_AT_call_target` expressions. `resolve_indirect_call` falls back to the full address-taken pool when `callee_type` is empty.
- `list_icf_groups` returns `Total=0` with an explanatory `Note` — ingest parses `.icf` dumps but doesn't persist groups yet. Fixture's `lib/icf_pair.c` is set up so folding fires; only the persistence side is missing.
- `list_entry_points` covers `main` + externally-visible symbols. Constructor-attributed (`__attribute__((constructor))`) and `.init_array` entries are not classified — would need an additional DWARF pass.
- `link_resolutions.losing_objects` is always `[]` — GNU ld's map file only records what was included. Would require `ar t` + per-member `nm` cross-referencing.
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

Build the fixture via `bash testdata/fixture/scripts/build.sh` — this pins the dump flags. If dumps change shape across GCC majors, refresh `testdata/samples/gcc-<version>/` from the fresh builddir.

## Testing

- `go test ./...` covers everything. The MCP tests build a fresh `.hbr` in-process via `collectForTest` in `internal/mcp/collect_helper_test.go` — no test needs `go run ./cmd/herbarium`.
- `TestE2EFixtureContract` in `internal/mcp/e2e_test.go` walks 12 tools and asserts the fixture's full contract; a regression in either ingest or MCP surface trips it.
- Parser tests use pinned samples under `testdata/samples/gcc-16/`. If the pinned GCC version changes, update `store.SchemaVersion` only if the on-disk schema also changes — sample-format drift alone doesn't warrant a bump.
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
