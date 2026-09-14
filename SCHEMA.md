# The `.hbr` schema

Reference for what is in a herbarium index, written from the code as it
stands. The DDL itself is `internal/store/schema.sql`; the design intent
is `herbarium-plan.md § Database schema`. This file is the third thing:
what a row *means*, which pass writes it, and which compiler artifact
the fact ultimately comes from.

It exists so that "herbarium doesn't know X" can be checked against the
schema before anyone reaches for a new column. Regenerate it from the
code when the schema moves — do not hand-patch it.

17 tables, 1 view, 1 FTS index.

Provenance abbreviations used below:

| Short | Means |
|---|---|
| **MI** | `builddir/meson-info/*.json`, read by `internal/mesonintrospect` |
| **.ci / .cgraph / .inline / .icf** | GCC dump sidecars beside each `.o`, parsed by `internal/gccdump` |
| **OR** | `-fsave-optimization-record` JSON (`*.opt-record.json.gz`) |
| **DWARF** | `debug/dwarf` over each `.o`, via `internal/dwarfingest` |
| **nm / objdump / .map** | binutils run at collect time, via `internal/linkplane` |
| **ninja** | the binary `.ninja_deps` log, via `internal/ninjadeps` |

## 0. The join key, and the two path conventions

`symbols.usr` is the only key that crosses planes. External linkage gets
`c:@F@name`; internal linkage gets `c:<tu-path>@F@name`.

Two consequences fall directly out of that scheme and explain most
surprises in this index:

- **A static is a distinct symbol per TU.** One `static inline` in a
  header, included by N TUs, is up to N `symbols` rows with N different
  USRs — not one symbol with N definitions.
- **Internal-linkage USRs are unresolvable at link time**, because the
  linker never saw a per-TU name. This is why `link_resolutions` — and
  therefore the `symbol_reachability` view over it — structurally cannot
  contain them. See §7.

Every other table either holds a `symbols.id` or joins through `usr`.

Paths come in two conventions that **never join to each other**:

- **project-relative** (`app1/main.c`) — the symbol plane, the source
  plane, `target_sources`.
- **builddir-relative** (`app1/main.c.o`) — every `object` /
  `object_file` / `winning_object` column in the inlining, ICF and link
  planes.

## 1. Build-system plane

| Table | A row is | Written by | From |
|---|---|---|---|
| `meta` | one key/value stamp | `store.Init`, then `cmd/herbarium/collect.go` | `schema_version` from `store.SchemaVersion`; `gcc_version`, `meson_version` from MI; `herbarium_version`, `indexed_at`, `project_root_hint` from the collect invocation |
| `targets` | one meson build target | `ingest.Targets` | MI `targets.json` — `name`, `kind`, `link_command` |
| `target_sources` | one TU meson compiles into that target | `ingest.Targets` | MI target `sources` |

`target_sources` holds **compiled TUs only, never headers**. An
executable that links a `lib/` archive therefore lists only its own
`app/` files; the archive's sources answer under the *library* target.

`indexed_at` carries sub-second precision because `reload_index` uses it
to distinguish "the agent re-collected" from "the agent forgot to".

`schema.sql` lists `build_config_hash` among the expected `meta` keys.
Nothing writes it. The five keys above are the complete set.

## 2. Source plane

| Table | A row is | Written by | From |
|---|---|---|---|
| `blobs` | one zstd-compressed file body, keyed by SHA-256 of the **raw** bytes | `internal/blobstore` | file contents |
| `sources` | project-relative path → blob | `ingest.Sources` | target sources + ninja header deps under `--project-root` |
| `external_sources` | verbatim absolute path → blob | `ingest.Sources` | ninja deps **outside** project-root; populated only when an `--include-external` glob matches, empty otherwise |
| `generated_sources` | builddir-relative path → blob | `ingest.Sources` | MI `generated` entries and ninja headers rooted under the builddir; no flag required |

All four share `blobs`, so byte-identical content dedups across them.

This plane is the **only** place types, macros and enum constants exist.
They are text here and nothing anywhere else — no symbol row, no USR, no
kind. A search for one belongs in `search_source`, never `find_symbol`.

## 3. Symbol plane

