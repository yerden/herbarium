// Package gccdump parses GCC's textual dump files produced by
// -fcallgraph-info and -fdump-ipa-*. Each parser is a pure function
// from a file path (or a bytes slice) to a typed result; the ingest
// orchestrator ties results across TUs together and populates the DB.
//
// Every string in these result types is verbatim from the dump — no
// path resolution, no USR synthesis. Those transformations belong to
// the ingest layer so the parsers stay dependency-free and testable.
package gccdump

// Function is one entry from the .cgraph post-IPA symbol table.
// A function may appear across multiple TU dumps (external linkage or
// static-inline in a header); the ingest layer aggregates.
type Function struct {
	// LocalID is the per-dump identifier like "5" in "main/5". Not
	// globally unique — used only to resolve edges within one .cgraph.
	LocalID string
	// Name is the source identifier (unqualified).
	Name string
	// LinkageName is what appears in parens after the id; usually equals
	// Name, but for clones it's the specialized suffix form
	// (e.g., "use_dispatch.constprop") and for anonymous symbols it may
	// be something GCC-mangled.
	LinkageName string
	// Kind is 'function' or 'variable' — the first token of the Type line.
	Kind string
	// Analyzed is true when the entry has "definition analyzed" — i.e.
	// this TU actually contains the definition.
	Analyzed bool
	// BodyRemoved is set when "Body removed by symtab_remove_unreachable_nodes"
	// appears — the compiler tossed the body.
	BodyRemoved bool
	// VisibilityFlags are the space-separated tokens after "Visibility:"
	// (e.g., {"externally_visible", "semantic_interposition", "public", "weak"}).
	VisibilityFlags []string
	// AddressTaken reflects the "Address is taken." standalone line.
	AddressTaken bool
	// FunctionFlags are the tokens after "Function flags:" — 'body',
	// 'only_called_directly', 'only_called_at_startup', 'executed_once', etc.
	// The 'count:…' pseudo-token is stripped.
	FunctionFlags []string
	// CloneOfID is the parent's LocalID when this entry is a clone
	// ("Clone of X/N"), else "". The Name of a clone still points at
	// the parent's Name; the LinkageName carries the specialized suffix.
	CloneOfID string
	// Refs are outgoing references (variable accesses / function
	// address-takes). Each carries the target's local id and the kind
	// (read | write | addr | alias).
	Refs []Ref
	// Called records outgoing direct calls per "Calls:" line.
	Called []Call
	// IndirectSites records indented "indirect simple callsite …" lines
	// under the "Calls:" section. Populated with SpeculativeTargets set
	// when the compiler resolved any (rare in pure C).
	IndirectSites []IndirectSite
}

// Ref is one entry in the References or Referring list.
type Ref struct {
	TargetLocalID string
	Kind          string // "read" | "write" | "addr" | "alias"
}

// Call is one entry in the Calls or Called by list.
type Call struct {
	TargetLocalID string
	Inlined       bool
}

// IndirectSite is one "indirect simple callsite …" record from .cgraph.
// FileLine and Column are attached by the ingest layer from the matching
// .ci edge (.cgraph does not carry them).
type IndirectSite struct {
	// SpeculativeCount is the "num speculative call targets: N" value.
	// Zero on our fixture — this is normal for pure C.
	SpeculativeCount int
	// SpeculativeTargets lists any resolved candidates (parsed from
	// follow-up lines when SpeculativeCount > 0).
	SpeculativeTargets []string
}

// Cgraph is the top-level result of parsing one .cgraph dump.
type Cgraph struct {
	// TrivialNeeded lists the local IDs of entry-point symbols (name of
	// main, exported vars, etc.). Not currently used but preserved for
	// future reachability work.
	TrivialNeeded []string
	// Symbols is the union of every symbol-table section, keyed by
	// LocalID and populated with the latest observation. Iteration order
	// is unspecified.
	Symbols map[string]*Function
}

// CI is the top-level result of parsing one .ci dump.
type CI struct {
	// Title is the outer "title:" — typically the compiled source path
	// relative to the compile CWD (usually the builddir).
	Title string
	// Nodes carries name → node info. External-reference nodes have
	// IsExternal=true and no StackBytes.
	Nodes map[string]CINode
	// Edges lists every direct/indirect call recorded by GCC.
	Edges []CIEdge
}

// CINode is one graph node from a .ci dump.
type CINode struct {
	Name          string
	DeclFile      string // path from label; may be relative to compile CWD
	DeclLine      int
	DeclColumn    int
	StackBytes    int  // 0 if not reported (external reference)
	StackKind     string // "static" | "dynamic" | "" when absent
	DynamicObjs   int  // -fcallgraph-info=da counter
	IsExternal    bool // true when the node uses `shape : ellipse`
	IsIndirectPlaceholder bool // true for the synthetic "__indirect_call" node
}

// CIEdge is one edge from a .ci dump.
type CIEdge struct {
	Source     string
	Target     string
	SiteFile   string // path from label
	SiteLine   int
	SiteColumn int
	// Indirect is true when Target is the synthetic "__indirect_call"
	// placeholder — one edge per indirect call site in the source.
	Indirect bool
}

// InlineSummary is one function's IPA inline summary block.
type InlineSummary struct {
	FunctionLocalID string
	Name            string
	LinkageName     string
	Inlinable       bool
	// Decisions the pass logged for this function's outgoing calls,
	// where "inlined into <caller>" tags appear.
	// Populated as a supplement to .cgraph's per-edge (inlined) tags.
	// Empty on our fixture.
	InlinedInto []string
}

// InlineDump is the top-level result of parsing a .inline file.
type InlineDump struct {
	Summaries []InlineSummary
}

// ICFGroup is one non-singular congruence class from a .icf dump.
type ICFGroup struct {
	// MemberNames lists the folded functions by name. Winning symbol is
	// determined by the linker, not by ICF, so the group is unordered.
	MemberNames []string
}

// ICFDump is the top-level result of parsing a .icf file. Members is
// empty when every class is singular (as on our fixture).
type ICFDump struct {
	Groups []ICFGroup
}

// DevirtHit is one speculative resolution recorded by the ipa-devirt pass.
type DevirtHit struct {
	CallerName string
	// TargetName is the callee GCC speculatively resolved to. In pure C
	// this rarely fires; parser tolerates empty.
	TargetName string
	Confidence string // "speculative" or "resolved"
}

// DevirtDump is the top-level result of parsing a .devirt file.
type DevirtDump struct {
	Hits []DevirtHit
}
