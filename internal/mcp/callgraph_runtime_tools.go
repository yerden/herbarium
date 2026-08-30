package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
)

// registerCallGraphRuntimeTools wires the post-optimization view:
// list_linked_callers, list_linked_callees (objdump-derived edges,
// per-target) and describe_inline_decisions (which callees were inlined
// into their caller).
func (s *Server) registerCallGraphRuntimeTools() {
	s.mcp.AddTool(newTool("list_linked_callers",
		mcp.WithDescription(
			"Callers per objdump of the shipped binary (runtime view; post-inlining, "+
				"post-optimization, per-target). Different answer than list_callers "+
				"when inlining is aggressive; this is ground truth for 'what actually "+
				"calls X at runtime'. Edges are resolved by branch-target address, so "+
				"same-named internal-linkage callers across TUs are disambiguated when "+
				"a linker map is present; without a map file the ingest falls back to "+
				"name-based lookup and may collapse such collisions.",
		),
		mcp.WithString("callee_usr", mcp.Required(),
			mcp.Description("USR of the callee (from find_symbol.hits[].usr or describe_symbol.usr).")),
		mcp.WithString("target", mcp.Required(),
			mcp.Description("Target binary name — runtime callgraph is per-target.")),
	), s.handleListLinkedCallers)

	s.mcp.AddTool(newTool("list_linked_callees",
		mcp.WithDescription(
			"Callees per objdump of the shipped binary (runtime view; post-inlining, "+
				"post-optimization, per-target). Different answer than list_callees "+
				"when inlining is aggressive; the set difference "+
				"list_callees − list_linked_callees is the definitive 'what got "+
				"inlined or DCE'd into this caller for this target'. Edges are resolved "+
				"by branch-target address, so same-named internal-linkage callees across "+
				"TUs are disambiguated when a linker map is present; without a map file "+
				"the ingest falls back to name-based lookup and may collapse such "+
				"collisions.",
		),
		mcp.WithString("caller_usr", mcp.Required(),
			mcp.Description("USR of the caller (from find_symbol.hits[].usr or describe_symbol.usr).")),
		mcp.WithString("target", mcp.Required(),
			mcp.Description("Target binary name — runtime callgraph is per-target.")),
	), s.handleListLinkedCallees)

	s.mcp.AddTool(newTool("describe_inline_decisions",
		mcp.WithDescription(
			"IPA-inline decisions GCC recorded for a caller (from the .cgraph "+
				"dump's `(inlined)` tag). NOT ground truth for what's inlined in the "+
				"final binary: misses early tree-level inlines (always_inline, trivial "+
				"static inlines) and post-IPA folds (RTL inliner, constprop clones, "+
				"DCE'd calls), all of which show `inlined=0` here despite leaving no "+
				"call in the binary. For the definitive source-vs-runtime diff for a "+
				"given target, subtract list_linked_callees from list_callees; use "+
				"this tool only as corroborating detail on why IPA made its call.",
		),
		mcp.WithString("caller_usr", mcp.Required(),
			mcp.Description("USR of the caller (from find_symbol.hits[].usr or describe_symbol.usr).")),
	), s.handleDescribeInlineDecisions)
}

// -- list_linked_callers ---------------------------------------------

// LinkedCallerEdge is one row of list_linked_callers.
type LinkedCallerEdge struct {
	Caller SymbolRef `json:"caller"`
}

// ListLinkedCallersResponse is what list_linked_callers returns.
type ListLinkedCallersResponse struct {
	CalleeUSR string             `json:"callee_usr"`
	Target    string             `json:"target"`
	Callers   []LinkedCallerEdge `json:"callers"`
	Total     int                `json:"total"`
}

