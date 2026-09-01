## Herbarium — reachability analysis

Ground truth for "is symbol X in the shipped binary?" is the `link_resolutions`
table (linker map, post-inlining, per-target). A symbol with a definition but
no `link_resolutions` row for the target was dead-stripped, never pulled from
its archive, or fully inlined away. See "Inlined-away vs dead-stripped" below
to tell those apart.

### Workflow

1. `herbarium_list_targets` — confirm the target name and kind.
2. `herbarium_list_unreachable_symbols` (takes a `target` argument) — the
   default sweep. It selects every symbol with a definition and no
   `link_resolutions` row for that target, with no additional filtering; read
   the results with the false-positive classes below in mind.
3. For custom filtering (name prefix, external-linkage-only, etc.) drop to
   `herbarium_sql_query`:

   ```sql
   SELECT s.name, s.kind, s.linkage, sd.file, sd.line
   FROM symbols s
   JOIN symbol_definitions sd ON sd.symbol_id = s.id
   WHERE s.name GLOB '<prefix>_*'       -- your prefix
     AND s.linkage = 'external'         -- skip internal-linkage noise
     AND sd.line > 0                    -- skip header-defined statics
     AND NOT EXISTS (
       SELECT 1 FROM link_resolutions lr
       JOIN targets t ON t.id = lr.target_id
       WHERE lr.usr = s.usr AND t.name = '<target>')
   ```

4. Verify each surprising hit before calling it dead code (see "Verifying").

### False positives

**Internal-linkage symbols.** Static functions and `static` data never have a
`link_resolutions` row — `ingest.Link` filters out every nm entry with
local-symbol type (`t`, `d`, `b`, `r`) because those lines don't identify a
definition uniquely across TUs. Treat any `s.linkage != 'external'` result as
"unverified", not "dead".

Worst case: `static const` data defined in a header (`foo_dict`-style tables).
Every including TU gets its own copy with a per-file USR
(`c:<file>@V@name`), attributed to the including `.c` file at line 0, and
none of the copies appear in the map. Filter with `sd.line > 0`.

**ICF-folded symbols.** GCC's identical-code folding merges two identical
function bodies into one symbol; the loser has no `link_resolutions` row of
its own and looks dead. `herbarium_list_icf_groups` currently returns
`Total=0` because ICF-group persistence is not yet implemented — until then,
if a suspect symbol has an obvious same-signature sibling in the same target,
suspect a fold before you call it dead.

**Constructor / `.init_array` reachability.** `herbarium_list_entry_points`
covers `main` and externally-visible symbols but does not classify
`__attribute__((constructor))` functions or `.init_array` entries. A whole
subgraph reachable only from a constructor will therefore appear unreachable
in every automated view. Use `herbarium_describe_symbol` on the suspect and
inspect whether its callers are constructor-attributed.

**Indirect-only reachability.** A symbol reached only via function pointers
is invisible to both cgraph and objdump edge lists, and often absent from
`link_resolutions` reasoning too. See "Verifying" below.

### Inlined-away vs dead-stripped

A missing `link_resolutions` row can mean the symbol was inlined at every
call site or that it was truly stripped. To disambiguate:

- `herbarium_list_inline_instances` — if it returns instances, the symbol's
  body is physically present inside its callers: inlined away, not
  stripped. This is the direct answer and it covers folds the IPA dumps
  never saw (`always_inline`, trivial static callees).
- `herbarium_explain_call` — for one caller/callee pair this returns a
  single verdict: `inlined_and_present`, `inlined_then_folded`,
  `declined` (with GCC's reason), or `no_decision_logged`. Prefer it over
  reading the planes yourself.
- `herbarium_describe_inlining` — the survey form, for every call in one
  function. `records` gives each pass's verdict; `cgraph_edges` is
  IPA-stage only, so `inlined=0` there does not mean the call survived.
- Cross-check by comparing `herbarium_list_callers` (cgraph, pre-inlining)
  with `herbarium_list_linked_callers` (objdump, post-inlining). Callers
  present in the first but absent from the second are the inlined sites.
- If neither view shows any callers, the symbol is truly unreferenced from
  code — unless it is reached only indirectly (see below).

### Verifying a suspicious hit

- `herbarium_describe_symbol` — the one-shot check. Returns per-target
  `link_resolutions`, all definitions, and the `address_taken` flag in a
  single call; usually enough to confirm or reject a "dead" verdict for one
  symbol.
- `herbarium_describe_link_resolution` — the per-object-file breakdown
  (winning object, archive, linkage kind). Reach for this only when
  `describe_symbol`'s summary isn't enough.
- `herbarium_list_indirect_call_sites` — indirect calls are invisible to
  both cgraph and objdump edge lists. If the only path to a symbol goes
  through a function pointer (`.fn` fields, handler tables), neither view
  shows it. Check `herbarium_list_devirt_hints` and
  `herbarium_list_address_taken_functions` too.

### Semantics cheat sheet

| View | What it sees | Misses |
|---|---|---|
| cgraph (`list_callers`) | source-level direct calls, pre-inlining | indirect calls |
| objdump (`list_linked_callers`) | runtime direct calls, post-inlining | indirect calls, data refs |
| `link_resolutions` | linker map, per-target | internal-linkage symbols, ICF losers, COMDAT details |

**Silent misses across all three views:** ICF folds (until persistence
lands), symbols reachable only from constructors / `.init_array`, and
symbols reachable only via function pointers. Anything in these categories
will look dead in every automated view — trace the dispatch manually via the
indirect-call and address-taken tools.