| Table | A row is | Written by | From |
|---|---|---|---|
| `symbols` | one USR = one identity | inserted by `ingest.Compiler`; `signature` updated by `ingest.DWARF` | `.cgraph` node: `name`, `kind` (first token of the `Type:` line), `linkage` (from `Visibility:` flags), `address_taken`, `linkage_names` including clone suffixes. `signature` is **DWARF-only** |
| `symbols_fts` | FTS5 shadow over `name` + `signature` | rebuilt at the end of `ingest.DWARF` | contentless mode over `symbols`; tokenizer splits on `_` |
| `symbol_definitions` | one **observed definition**; multi-def is normal | inserted by `ingest.Compiler`, repaired by `ingest.DWARF` | `file`/`line` from the `.ci` node, or — where no `.ci` node exists — from DWARF's abstract-instance root; `decl_file`/`decl_line` from DWARF `DW_AT_declaration`; `is_weak`, `linkage_name` from `.cgraph` |

`symbols.kind` is a **closed two-value enum: `function` | `variable`**.
It is the first token of GCC cgraph's `Type:` line, and the cgraph
describes only what reached the assembler, so nothing else can ever
appear. `herbarium-plan.md`'s appendix reserves `typedef` and struct
kinds for a future walker phase; nothing populates them, and
`internal/usr/usr.go`'s `Typedef` helper has no caller.

Multi-def is the normal case, not an edge case: `main` in a
multi-executable project, a weak fallback plus its strong override, a
header inline pulled into several TUs. The `symbols` row stays one.

Two structural facts that bite:

- **`pruneUnreferenced` runs inside `ingest.Compiler`, before the DWARF
  pass exists.** A symbol survives only if it has a def row, is
  address-taken, has non-internal linkage, or is referenced from the
  cgraph-edge / opt-record / ICF planes. DWARF-only evidence — an inline
  instance, an indirect call site — **cannot** save it, because that
  pass has not run yet. This is a deliberate size fix (it removed 82% of
  one real `.hbr`), not an oversight, but it means the compiler plane
  decides what the DWARF plane is allowed to attach to.
- **A symbol can exist with zero `symbol_definitions` rows**, kept alive
  purely by an edge reference. The fixture's `use_dispatch` is exactly
  this. Any query routing symbol → file through `symbol_definitions`
  silently drops those symbols.

## 4. Call plane

| Table | A row is | Written by | From |
|---|---|---|---|
| `call_edges` | one direct call | `ingest.Compiler` writes `source='compiler_cgraph'` with `target_id` NULL; `ingest.Link` writes `source='objdump'` with `target_id` set | `.cgraph` `Called:` lists; disassembly of each linked binary |
| `indirect_call_sites` | one `DW_TAG_call_site` carrying no `DW_AT_call_origin` | `ingest.DWARF` | `caller_id` from the innermost enclosing **inlined-subroutine** name; `file`/`line` resolved from `DW_AT_call_return_pc` through the CU line table; `callee_type`/`field_hint` from `DW_AT_call_target` or the `R_X86_64_PC32` relocation at `return_pc-4` |

`call_edges` is one table holding two incompatible planes, separated
only by `source`. A `compiler_cgraph` row is pre-inlining and
target-agnostic; an `objdump` row is post-optimization and per-target.
No query should touch this table without a `source` predicate.

`indirect_call_sites` attributes to the **source-view** caller, not the
enclosing symbol: when a call comes from code inlined out of F into G,
`caller_id` is F. So the caller of a hot-path dispatch written in a
header is the header's inline function, with everything that implies for
internal linkage.

**There is no devirtualization plane, and there cannot be one for C.**
A `devirt_hints` table, a `list_devirt_hints` tool and a `.devirt`
parser existed through schema v8; none ever held or returned a row. The
table had no writer, but the deeper reason it was removed in v9 is that
GCC's ipa-devirt pass acts on *polymorphic* calls — `OBJ_TYPE_REF` nodes,
which only C++ emits. Every `.devirt` dump herbarium has been pointed at,
fixture and production alike, reports `0 polymorphic calls, 0
devirtualized, 0 speculatively devirtualized`.

The one section of that dump carrying real C data was
`Noted function pointers stored in records`, a `(struct type, byte
offset) → function` table. It appears only where a dispatch table is a
`const` record with a static initializer, is absent from code that
assigns its function pointers at registration time, and where it does
appear DWARF already answers the same question better — `field_hint`
gives the *field name* (`ops.add`), not an offset that would need DWARF
to resolve anyway. Everything else `.devirt` carried is a strict subset
of `.cgraph`, which ingest requires and parses regardless: on the
fixture's `dispatch_impls.c`, `.cgraph` has 12 `Address is taken.` lines
and 18 `(addr)` references where `.devirt` has 2 and 3, because `.cgraph`
dumps at pass `000i`, before unreachable-node pruning.

