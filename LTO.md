# LTO is not supported

**Short version:** `herbarium collect` refuses to index a builddir built with
`-flto`, and that refusal is correct. Link-time optimization moves every
compiler pass herbarium reads facts from out of the compile step and into the
linker, where GCC writes the dumps to a temporary directory it then deletes.
`-ffat-lto-objects` recovers most of what goes missing but not all of it, and
what it recovers describes a compilation that did not produce the shipped
binary.

Build the indexing builddir with LTO off. It does not have to be the builddir
you ship from — every flag herbarium requires is codegen-inert, so a
`-Db_lto=false` builddir configured for indexing is byte-identical to a stock
non-LTO build.

## What was measured

GCC 16.2.1, Meson 1.12.0, binutils `nm`/`objdump` from the same toolchain,
against `testdata/fixture`. Three builddirs, identical except for the LTO
flags:

```sh
c_args="-g -gcolumn-info -fcallgraph-info=su,da -fdump-ipa-cgraph \
        -fdump-ipa-inline -fdump-ipa-icf -fsave-optimization-record \
        -fno-inline-functions-called-once -ffunction-sections -fdata-sections"

# baseline
bash testdata/fixture/scripts/build.sh /tmp/plain-bd

# thin LTO (what -Db_lto=true gives you)
meson setup /tmp/lto-bd --buildtype=debugoptimized -Db_lto=true \
  "-Dc_args=$c_args" testdata/fixture && meson compile -C /tmp/lto-bd

# fat LTO
meson setup /tmp/fat-bd --buildtype=debugoptimized -Db_lto=true \
  "-Dc_args=$c_args -ffat-lto-objects" testdata/fixture && meson compile -C /tmp/fat-bd
```

`build.ninja` confirms Meson passes every diagnostic flag through unchanged in
all three cases — nothing below is Meson dropping a flag.

## Thin LTO: three of the four inputs vanish

Sidecars beside each `.o`:

| artifact | baseline | thin LTO | fat LTO |
|---|---|---|---|
| `*.ci` (`-fcallgraph-info`) | present | **absent** | **absent** |
| `*.095i.inline` (`-fdump-ipa-inline`) | present | **absent** | present |
| `*.000i.cgraph` | present | present | present |
| `*.089i.icf` | present | present | present |
| `*.opt-record.json.gz` | present | present | present |
| `.debug_info` in the `.o` | present | **absent** | present |

`collect` against the thin-LTO builddir:

```
herbarium preflight failed for /tmp/lto-bd
  detected: GCC=16.2.1 Meson=1.12.0

  1. [missing_ci] .ci dump missing for 7/7 objects (sample: .../app1.p/main.c.o)
     fix: add `-fcallgraph-info=su,da` to c_args and rebuild
  2. [no_debug_info] no .debug_info section in .../app1.p/main.c.o — was -g in effect?
     fix: add `-g -gcolumn-info` to c_args and rebuild
```

The `.o` is a GIMPLE container: `.gnu.lto_*` sections, and a `.text` of 48
bytes against the baseline's 271.

