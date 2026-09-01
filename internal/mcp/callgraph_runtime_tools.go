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
			"Everything that happened to the calls in one function, across three "+
				"planes. `records` is GCC's own optimization record: every pass's "+
				"decision, including the early inliner (pass 'einline') that folds "+
				"always_inline and trivial callees before any IPA pass runs, and "+
				"including the rejections with the compiler's reason for each. "+
				"`instances` is DWARF: the inlined bodies that actually survived "+
				"into the object's code — outcome rather than decision, so a callee "+
				"that folded to a constant afterwards has a record and no instance. "+
				"`decisions` is the older per-edge view from the .cgraph `(inlined)` "+
				"tag, IPA-stage only. For the post-link runtime answer for one "+
				"target, subtract list_linked_callees from list_callees.",
		),
		mcp.WithString("caller_usr", mcp.Required(),
			mcp.Description("USR of the caller (from find_symbol.hits[].usr or describe_symbol.usr).")),
	), s.handleDescribeInlineDecisions)

	s.mcp.AddTool(newTool("list_inline_sites",
		mcp.WithDescription(
			"The reverse of describe_inline_decisions: everywhere one function's "+
				"body was inlined INTO another. Answers 'where did this helper go?' "+
				"for a symbol that has no runtime callers because it was folded "+
				"away. `instances` are the folds whose code survived into an object "+
				"(DWARF); `records` are the decisions GCC logged, rejections "+
				"included. A function with instances but no linked callers was "+
				"inlined everywhere, not dead.",
		),
		mcp.WithString("callee_usr", mcp.Required(),
			mcp.Description("USR of the function that may have been inlined (from find_symbol.hits[].usr).")),
	), s.handleListInlineSites)
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

// InlineDecision is one row of describe_inline_decisions.decisions —
// the .cgraph per-edge view.
type InlineDecision struct {
	Callee  SymbolRef `json:"callee"`
	Inlined bool      `json:"inlined"`
}

// InlineRecordRow is one decision from GCC's optimization record.
type InlineRecordRow struct {
	Caller SymbolRef `json:"caller"`
	Callee SymbolRef `json:"callee"`
	// Pass is 'einline' for the early inliner (pre-IPA, handles
	// always_inline) or 'inline' for the IPA inliner. The same call can
	// appear under both — two passes looked at it.
	Pass     string   `json:"pass"`
	Inlined  bool     `json:"inlined"`
	Reason   string   `json:"reason,omitempty"` // GCC's words, when not inlined
	Location Location `json:"location"`
}

// InlineInstanceRow is one inlined body present in an object's code.
type InlineInstanceRow struct {
	Caller SymbolRef `json:"caller"`
	Callee SymbolRef `json:"callee"`
	// Depth is 1 when the body sits directly in caller; deeper rows name
	// the inlined body they landed inside in ParentCallee.
	Depth        int        `json:"depth"`
	ParentCallee *SymbolRef `json:"parent_callee,omitempty"`
	Location     Location   `json:"location"`
	Object       string     `json:"object"`
}

// DescribeInlineDecisionsResponse is what describe_inline_decisions
// returns. The three planes disagree by design — see the tool
// description for which one answers which question.
type DescribeInlineDecisionsResponse struct {
	CallerUSR string              `json:"caller_usr"`
	Decisions []InlineDecision    `json:"decisions"`
	Records   []InlineRecordRow   `json:"records"`
	Instances []InlineInstanceRow `json:"instances"`
}

// ListInlineSitesResponse is what list_inline_sites returns.
type ListInlineSitesResponse struct {
	CalleeUSR string              `json:"callee_usr"`
	Instances []InlineInstanceRow `json:"instances"`
	Records   []InlineRecordRow   `json:"records"`
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
	records, err := s.inlineRecords("r.caller_id = ?", callerID)
	if err != nil {
		return mcp.NewToolResultError("describe_inline_decisions: " + err.Error()), nil
	}
	instances, err := s.inlineInstances("i.caller_id = ?", callerID)
	if err != nil {
		return mcp.NewToolResultError("describe_inline_decisions: " + err.Error()), nil
	}
	return jsonResult(DescribeInlineDecisionsResponse{
		CallerUSR: usr,
		Decisions: out,
		Records:   records,
		Instances: instances,
	})
}

