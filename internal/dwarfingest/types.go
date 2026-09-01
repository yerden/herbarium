// Package dwarfingest reads DWARF debug info from ELF objects and
// extracts source-level facts herbarium's compiler-side ingest cannot
// see: symbol signatures, decl file/line/column, struct/union field
// names, and — through DW_TAG_call_site — the file:line:column of every
// call site the compiler recorded (indirect included).
//
// Every entry in the result types below carries raw, unresolved strings.
// The ingest orchestrator (internal/ingest) is responsible for making
// paths project-relative and matching against the compiler-plane USRs
// so DWARF facts can UPSERT symbols and symbol_definitions rows.
package dwarfingest

import "debug/dwarf"

// Info is the top-level result of reading one .o's DWARF.
type Info struct {
	ObjectPath  string
	CompDir     string // DW_AT_comp_dir on the CU
	CUFile      string // DW_AT_name on the CU — the .c source being compiled
	Subprograms []Subprogram
	CallSites   []CallSite
	// InlineInstances is what the inliner actually left in this object's
	// code, whichever pass performed the fold.
	InlineInstances []InlineInstance
	Structs         []StructInfo
	Typedefs        []TypedefInfo
	Variables       []VariableInfo
}

// Subprogram is one DW_TAG_subprogram DIE. A given DWARF may carry both
// a declaration (Declaration=true) and a definition (Definition=true)
// for the same name; the ingest layer picks the def to enrich symbol
// rows.
type Subprogram struct {
	Name        string
	LinkageName string // DW_AT_linkage_name when present (usually the same as Name in C)
	DeclFile    string // resolved via the CU's file table
	DeclLine    int
	DeclColumn  int
	Signature   string // reconstructed: "returnType (param1, param2, ...)"
	Definition  bool   // has DW_AT_low_pc — a real def
	Declaration bool   // DW_AT_declaration=1 — a decl only
	External    bool   // DW_AT_external=1
	// AbstractInline marks the abstract instance root GCC emits for a
	// function it inlined everywhere. Its DeclFile/DeclLine are the only
	// record of where the body was written — for a static inline in a
	// header, GCC emits no .ci node at all, so the compiler plane never
	// saw that location. See ingest.DWARF.
	AbstractInline bool
}

// CallSite is one DW_TAG_call_site DIE. GCC emits these under the
// enclosing subprogram — including inlined subprograms, which is why
// the enclosing name may not be the source-view caller.
type CallSite struct {
	EnclosingName string // name of the DW_TAG_subprogram this site sits under
	// SourceCallerName is the source-view caller derived from DWARF's
	// inlined-subroutine chain. When a call comes from code inlined out
	// of function F into G, EnclosingName is G but SourceCallerName is F.
	SourceCallerName string
	File             string
	Line             int
	Column           int
	Indirect         bool   // no DW_AT_call_origin — GCC couldn't resolve statically
	CalleeName       string // when DW_AT_call_origin resolves to a named DIE

	// CalleeType is the callee's signature, rendered the same way
	// Subprogram.Signature is ("int (int, int)"), so the two join
	// directly. It is the pointee of the function-pointer the call
	// goes through, not the pointer type itself. Empty when neither
	// DW_AT_call_target nor the call instruction's relocation names
	// something typed.
	CalleeType string
	// FieldHint names what the call dispatches through: "ops.add" for
	// a struct member, "g_hook" for a plain global fn-ptr, or a
	// parameter name when DW_AT_call_target points at one.
	FieldHint string

	// Inputs for the second resolution phase (see calltarget.go);
	// unexported because they are meaningless once Read returns.
	returnPC     uint64
	callTarget   []byte       // raw DW_AT_call_target expression
	enclosingOff dwarf.Offset // DIE offset of the physically enclosing subprogram
}

// InlineInstance is one DW_TAG_inlined_subroutine DIE: a callee whose
// body the compiler copied into another function and whose code survived
// into this object. It is the only route to the early inliner's work —
// always_inline and trivial static callees are folded before any IPA
// pass runs, so no IPA dump mentions them — but it is an outcome, not a
// decision: a callee that folds away to nothing after inlining leaves no
// DIE at all.
type InlineInstance struct {
	// CalleeName comes from DW_AT_abstract_origin and is the source
	// function's name, not a clone's: a constprop clone inlined back
	// into its single caller points at the original subprogram DIE.
	CalleeName string
	// CallerName is the physical frame the body landed in — the
	// innermost enclosing DW_TAG_subprogram.
	CallerName string
	// ParentCalleeName is the inlined body immediately containing this
	// one, empty at Depth 1. GCC nests these when an inlined callee had
	// its own calls inlined.
	ParentCalleeName string
	Depth            int
	File             string // DW_AT_call_file, resolved via the CU file table
	Line             int    // DW_AT_call_line — where the call was written
	Column           int
}

// StructInfo is one DW_TAG_structure_type DIE. Anonymous structs get
// an empty Name — ingest disambiguates with the __anon_line_col USR.
type StructInfo struct {
	Name     string
	DeclFile string
	DeclLine int
	Fields   []FieldInfo
}

// FieldInfo is one DW_TAG_member DIE. Type is a rendered string
// (e.g., "int (*)(int, int)" for a fn-pointer field).
type FieldInfo struct {
	Name string
	Type string
}

// TypedefInfo is one DW_TAG_typedef DIE.
type TypedefInfo struct {
	Name     string
	DeclFile string
	DeclLine int
	Target   string // rendered underlying type
}

// VariableInfo is one CU-scope DW_TAG_variable DIE.
type VariableInfo struct {
	Name     string
	DeclFile string
	DeclLine int
	Type     string
}