Note that both fix hints are wrong — the flags they name are already on the
command line. Following them produces the identical failure. See
[What preflight should say](#what-preflight-should-say).

## Fat LTO: still refused, and now it also misleads

`-ffat-lto-objects` compiles each TU twice, so DWARF and the IPA-inline dump
come back and the `.o` carries a real 271-byte `.text`, identical in size to
the baseline. Preflight still refuses, on one finding:

```
  1. [missing_ci] .ci dump missing for 7/7 objects
```

GCC does not write `.ci` under `-flto` at all, fat or thin. Since the compiler
plane is anchored on the callgraph-info dump, that single gap is enough.

It is the right outcome for a second reason. The linker discards the fat
`.text` and re-optimizes from the GIMPLE, so a fat-LTO index — if preflight let
one through — would describe per-TU compilations that were thrown away.

## Why the dumps are not there

Under `-flto` the compile step stops after the early IPA passes and streams
GIMPLE to the object. The real inliner, IPA-ICF, the final cgraph and the
assembly output that `-fcallgraph-info` hooks into all run at link time, in
WPA and ltrans. What survives beside the `.o` is the compile-stage view only:
`000i.cgraph` is the pre-IPA symbol table, and the `.icf` dump and optimization
record describe decisions the link stage is free to redo.

So the flags are not being ignored. They are being honored at a stage whose
output herbarium never sees.

## Even with the dumps, the two planes would disagree

This is the part that makes "relax preflight and ingest what we can" a bad
trade. The link plane (`nm`/`objdump` over the finished binary) stays truthful
under LTO; the compiler plane would describe pre-LTO code. Functions defined in
`app1` (baseline → fat LTO):

```
baseline:  add_ints compute hook icf_add_one icf_bump_by_one main mul_ints
fat LTO:   add_ints main mul_ints
```

`compute`, `hook`, `icf_add_one` and `icf_bump_by_one` are gone — cross-TU
inlining plus `--gc-sections`. Direct call instructions in the binary drop from
12 to 6.

Herbarium joins the two planes by name into a USR. Every symbol above would
have a `symbols` row from the compiler plane and no `link_resolutions` row, and
`list_unreachable_symbols` is literally `symbols` minus `link_resolutions` — so
the tool whose job is finding dead code would report most of the live code as
dead. `linkage_names` cannot rescue that join: it resolves a renamed symbol
back to its source name, and these symbols are not renamed, they are absent.
(WPA does also privatize and rename survivors — the fixture is too small to
produce one — which is a second joining problem on top of this one.)

A loud refusal is better than an index that is confidently wrong about
reachability.

## Secondary: nm reads the plugin's symbol table, not the ELF one

On any LTO object — fat included — `nm --defined-only --format=posix` returns
rows with an empty size column, because binutils resolves the symbols through
the LTO plugin's GIMPLE symbol table rather than the ELF `.symtab`:

```
baseline:  compute T 0 29
LTO:       compute T 0
```

`parseNMPosix` tolerates the short row and leaves `Size` at 0, so this is not a
crash — but it is one more reason the link plane cannot be trusted to match the
compiler plane under LTO.

## What support would actually require

The data does exist. `-flto -save-temps` keeps the link-stage artifacts:

```sh
gcc -O2 -g -flto -fcallgraph-info=su,da -fdump-ipa-inline -save-temps *.c -o out
# leaves: out.ltrans0.ltrans.ci, out.wpa.095i.inline, out.ltrans0.ltrans.s, ...
```

Both dumps herbarium needs are there. The obstacle is their unit. Those files
are named for **the linked output and its ltrans partition**, not for a
translation unit: one set per executable, partitioned by the linker, with a
partition count that depends on `-flto=N` and the WPA balancing. That breaks
two load-bearing assumptions at once:

- `builddir.resolveSidecars` derives every sidecar path from the `.o`
  basename. Under LTO there is no per-`.o` dump to find.
- The USR scheme anchors an internal-linkage symbol at its TU
  (`herbarium-plan.md` appendix). A partition is not a TU, and after WPA
  privatization the symbol's name no longer identifies its origin TU either.

Supporting LTO is therefore a new ingest shape — partition-keyed rather than
object-keyed, driven off the link step rather than the compile step — not a
parser tweak. It is not planned.

## What preflight should say

Today preflight diagnoses the symptom and prints a fix that cannot work. The
improvement is to detect LTO directly — `.gnu.lto_*` sections in the sampled
`.o`, or `-flto` in `compile_commands.json` — and emit a finding that names the
real cause and the real fix:

> this builddir was configured with `-flto`; herbarium cannot index an LTO
> build, because GCC runs the passes it reads at link time. Configure the
> indexing builddir with `-Db_lto=false` — it need not be the builddir you ship
> from.

and suppress the misleading `missing_ci` / `no_debug_info` findings when it
fires. Not implemented yet.