func (s *Server) handleListLinkedCallers(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	usr, err := req.RequireString("callee_usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	target, err := req.RequireString("target")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	calleeID, err := s.symbolIDByUSR(usr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	targetID, err := s.targetIDByName(target)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	rows, err := s.db.Query(`
		SELECT DISTINCT c.usr, c.name, c.kind, IFNULL(c.signature, '')
		FROM call_edges e
		JOIN symbols c ON c.id = e.caller_id
		WHERE e.callee_id = ? AND e.source = 'objdump' AND e.target_id = ?
		ORDER BY c.name`, calleeID, targetID)
	if err != nil {
		return mcp.NewToolResultError("list_linked_callers: " + err.Error()), nil
	}
	defer rows.Close()
	var out []LinkedCallerEdge
	for rows.Next() {
		var r SymbolRef
		if err := rows.Scan(&r.USR, &r.Name, &r.Kind, &r.Signature); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		out = append(out, LinkedCallerEdge{Caller: r})
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}
	return jsonResult(ListLinkedCallersResponse{
		CalleeUSR: usr,
		Target:    target,
		Callers:   out,
		Total:     len(out),
	})
}

// -- list_linked_callees ---------------------------------------------

// LinkedCalleeEdge is one row of list_linked_callees.
type LinkedCalleeEdge struct {
	Callee SymbolRef `json:"callee"`
}

// ListLinkedCalleesResponse is what list_linked_callees returns.
type ListLinkedCalleesResponse struct {
	CallerUSR string             `json:"caller_usr"`
	Target    string             `json:"target"`
	Callees   []LinkedCalleeEdge `json:"callees"`
	Total     int                `json:"total"`
}

func (s *Server) handleListLinkedCallees(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	usr, err := req.RequireString("caller_usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	target, err := req.RequireString("target")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	callerID, err := s.symbolIDByUSR(usr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	targetID, err := s.targetIDByName(target)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	rows, err := s.db.Query(`
		SELECT DISTINCT c.usr, c.name, c.kind, IFNULL(c.signature, '')
		FROM call_edges e
		JOIN symbols c ON c.id = e.callee_id
		WHERE e.caller_id = ? AND e.source = 'objdump' AND e.target_id = ?
		ORDER BY c.name`, callerID, targetID)
	if err != nil {
		return mcp.NewToolResultError("list_linked_callees: " + err.Error()), nil
	}
	defer rows.Close()
	var out []LinkedCalleeEdge
	for rows.Next() {
		var r SymbolRef
		if err := rows.Scan(&r.USR, &r.Name, &r.Kind, &r.Signature); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		out = append(out, LinkedCalleeEdge{Callee: r})
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}
	return jsonResult(ListLinkedCalleesResponse{
		CallerUSR: usr,
		Target:    target,
		Callees:   out,
		Total:     len(out),
	})
}

// -- describe_inline_decisions ---------------------------------------

// InlineDecision is one row of describe_inline_decisions.
type InlineDecision struct {
	Callee  SymbolRef `json:"callee"`
	Inlined bool      `json:"inlined"`
}

// DescribeInlineDecisionsResponse is what describe_inline_decisions
// returns.
type DescribeInlineDecisionsResponse struct {
	CallerUSR string           `json:"caller_usr"`
	Decisions []InlineDecision `json:"decisions"`
	Total     int              `json:"total"`
}

func (s *Server) handleDescribeInlineDecisions(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	usr, err := req.RequireString("caller_usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	callerID, err := s.symbolIDByUSR(usr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	rows, err := s.db.Query(`
		SELECT c.usr, c.name, c.kind, IFNULL(c.signature, ''), d.inlined
		FROM inline_decisions d
		JOIN symbols c ON c.id = d.callee_id
		WHERE d.caller_id = ?
		ORDER BY c.name`, callerID)
	if err != nil {
		return mcp.NewToolResultError("describe_inline_decisions: " + err.Error()), nil
	}
	defer rows.Close()
	var out []InlineDecision
	for rows.Next() {
		var d InlineDecision
		var inlined int
		if err := rows.Scan(&d.Callee.USR, &d.Callee.Name, &d.Callee.Kind, &d.Callee.Signature, &inlined); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		d.Inlined = inlined == 1
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}
	return jsonResult(DescribeInlineDecisionsResponse{
		CallerUSR: usr,
		Decisions: out,
		Total:     len(out),
	})
}