So `resolve_indirect_call` narrows by type-compatibility over
address-taken functions and nothing else. Its output is a **candidate
list, never a resolution** — no plane in this index names the actual
callee of an indirect call.

## 5. Inlining plane — three tables that disagree on purpose

| Table | A row is | Written by | From |
|---|---|---|---|
| `inline_decisions` | an IPA-stage inline | `ingest.Compiler` | `.cgraph`'s `(inlined)` tag — **IPA only**, blind to the early inliner |
| `inline_records` | any decision GCC logged, rejections included, with its reason | `ingest.Compiler` | OR JSON; `pass` is `einline` (pre-IPA) or `inline` (IPA) |
| `inline_instances` | a body that **survived into the object** | `ingest.DWARF` | `DW_TAG_inlined_subroutine`; `depth` 1 is straight into `caller_id`, deeper rows name the body they landed inside in `parent_callee_id` |

The disagreement is the point, and each plane is wrong in a different
direction:

- `inline_decisions` misses everything the early inliner folded
  (`always_inline`, trivial callees) — those edges are gone before any
  IPA pass writes a dump.
- `inline_records` sees the early pass and keeps rejections, but GCC
  omits the source location on many records (`file=''`, `line=0`), and a
  decision here is not proof the code survived.
- `inline_instances` is ground truth for what is in the object, but a
  callee whose copy folds to a constant afterwards leaves no DIE and so
  no row — it under-reports.

Never answer "was X inlined" from one plane. For the post-link answer on
a specific target, `list_callees` − `list_linked_callees` remains the
definitive diff.

`inline_records` and `inline_instances` carry `object` (builddir-relative);
`inline_decisions` carries no location at all.

## 6. ICF plane

| Table | A row is | Written by | From |
|---|---|---|---|
| `icf_groups` | one non-singular equivalence class, per TU | `ingest.Compiler` | `.icf` dump; `winner_symbol_id` is the surviving symbol, `object_file` is builddir-relative |
| `icf_group_members` | one **loser** of a group — the winner lives on the group row | `ingest.Compiler` | same |

IPA-ICF only. Linker-level ICF (`gold`/`lld --icf=all`) is a separate
pass and is represented nowhere in this schema, so if the linker folds
further, this plane under-reports.

## 7. Link plane

| Table | A row is | Written by | From |
|---|---|---|---|
| `link_resolutions` | "target T resolved USR U to this object" | `ingest.Link` | `winning_object` from the `.map` file when present, else a deterministic heuristic (strong > weak > local) over an nm scan of every `.o`; `losing_objects` from that same nm scan; `linkage_kind` from nm; `archive` set when the definition came from a `.a` |
| `symbol_reachability` | a **VIEW**, not a table: `link_resolutions ⋈ symbols` on `usr` | — | emits `reachable=1` rows only; `section_kept` is hardcoded 1 |

Because it is a view over `link_resolutions`, `symbol_reachability` has
no `reachable=0` row to find. Absence *is* the negative answer — test
with `NOT EXISTS` or a `LEFT JOIN`, never `WHERE reachable = 0`.

`losing_objects` is broader than a strict map-file implementation: it
comes from an nm scan across every `.o` in the builddir, so it lists
other objects that also define the name, including archive members the
linker never pulled in.

### What this plane structurally cannot answer

`link_resolutions` is keyed by names the **linker** saw, so internal
linkage never reaches it, and therefore never reaches the
`symbol_reachability` view. Any filter phrased as "this symbol is
reachable in target T" silently excludes every `static` and every header
`static inline` — which is precisely what a dispatch table is made of.

The gap is wider than that one filter. Inspecting the whole schema:

- `target_sources` maps targets to TUs, and holds no headers.
- every `object` / `object_file` / `winning_object` column names a `.o`,
  and **no table maps a `.o` to a target**.
- an internal-linkage symbol with no def row (§3) has no file to join on
  either; its TU survives only inside the USR string.

So **"is this internal-linkage symbol's code in target T" is not
answerable from the current schema by any query.** Tools that scope by
target answer a narrower question than their argument name suggests, and
their empty results must be read as "nothing resolved under that name",
never as "no such code".

## Known-inert corners

All confirmed by grep against the current tree, not inferred:

| What | Status |
|---|---|
| `meta.build_config_hash` | listed in `schema.sql`, never stamped |
| `symbol_reachability.section_kept` | constant 1; parsing "Discarded input sections" is future work |
| `usr.Typedef` | defined, no caller |
