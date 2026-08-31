# Install guide: index a Meson/C project and serve it in opencode

End-to-end walkthrough, from a source checkout to herbarium's tools showing up
in an opencode session. See [`WHEN_TO_USE.md`](WHEN_TO_USE.md) for deciding
which questions are worth routing through the index once it is up.

## 1. Build the binary

```sh
go build -o herbarium ./cmd/herbarium
```

Go ≥ 1.26. Keep the resulting path — opencode needs an absolute one in step 6.

## 2. Configure the Meson build with the dump flags

Herbarium never invokes `meson`, `ninja`, `gcc`, or `ld`: you produce the build,
it reads what the toolchain already wrote to disk. Preflight refuses to index a
builddir missing any required dump and reprints the corrected `meson setup`
line, so a wrong invocation here fails loudly rather than degrading the index.

```sh
cd /path/to/your/c-project
meson setup builddir \
  --buildtype=debugoptimized \
  -Dc_args="-g -gcolumn-info -fcallgraph-info=su,da \
            -fdump-ipa-cgraph -fdump-ipa-inline \
            -fdump-ipa-devirt -fdump-ipa-icf"
```

GCC ≥ 10 is required (`-fcallgraph-info` landed there); preflight checks the
compiler version reported by Meson. What each flag buys:

| Flag | Required for |
|---|---|
| `-g -gcolumn-info` | Symbol identity, decl/def locations, signatures, typedef chains, struct field names |
| `-fcallgraph-info=su,da` | Direct call edges, stack usage, data-area sizes |
| `-fdump-ipa-cgraph` | Post-IPA callgraph with `address_taken` flags and indirect call site records |
| `-fdump-ipa-inline` | Inlining decisions |
| `-fdump-ipa-devirt` | Speculative devirtualization hints for indirect calls |
| `-fdump-ipa-icf` | Identical-code-folded function groups |

Transitive header sets come from ninja's consolidated `.ninja_deps`, which the
build already writes; no extra flag is needed for them.

### One flag deliberately left out

Every flag above is codegen-inert. `-g -gcolumn-info` only emit DWARF;
`-fcallgraph-info` and the four `-fdump-ipa-*` only write files next to the
object. None of them participates in an optimization decision, so `.text` is
byte-identical to a stock build and the binary can still be stripped for
shipping. The index therefore describes the binary you actually ship.

There is one more flag herbarium can use, left out of that block on purpose:
`-fno-inline-functions-called-once`. It is the only flag in the set that
changes generated code. Its effect is narrow — a `static` function with exactly
one caller stays out-of-line instead of being inlined on the called-once rule
alone. Nothing is reordered and no semantics change; it withholds one specific
inlining decision for one class of function.

**Preflight does not check for it.** The gates are GCC version, objects
present, `.ci` present, `.cgraph` present, and `.debug_info` present — so this
is purely your call, and the failure message offers it as an explicit opt-in
below the recommended `meson setup` line.

Leaving it out costs you this: single-caller helpers that `-O2` inlined will
not appear as distinct nodes or edges. That is usually the answer you want,
since they are not distinct in the binary either. Add the flag when you are
reading the index as a *source-level* call graph and would rather see those
helpers than match the shipped artifact byte for byte.

### Two settings that are yours to decide, not herbarium's

The same reasoning governs the two below: index the build you actually care
about rather than adding flags to please the indexer. Neither is expensive —
one changes the binary you ship, the other needs an edit to `meson.build`.

- If your real build already dead-strips, index a build that dead-strips.
- If it does not, adding `--gc-sections` here produces an index that describes
  a binary you never ship. That is worse than an index with a quiet
  `list_unreachable_symbols`.

**Dead-strip reachability** — `-ffunction-sections -fdata-sections` in `c_args`,
`-Wl,--gc-sections` in the link args.

Reachability is not a graph traversal herbarium performs. `symbol_reachability`
is a view over `link_resolutions`, and `link_resolutions` is built from `nm` on
the *finished binary* — so "reachable" means the linker actually kept a
definition. Link without `--gc-sections` and the linker keeps everything it
pulled in, so every symbol resolves, `list_unreachable_symbols` comes back
empty, and `describe_symbol`'s reachability field reports `reachable` for
things nothing calls. The tools do not break; they stop being informative.

Both halves are needed. `--gc-sections` collects at section granularity, so
without `-ffunction-sections` a TU's functions share one `.text` section and
nothing is stripped short of the whole TU going unused.

Cost: `-ffunction-sections -fdata-sections` add a section header and
relocations per function, so objects grow and the link does more work — the
emitted instructions per function are unchanged, and with `--gc-sections` the
final binary usually ends up smaller. The thing to actually weigh is
`--gc-sections` itself: it can strip a section reached only from hand-written
asm or a custom linker script, which is what `__attribute__((used))` and
`KEEP()` exist for. Default linker scripts already `KEEP` `.init_array`, so
constructors survive.

**Linker map files** — `-Wl,-Map=<target>.map` per target.