// -- list_inline_sites -----------------------------------------------

func (s *Server) handleListInlineSites(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	usr, err := req.RequireString("callee_usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	calleeID, err := s.symbolIDByUSR(usr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	instances, err := s.inlineInstances("i.callee_id = ?", calleeID)
	if err != nil {
		return mcp.NewToolResultError("list_inline_sites: " + err.Error()), nil
	}
	records, err := s.inlineRecords("r.callee_id = ?", calleeID)
	if err != nil {
		return mcp.NewToolResultError("list_inline_sites: " + err.Error()), nil
	}
	return jsonResult(ListInlineSitesResponse{
		CalleeUSR: usr,
		Instances: instances,
		Records:   records,
	})
}

// inlineRecords reads inline_records under a caller_id or callee_id
// predicate. Both tools want the same columns, so the WHERE clause is
// the only thing that varies.
func (s *Server) inlineRecords(where string, arg any) ([]InlineRecordRow, error) {
	rows, err := s.db.Query(`
		SELECT cr.usr, cr.name, cr.kind, IFNULL(cr.signature, ''),
		       ce.usr, ce.name, ce.kind, IFNULL(ce.signature, ''),
		       r.pass, r.inlined, IFNULL(r.reason, ''),
		       IFNULL(r.file, ''), IFNULL(r.line, 0), IFNULL(r.column, 0)
		FROM inline_records r
		JOIN symbols cr ON cr.id = r.caller_id
		JOIN symbols ce ON ce.id = r.callee_id
		WHERE `+where+`
		ORDER BY r.pass, ce.name, r.file, r.line`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InlineRecordRow
	for rows.Next() {
		var rec InlineRecordRow
		var inlined int
		var file string
		var line, col int
		if err := rows.Scan(
			&rec.Caller.USR, &rec.Caller.Name, &rec.Caller.Kind, &rec.Caller.Signature,
			&rec.Callee.USR, &rec.Callee.Name, &rec.Callee.Kind, &rec.Callee.Signature,
			&rec.Pass, &inlined, &rec.Reason, &file, &line, &col,
		); err != nil {
			return nil, err
		}
		rec.Inlined = inlined == 1
		rec.Location = Location{Path: file, Line: line, Column: col}
		s.enrichLocation(&rec.Location, true)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// inlineInstances reads inline_instances under a caller_id or callee_id
// predicate.
func (s *Server) inlineInstances(where string, arg any) ([]InlineInstanceRow, error) {
	rows, err := s.db.Query(`
		SELECT cr.usr, cr.name, cr.kind, IFNULL(cr.signature, ''),
		       ce.usr, ce.name, ce.kind, IFNULL(ce.signature, ''),
		       i.depth, IFNULL(p.usr, ''), IFNULL(p.name, ''), IFNULL(p.kind, ''),
		       IFNULL(i.file, ''), IFNULL(i.line, 0), IFNULL(i.column, 0),
		       IFNULL(i.object, '')
		FROM inline_instances i
		JOIN symbols cr ON cr.id = i.caller_id
		JOIN symbols ce ON ce.id = i.callee_id
		LEFT JOIN symbols p ON p.id = i.parent_callee_id
		WHERE `+where+`
		ORDER BY i.depth, ce.name, i.file, i.line`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InlineInstanceRow
	for rows.Next() {
		var inst InlineInstanceRow
		var parent SymbolRef
		var file string
		var line, col int
		if err := rows.Scan(
			&inst.Caller.USR, &inst.Caller.Name, &inst.Caller.Kind, &inst.Caller.Signature,
			&inst.Callee.USR, &inst.Callee.Name, &inst.Callee.Kind, &inst.Callee.Signature,
			&inst.Depth, &parent.USR, &parent.Name, &parent.Kind,
			&file, &line, &col, &inst.Object,
		); err != nil {
			return nil, err
		}
		if parent.USR != "" {
			inst.ParentCallee = &parent
		}
		inst.Location = Location{Path: file, Line: line, Column: col}
		s.enrichLocation(&inst.Location, true)
		out = append(out, inst)
	}
	return out, rows.Err()
}
