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

// Info is the top-level result of reading one .o's DWARF.
type Info struct {
	ObjectPath string
	CompDir    string   // DW_AT_comp_dir on the CU
	CUFile     string   // DW_AT_name on the CU — the .c source being compiled
	Subprograms []Subprogram
	CallSites   []CallSite
	Structs     []StructInfo
	Typedefs    []TypedefInfo
	Variables   []VariableInfo
}

// Subprogram is one DW_TAG_subprogram DIE. A given DWARF may carry both
// a declaration (Declaration=true) and a definition (Definition=true)
// for the same name; the ingest layer picks the def to enrich symbol
// rows.
type Subprogram struct {
	Name       string
	LinkageName string // DW_AT_linkage_name when present (usually the same as Name in C)
	DeclFile   string // resolved via the CU's file table
	DeclLine   int
	DeclColumn int
	Signature  string  // reconstructed: "returnType (param1, param2, ...)"
	Definition bool    // has DW_AT_low_pc — a real def
	Declaration bool   // DW_AT_declaration=1 — a decl only
	External   bool    // DW_AT_external=1
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
	File     string
	Line     int
	Column   int
	Indirect bool   // no DW_AT_call_origin — GCC couldn't resolve statically
	CalleeName string // when DW_AT_call_origin resolves to a named DIE
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
