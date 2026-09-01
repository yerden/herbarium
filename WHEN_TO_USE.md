# When to use herbarium

Herbarium indexes **linked reality** — what actually made it into the binary and how the pieces connect. Plain source access (grep + read) answers questions about **source text**. Pick the tool that matches which the question is really about.

If the answer lives in one region of one file, grep+read wins on tokens. Herbarium pays for itself when the question requires facts that only exist after compile + link.

## Use herbarium when the question mentions…

### Callers, callees, or paths
- "Who eventually calls `foo`?"
- "What does `bar` reach?"
- "Shortest path from `main` to `parse_config`?"

Tools: `list_callers`, `list_callees`, `list_call_paths` (compiler cgraph, pre-link); `list_linked_callers`, `list_linked_callees` (post-link, after ICF and dead-strip).

Grep sees direct string matches only — misses cross-TU, misses indirect resolutions, has no transitivity.

### Which definition won at link time
- "Which `.o` contributed the linked copy of `X`?"
- "Was the weak `default_impl` overridden?"
- "Was `foo` folded with `bar` by ICF?"
- "Which symbols are undefined / weak in this target?"

Tools: `describe_link_resolution`, `list_weak_symbols`, `list_undefined_symbols`, `list_icf_groups`.

Source text alone cannot answer these — the answer is in the map file and `nm`/`objdump` output.

### Reachability / dead code
- "Is `helper` reachable from any entry point after `--gc-sections`?"
- "What symbols were dead-stripped from `app1`?"
- "What are the entry points of this binary?"

Tools: `list_unreachable_symbols`, `list_entry_points`, `describe_symbol` (reachability field).

Requires the entry-point set + the linked call graph — no grep query gets there.

### Indirect calls / function pointers
- "What could `g_ops.add` resolve to at this call site?"
- "Which functions have their address taken?"
- "Which dispatch-table-style call sites exist in this target?"

Tools: `list_indirect_call_sites`, `resolve_indirect_call`, `list_address_taken_functions`.

Grep finds the address-take but not the resolution set.

### Compiler decisions
- "Was this call inlined — by which pass, and if not, why not?"
- "Where did this helper's body end up? It has no callers in the binary."
- "Did GCC specialize / clone this function (e.g. `foo.constprop.0`)?"
- "Any devirtualization hints here?"

Tools: `explain_call` (start here for one specific call), `describe_inlining`, `list_inline_instances`, `list_devirt_hints`.

These facts exist only in the compiler's own records — its optimization record, its IPA dumps, and DWARF. There is nothing in source to grep for.

### Multi-definition or ambiguous symbols
- "How many definitions of `init` exist across TUs?"
- "What linkage names does this function have?"
- "Which definition is the one the linker chose?"

Tools: `find_symbol`, `describe_symbol` (returns all definitions, linkage names, reachability, link resolutions in one payload).

Grep finds occurrences; herbarium tells you which one is the linked copy.

## Use plain source access when the question is…

- "How does binary X assign a value of type Y?" — answer is in a function body.
- "Where is this log message emitted?" — string match.
- "What does this macro expand to?" — text.
- "Find all uses of this string constant."
- "Read this file / function and explain what it does."

For these, `grep` + `read` is near-optimal. Herbarium's structured envelopes and multi-tool round-trips are pure overhead on localized reads.

If you want to stay inside the artifact (e.g. to keep answers pinned to indexed blobs), use `search_source` for grep and `read_source` for reads — they route through the packed sources without buying the structured-index tax.

## Combined workflows (herbarium locates, source reads)

- **"Where is the real linked definition of `foo`?"** → `describe_symbol` → `decl_file:line` from DWARF → `read_source`. Beats guessing among headers/impls.
- **"What entry points might reach this suspicious function?"** → `list_call_paths` to enumerate paths → read one path in source.
- **"Is this function used in the shipping binary before I refactor?"** → `describe_symbol` for reachability → decide whether to read at all.
- **"What are all the concrete implementations behind this indirect call?"** → `resolve_indirect_call` → `read_source` on each candidate.

## Rule of thumb

Reach for herbarium if the question involves any of:

> callers · callees · reachability · entry points · weak / strong · ICF · inline · indirect · address-taken · "which `.o`" · "did it get dead-stripped" · "which definition won"

Grep first if the question is:

> "how does X work" · "where is Y" · "find the code that does Z" · "what does this macro / string / log mean"

Escalate from grep to herbarium only if the search fans out across many files or the question quietly turned into a cross-TU / linked-reality question.

## Cost note

Herbarium tools return JSON envelopes with structured metadata alongside the text payload. On localized reads that overhead dominates and direct source access is cheaper. On global / cross-TU / linked-reality questions the index replaces an O(N-file) crawl and the envelope is a rounding error.