Build-time cost is negligible: ld writes one text file it already has the
information for. The friction is purely mechanical — maps are matched to
targets by filename, so the map must be named exactly `<target>.map` at the
top of the builddir. A global `-Dc_link_args` would point every target at one
filename, so this has to go in `meson.build`:

```python
executable('myapp', myapp_srcs,
  link_args: ['-Wl,-Map=myapp.map', '-Wl,--gc-sections'])
```

Without a map, `link_resolutions.winning_object` falls back to a per-`.o` `nm`
scan with a strong > weak > local heuristic. That still identifies weak-vs-strong
resolution correctly; the case it cannot resolve is two same-named statics in
two TUs of the same target, which fall back to name lookup and may misattribute.

## 3. Build

```sh
meson compile -C builddir
```

The `.ci`, `.cgraph`, `.inline`, `.devirt`, and `.icf` dumps land beside each
`.o`. They can be deleted after a successful collect with no loss of index
correctness — the next collect just needs them again.

## 4. Collect

```sh
/abs/path/to/herbarium collect \
  --builddir builddir \
  --project-root . \
  --out myproject.hbr
```

`nm` and `objdump` must be on `PATH` for this step, and only this step — serve
mode shells out to nothing. Each invocation logs its command line, elapsed
time, and payload size to stderr.

Flags worth knowing:

| Flag | Effect |
|---|---|
| `--target NAME[,NAME...]` | Restrict `nm`/`objdump`/map work to these targets. Repeatable. Compiler-plane ingest (symbols, cgraph edges, DWARF) still covers every TU, so this is a fast slice, not a partial index. An unknown name is a hard error that lists what is available. |
| `--strict` | Refuse to pack any source whose mtime is newer than its `.o`. Use it when you need a guarantee that packed blobs match the DWARF line numbers. |
| `--include-external GLOB` | Pack headers from outside `--project-root` (e.g. `/usr/include/**`) into `external_sources`. Repeatable; a zero-match glob is a hard error. |

`collect` refuses to overwrite an existing `.hbr`. Incremental re-ingest is
deferred, so every run is a full rebuild — `rm` the old artifact first (step 8).

A failing preflight looks like this, and no indexing proceeds:

```
herbarium preflight failed for builddir
  detected: GCC=16.2.1 Meson=1.9.1

  1. [missing_cgraph] .cgraph dump missing for 12/40 objects (sample: …)
     fix: add `-fdump-ipa-cgraph` (and the other -fdump-ipa-* flags) to c_args and rebuild
```

## 5. Smoke-test the artifact

```sh
herbarium serve --hbr myproject.hbr --check
```

Opens the `.hbr` read-only, registers all 27 tools, prints
`herbarium serve --check: … opens (schema N)`, and exits. This catches a schema
mismatch between binary and artifact before opencode sees a transport that dies
on startup.

## 6. Wire it into opencode

Project-scoped config at the root of the C project you indexed —
`<your-c-project>/opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "herbarium": {
      "type": "local",
      "command": [
        "/abs/path/to/herbarium",
        "serve",
        "--hbr", "/abs/path/to/myproject.hbr",
        "--project-root", "/abs/path/to/your/c-project"
      ],
      "enabled": true
    }
  }
}
```

Use absolute paths throughout — the working directory of the spawned process is
not guaranteed. stdout carries the JSON-RPC framing and all banners go to
stderr, so stdio needs no extra flags.

`--project-root` is optional at serve time. It enables only the live-file modes
of `verify_source` and `list_source_drift`, which compare the on-disk checkout
against the packed blobs. Everything else answers from the `.hbr` alone, which
means the artifact is portable: copy it to another machine, drop
`--project-root`, and the other 25 tools work unchanged.

### HTTP transport

If you would rather run one server for several sessions:

```sh
herbarium serve --hbr myproject.hbr --project-root . \
  --transport http --http-addr :7473
```

```json
"herbarium": { "type": "remote", "url": "http://localhost:7473/mcp", "enabled": true }
```

The `/mcp` path is the streamable-HTTP server's default endpoint; `--http-addr`
sets only host and port.

## 7. Verify inside opencode

Start opencode in the project directory and have it call `list_targets`, then
`describe_symbol` on a function you know is in the binary. A healthy index
returns definitions, linkage names, reachability, and link resolutions in one
payload.

If the transport dies with `MCP error -32000: Connection closed`, the cause is a
tool registered with `mcp.NewTool` instead of the local `newTool` helper —
`mcp.NewTool` defaults `destructiveHint=true`, which contradicts `readOnly=true`
and some clients reject it. `TestToolAnnotationsAreConsistent` guards against
this; a green `go test ./internal/mcp/` rules it out.

## 8. Re-index after a rebuild

```sh
meson compile -C builddir
rm -f myproject.hbr
herbarium collect --builddir builddir --project-root . --out myproject.hbr
```

Restart the MCP server afterward. `serve` holds the file open read-only for the
life of the process and will not pick up a replacement underneath it.
