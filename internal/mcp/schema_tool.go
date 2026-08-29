package mcp

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/yerden/herbarium/internal/store"
)

// registerSchemaTool wires describe_schema. The response gives an agent
// everything it needs to synthesize a sql_query call: the DDL, the
// enum-value glossary, and a small set of join recipes that document
// how the tables relate (nothing surprising if you read the schema
// carefully, but LLMs miss the subtleties otherwise).
func (s *Server) registerSchemaTool() {
	tool := newTool("describe_schema",
		mcp.WithDescription(
			"Return the full read-only .hbr schema (CREATE TABLE DDL) plus "+
				"enum-value glossary and canonical join recipes. Pair with "+
				"sql_query for arbitrary queries the tool set doesn't cover.",
		),
		mcp.WithIdempotentHintAnnotation(true),
	)
	s.mcp.AddTool(tool, s.handleDescribeSchema)
}

// SchemaResponse is what describe_schema returns. Emitted both as
// structured content (for agents that consume JSON) and as a text
// rendering (for humans reading the raw MCP transcript).
type SchemaResponse struct {
	SchemaVersion string       `json:"schema_version"`
	DDL           string       `json:"ddl"`
	Enums         []SchemaEnum `json:"enums"`
	JoinRecipes   []SchemaJoin `json:"join_recipes"`
}

// SchemaEnum is one closed-vocabulary column documented in the response.
type SchemaEnum struct {
	Column string   `json:"column"` // "symbols.kind"
	Values []string `json:"values"`
	Notes  string   `json:"notes,omitempty"`
}

// SchemaJoin is one canonical cross-plane query.
type SchemaJoin struct {
	Purpose string `json:"purpose"`
	SQL     string `json:"sql"`
}

func (s *Server) handleDescribeSchema(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp := SchemaResponse{
		SchemaVersion: store.SchemaVersion,
		DDL:           store.Schema(),
		Enums:         SchemaEnums,
		JoinRecipes:   SchemaJoinRecipes,
	}
	body, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("marshal schema response: " + err.Error()), nil
	}
	result := mcp.NewToolResultText(string(body))
	result.StructuredContent = resp
	return result, nil
}

// SchemaEnums documents the closed vocabularies in the schema. Kept in
// code (not in the DDL) because SQLite has no native ENUM and the plan
// treats these vocabularies as part of the contract — changing them is
// a schema-breaking change (see § Database schema).
var SchemaEnums = []SchemaEnum{
	{
		Column: "targets.kind",
		Values: []string{"executable", "static_library", "shared_library"},
	},
	{
		Column: "symbols.kind",
		Values: []string{"function", "variable", "typedef"},
		Notes:  "Additional entity kinds (struct/union/enum tags, fields) may appear once a future walker phase populates them; see herbarium-plan.md Appendix.",
	},
	{
		Column: "symbols.linkage",
		Values: []string{"external", "internal", "weak", "common"},
		Notes:  "Aggregated across all TUs where the symbol is defined. Ranking: external > weak > common > internal.",
	},
	{
		Column: "call_edges.source",
		Values: []string{"compiler_cgraph", "objdump"},
		Notes:  "compiler_cgraph rows carry NULL target_id (source-view, target-agnostic). objdump rows are per-target and post-optimization.",
	},
	{
		Column: "devirt_hints.confidence",
		Values: []string{"speculative", "resolved"},
	},
	{
		Column: "link_resolutions.linkage_kind",
		Values: []string{"strong", "weak", "unique_global", "common"},
	},
	{
		Column: "symbol_reachability.reachable",
		Values: []string{"1"},
		Notes:  "symbol_reachability is a view over link_resolutions; it only emits reachable=1 rows. A symbol that was dead-stripped, dynamically resolved, or fully inlined away is absent from the view for that target — test with NOT EXISTS or a LEFT JOIN, not WHERE reachable = 0.",
	},
}

// SchemaJoinRecipes are the canonical cross-plane queries an agent will
// reach for. Each one anchors on USR — the only join key between planes
// (see herbarium-plan.md § Invariant).
var SchemaJoinRecipes = []SchemaJoin{
	{
		Purpose: "Resolve a name to a symbol row (identity), including link-time clones like foo.constprop.0",
		SQL: `SELECT s.*
FROM symbols s
WHERE s.name = :name
   OR json_each.value = :name
     AND s.rowid IN (
       SELECT rowid FROM symbols, json_each(symbols.linkage_names)
     )`,
	},
	{
		Purpose: "All defs of a symbol (multi-def: multi-executable main, weak+strong hook, static-inline header)",
		SQL: `SELECT sd.file, sd.line, sd.decl_file, sd.decl_line, sd.is_weak, sd.linkage_name
FROM symbol_definitions sd
JOIN symbols s ON sd.symbol_id = s.id
WHERE s.usr = :usr`,
	},
	{
		Purpose: "Source-view callers of a function (from GCC's cgraph, target-agnostic)",
		SQL: `SELECT caller.usr, caller.name
FROM call_edges e
JOIN symbols caller ON caller.id = e.caller_id
JOIN symbols callee ON callee.id = e.callee_id
WHERE callee.usr = :usr AND e.source = 'compiler_cgraph'`,
	},
	{
		Purpose: "Runtime-view callers per target (from objdump, post-inlining, post-optimization)",
		SQL: `SELECT caller.usr, caller.name, t.name AS target
FROM call_edges e
JOIN symbols caller ON caller.id = e.caller_id
JOIN symbols callee ON callee.id = e.callee_id
JOIN targets t ON t.id = e.target_id
WHERE callee.usr = :usr AND e.source = 'objdump'`,
	},
	{
		Purpose: "Fetch source content for a location returned by any tool",
		SQL: `SELECT s.path, s.blob_hash, b.size
FROM sources s JOIN blobs b ON b.hash = s.blob_hash
WHERE s.path = :path`,
	},
	{
		Purpose: "Reachability of a symbol per target (present row = reachable; absent row = dead-stripped, dynamically resolved, or fully inlined away)",
		SQL: `SELECT t.name AS target, r.reachable, r.section_kept
FROM symbol_reachability r
JOIN symbols s ON s.id = r.symbol_id
JOIN targets t ON t.id = r.target_id
WHERE s.usr = :usr`,
	},
	{
		Purpose: "Which object supplied a symbol in a given target (weak-strong resolution)",
		SQL: `SELECT lr.winning_object, lr.linkage_kind, lr.archive, lr.losing_objects
FROM link_resolutions lr
JOIN targets t ON t.id = lr.target_id
WHERE lr.usr = :usr AND t.name = :target`,
	},
	{
		Purpose: "Read the declaration site of a symbol from an external header " +
			"(only useful when collect was invoked with --include-external and the " +
			"DWARF decl_file falls under the packed globs; empty result otherwise)",
		SQL: `SELECT es.abs_path, es.blob_hash, sd.decl_line
FROM symbol_definitions sd
JOIN symbols s ON s.id = sd.symbol_id
JOIN external_sources es ON es.abs_path = sd.decl_file
WHERE s.usr = :usr`,
	},
	{
		Purpose: "Read a build-tree file (configure_file / custom_target output) via generated_sources. " +
			"Key is builddir-relative — the same value list_source_files returns for IsGenerated=true rows " +
			"whose Path does not start with '/'.",
		SQL: `SELECT gs.builddir_rel, gs.blob_hash, b.size
FROM generated_sources gs JOIN blobs b ON b.hash = gs.blob_hash
WHERE gs.builddir_rel = :builddir_rel`,
	},
}
