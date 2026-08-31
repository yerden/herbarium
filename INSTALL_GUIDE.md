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
            -fdump-ipa-devirt -fdump-ipa-icf \
            -fno-inline-functions-called-once"
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
| `-fno-inline-functions-called-once` | Keeps distinct nodes in the post-IPA `.cgraph` for single-caller helpers that `-O2` would otherwise inline out of the `.ci` direct-edge view |

Transitive header sets come from ninja's consolidated `.ninja_deps`, which the
build already writes; no extra flag is needed for them.

### Optional, but they unlock whole tool groups

**Dead-strip reachability.** Add `-ffunction-sections -fdata-sections` to
`c_args` and `-Wl,--gc-sections` to the link args. Without them,
`list_unreachable_symbols` and `describe_symbol`'s reachability field have
nothing to report — everything survives the link.

**Linker map files.** These make the link plane exact instead of heuristic.
Maps are matched to targets by filename, so each map must be named
`<target>.map` at the top of the builddir — which means per-target `link_args`
in `meson.build`, not a global `-Dc_link_args`:

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
