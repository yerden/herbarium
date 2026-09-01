# herbarium

A GCC-native C code index for AI agents. Ingests an already-built Meson C project into a single portable SQLite artifact (`.hbr`) and serves it over MCP so an agent can answer questions about the code without re-parsing anything.

Every fact in the index traces back to something the compiler or linker already wrote to disk — GCC's callgraph and IPA dumps, DWARF, `nm`, `objdump`, and (optionally) the linker's map file. No CC wrapper, no plugin, no source re-parse. Herbarium never invokes GCC, `ld`, `meson`, or `ninja`; the build is entirely the user's responsibility.

**Status:** early / dev. Schema and MCP surface will move as the design shakes out. The design contract lives in [`herbarium-plan.md`](herbarium-plan.md).

## How it works

Two subcommands, each with a narrow contract:

- `herbarium collect --builddir DIR --project-root DIR --out FILE` — reads the builddir and writes a `.hbr`. Runs `nm` and `objdump` against the finished binaries; that's the extent of subprocess use.
- `herbarium serve --hbr FILE [--project-root DIR]` — opens an `.hbr` read-only and exposes 29 MCP tools. Zero external subprocess deps at serve time. Stdio by default; `--transport http` switches to streamable HTTP.

The `.hbr` file is the whole artifact: schema, facts, and compressed source blobs of every file the build touched. Portable across machines.

## Requirements

- **GCC ≥ 10** (needed for `-fcallgraph-info`). Preflight refuses older compilers.
- **Meson + ninja** — the build itself.
- **binutils** — `nm` and `objdump` must be on `PATH` at collect time only.
- **Go ≥ 1.26** to build herbarium from source.

## Build

```sh
go build ./cmd/herbarium
```

## Prepare the build

Configure the builddir with the diagnostic flags herbarium needs. Preflight will refuse to index a builddir missing any of them and print the exact `meson setup` line to fix it. For the full walkthrough — optional flags, linker maps, and wiring the server into an MCP client — see [`INSTALL_GUIDE.md`](INSTALL_GUIDE.md).

```sh
meson setup builddir \
  -Dc_args="-g -gcolumn-info -fcallgraph-info=su,da \
            -fdump-ipa-cgraph -fdump-ipa-inline \
            -fdump-ipa-devirt -fdump-ipa-icf \
            -fsave-optimization-record"
meson compile -C builddir
```

Every flag there is codegen-inert — `-g` emits DWARF, the rest only write dump files beside the object — so `.text` stays byte-identical to a stock build and the index describes the binary you actually ship. (`-fsave-optimization-record` is recorded in `DW_AT_producer`, so the `.o` differs there and nowhere else; add `-gno-record-gcc-switches` if you need the object bit-identical too.)

One optional flag changes that. `-fno-inline-functions-called-once` keeps single-caller statics out-of-line so they survive as distinct `.cgraph` nodes, at the cost of a narrow but real divergence from the shipped binary. It is not required and preflight does not check for it; add it only if call-graph legibility matters more to you than byte-fidelity.

Linker map files are optional but improve link-plane precision. Enable them per target in `meson.build`:

```python
executable('myapp', myapp_srcs, link_args: ['-Wl,-Map=myapp.map'])
```

Without a map, herbarium falls back to a per-.o nm scan for `winning_object` and static-symbol disambiguation. Same-named statics in two TUs of the same target can only be disambiguated with a map file.

## Collect and serve

```sh
# Ingest the builddir into a single .hbr file.
herbarium collect \
  --builddir builddir \
  --project-root . \
  --out project.hbr

# Serve it over MCP on stdio (default).
herbarium serve --hbr project.hbr --project-root .
```

`--project-root` at serve time is optional; it's only needed if you want live source-hash verification (`verify_source`, `list_source_drift`).

## MCP tools

The 29 tools are grouped by concern. Every location-returning tool wraps its position in a uniform `{path, line, column, blob_hash, snippet, absolute_path}` shape.

**Escape hatches** — `describe_schema`, `sql_query`.

**Source** — `read_source`, `list_source_files`, `search_source`, `verify_source`, `list_source_drift`.

**Targets** — `list_targets`, `describe_target`.

**Symbols** — `find_symbol` (FTS5 prefix search), `describe_symbol` (multi-def + linkage_names + reachability + link_resolutions in one call).

**Call graph, source view** — `list_callers`, `list_callees`, `list_call_paths`.

**Call graph, runtime view** — `list_linked_callers`, `list_linked_callees`, `describe_inlining` (three planes: every pass's decisions from the optimization record, the inlined bodies DWARF says survived, and the older `.cgraph` per-edge tag as a cross-check), `list_inline_instances` (where a function's body ended up), `explain_call` (one verdict for one call — `inlined_and_present`, `inlined_then_folded`, `declined` with GCC's reason, or `no_decision_logged` — plus the evidence behind it).

**Indirect calls** — `list_indirect_call_sites`, `list_address_taken_functions`, `resolve_indirect_call`, `list_devirt_hints`.

**Linkage and reachability** — `describe_link_resolution`, `list_weak_symbols`, `list_undefined_symbols`, `list_icf_groups`, `list_unreachable_symbols`, `list_entry_points`.

Tool descriptions in [`internal/mcp/`](internal/mcp/) are the user-facing contract; they document the exact semantics of each field.

## Design principles

1. **Compiler truth over source guesses.** No AST re-parse. No CC wrapper. No plugin.
2. **One artifact.** Everything lives inside `<name>.hbr`.
3. **USR is the only join key.** Every new schema table joins through an existing USR.
4. **Serve mode has zero external subprocess deps.** All `nm`/`objdump` runs happen at collect time only.
5. **Read-only at query time.** `store.OpenReadOnly` uses `mode=ro + query_only(1)`; the driver rejects writes. `sql_query` relies on this rather than parsing SQL.

## Known limitations

- `indirect_call_sites.callee_type` / `.field_hint` are resolved from `DW_AT_call_target` or, where GCC emits none, from the call instruction's relocation. Both routes are x86-64-only. Both decline rather than guess: a computed pointer, or a parameter list whose SysV register assignment can't be replayed (a leading `double` shifts every later argument's register), leaves both columns empty and `resolve_indirect_call` falls back to the full address-taken pool.
- `list_icf_groups` covers IPA-ICF only (from GCC's `.icf` dumps). Linker-level ICF (`gold`/`lld --icf=all`) is not tracked.
- `list_entry_points` covers `main` + externally-visible symbols only. `__attribute__((constructor))` and `.init_array` entries aren't classified.
- `link_resolutions.losing_objects` is broader than a strict map-file impl: nm sees every .o on disk, including archive members ld never pulled in. Read it as "other .o's that also define this symbol", not "candidates ld weighed and rejected".
- `symbol_reachability.section_kept` is always 1 — parsing "Discarded input sections" from the map file is future work.

## Repository layout

```
cmd/herbarium/          collect + serve subcommands
internal/
  mesonintrospect/      reads builddir/meson-info/*.json
  builddir/             enumerates .o files + sidecar dumps
  preflight/            refuses to index under-flagged builddirs
  store/                schema.sql + open/init/ro helpers
  blobstore/            zstd + SHA-256 content-addressed source blobs
  ninjadeps/            hand-rolled parser for ninja's .ninja_deps log
  gccdump/              per-dump-kind parsers (ci, cgraph, inline, icf, devirt)
  dwarfingest/          DWARF reader
  linkplane/            nm + objdump + map file parsers
  usr/                  USR synthesis
  ingest/               pipeline orchestrator
  mcp/                  MCP server + 29 tools
testdata/
  fixture/              minimal Meson project the tests build against
  samples/gcc-16/       pinned parser fixtures
```

## Further reading

- [`INSTALL_GUIDE.md`](INSTALL_GUIDE.md) — step-by-step: build, configure Meson, collect, and serve into an MCP client.
- [`WHEN_TO_USE.md`](WHEN_TO_USE.md) — which questions are worth routing through the index instead of grep.
- [`herbarium-plan.md`](herbarium-plan.md) — the design contract. Read this before making non-trivial changes.
- [`CLAUDE.md`](CLAUDE.md) — orientation for working in this repo.
