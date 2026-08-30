package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

const indirectSitesLimit = 500

// registerIndirectTools wires the indirect-call tools.
//
// callee_type is rendered in the same form as symbols.signature
// ("int (int, int)" — the pointee of the function pointer, not the
// pointer type), which is what lets resolve_indirect_call narrow by an
// equality join instead of a string transform.
func (s *Server) registerIndirectTools() {
	s.mcp.AddTool(newTool("list_indirect_call_sites",
		mcp.WithDescription(
			"Sites GCC recorded as indirect calls, with source location, the "+
				"callee's signature, and a hint naming what the call dispatches "+
				"through ('ops.add' for a struct member, a bare name for a global "+
				"fn-pointer or a fn-pointer parameter). Both are recovered from "+
				"DWARF and are empty when the compiler kept no trace of the target. "+
				"Filterable by caller USR, callee_type, or target. Feed a site_id "+
				"into resolve_indirect_call to get a candidate callee list combining "+
				"devirt hints and type-compatible address-taken functions.",
		),
		mcp.WithString("caller_usr",
			mcp.Description("USR of the enclosing function (from find_symbol.hits[].usr or describe_symbol.usr). Omit for all sites.")),
		mcp.WithString("callee_type",
			mcp.Description("Filter by callee signature, e.g. 'int (int, int)'. Same form as describe_symbol.signature, so a value from either tool matches here.")),
		mcp.WithString("target",
			mcp.Description("Restrict to sites whose caller is reachable in this target.")),
	), s.handleListIndirectCallSites)

	s.mcp.AddTool(newTool("list_address_taken_functions",
		mcp.WithDescription(
			"Functions whose address is taken somewhere (candidate targets for "+
				"indirect calls). Filterable by canonical fn-pointer type (matched "+
				"against the function's DWARF signature) or by target membership.",
		),
		mcp.WithString("fn_ptr_type",
			mcp.Description("Filter by function signature (as returned by describe_symbol.signature).")),
		mcp.WithString("target",
			mcp.Description("Restrict to functions reachable in this target.")),
	), s.handleListAddressTakenFunctions)

	s.mcp.AddTool(newTool("resolve_indirect_call",
		mcp.WithDescription(
			"Best-effort candidate list for one indirect callsite. Combines: "+
				"(a) GCC devirtualization hints, (b) address-taken functions whose "+
				"signature matches the site's callee_type. When DWARF left no "+
				"callee_type for the site, falls back to every address-taken "+
				"function — much broader. Each candidate is tagged with its "+
				"evidence source, so the fallback is distinguishable ('address_taken' "+
				"vs 'type_match').",
		),
		mcp.WithNumber("site_id", mcp.Required(),
			mcp.Description("indirect_call_sites.id — from list_indirect_call_sites.")),
	), s.handleResolveIndirectCall)

	s.mcp.AddTool(newTool("list_devirt_hints",
		mcp.WithDescription(
			"Everywhere GCC's speculative-devirtualization pass resolved an indirect "+
				"call to a specific target. High-confidence signals the agent can trust "+
				"without heuristics.",
		),
		mcp.WithString("target",
			mcp.Description("Restrict to hints whose caller is reachable in this target.")),
	), s.handleListDevirtHints)
}

// -- list_indirect_call_sites ----------------------------------------

// IndirectCallSite is one row of list_indirect_call_sites.
type IndirectCallSite struct {
	SiteID     int64     `json:"site_id"`
	Caller     SymbolRef `json:"caller"`
	Location   Location  `json:"location"`
	CalleeType string    `json:"callee_type,omitempty"`
	FieldHint  string    `json:"field_hint,omitempty"`
}

// ListIndirectCallSitesResponse is what list_indirect_call_sites returns.
type ListIndirectCallSitesResponse struct {
	Sites     []IndirectCallSite `json:"sites"`
	Total     int                `json:"total"`
	Truncated bool               `json:"truncated"`
}

func (s *Server) handleListIndirectCallSites(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	callerUSR := req.GetString("caller_usr", "")
	calleeType := req.GetString("callee_type", "")
	target := req.GetString("target", "")

	sqlText := `
		SELECT ics.id, s.usr, s.name, s.kind, IFNULL(s.signature, ''),
		       IFNULL(ics.file, ''), IFNULL(ics.line, 0), IFNULL(ics.column, 0),
		       IFNULL(ics.callee_type, ''), IFNULL(ics.field_hint, '')
		FROM indirect_call_sites ics
		JOIN symbols s ON s.id = ics.caller_id
		WHERE 1=1`
	var args []any
	if callerUSR != "" {
		sqlText += ` AND s.usr = ?`
		args = append(args, callerUSR)
	}
	if calleeType != "" {
		sqlText += ` AND ics.callee_type = ?`
		args = append(args, calleeType)
	}
	if target != "" {
		sqlText += ` AND EXISTS (
			SELECT 1 FROM symbol_reachability r
			JOIN targets t ON t.id = r.target_id
			WHERE r.symbol_id = s.id AND r.reachable = 1 AND t.name = ?
		)`
		args = append(args, target)
	}
	sqlText += ` ORDER BY ics.file, ics.line, ics.column LIMIT ?`
	args = append(args, indirectSitesLimit+1)

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return mcp.NewToolResultError("list_indirect_call_sites: " + err.Error()), nil
	}
	defer rows.Close()
	var out []IndirectCallSite
	for rows.Next() {
		var site IndirectCallSite
		var file string
		var line, col int
		if err := rows.Scan(
			&site.SiteID,
			&site.Caller.USR, &site.Caller.Name, &site.Caller.Kind, &site.Caller.Signature,
			&file, &line, &col,
			&site.CalleeType, &site.FieldHint,
		); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		site.Location = Location{Path: file, Line: line, Column: col}
		s.enrichLocation(&site.Location, true)
		out = append(out, site)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}
	truncated := len(out) > indirectSitesLimit
	if truncated {
		out = out[:indirectSitesLimit]
	}
	return jsonResult(ListIndirectCallSitesResponse{
		Sites:     out,
		Total:     len(out),
		Truncated: truncated,
	})
}

// -- list_address_taken_functions ------------------------------------

// AddressTakenFunction is one row of list_address_taken_functions.
type AddressTakenFunction struct {
	Symbol  SymbolRef `json:"symbol"`
	Targets []string  `json:"targets,omitempty"`
}

// ListAddressTakenFunctionsResponse is what list_address_taken_functions returns.
type ListAddressTakenFunctionsResponse struct {
	Functions []AddressTakenFunction `json:"functions"`
	Total     int                    `json:"total"`
}

func (s *Server) handleListAddressTakenFunctions(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fnPtrType := req.GetString("fn_ptr_type", "")
	target := req.GetString("target", "")

	sqlText := `
		SELECT s.usr, s.name, s.kind, IFNULL(s.signature, '')
		FROM symbols s
		WHERE s.kind = 'function' AND s.address_taken = 1`
	var args []any
	if fnPtrType != "" {
		sqlText += ` AND s.signature = ?`
		args = append(args, fnPtrType)
	}
	if target != "" {
		sqlText += ` AND EXISTS (
			SELECT 1 FROM symbol_reachability r
			JOIN targets t ON t.id = r.target_id
			WHERE r.symbol_id = s.id AND r.reachable = 1 AND t.name = ?
		)`
		args = append(args, target)
	}
	sqlText += ` ORDER BY s.name`

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return mcp.NewToolResultError("list_address_taken_functions: " + err.Error()), nil
	}
	defer rows.Close()
	var refs []SymbolRef
	for rows.Next() {
		var r SymbolRef
		if err := rows.Scan(&r.USR, &r.Name, &r.Kind, &r.Signature); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		refs = append(refs, r)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}
	byUSR, err := s.targetsByReachability(usrsOf(refs))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	out := make([]AddressTakenFunction, 0, len(refs))
	for _, r := range refs {
		out = append(out, AddressTakenFunction{Symbol: r, Targets: byUSR[r.USR]})
	}
	return jsonResult(ListAddressTakenFunctionsResponse{Functions: out, Total: len(out)})
}

// -- resolve_indirect_call -------------------------------------------

// ResolveCandidate is one candidate in resolve_indirect_call.
type ResolveCandidate struct {
	Symbol     SymbolRef `json:"symbol"`
	Evidence   string    `json:"evidence"`   // 'devirt' | 'type_match' | 'address_taken'
	Confidence string    `json:"confidence"` // 'resolved' | 'speculative' | 'candidate'
}

// ResolveIndirectCallResponse is what resolve_indirect_call returns.
type ResolveIndirectCallResponse struct {
	SiteID     int64              `json:"site_id"`
	Caller     SymbolRef          `json:"caller"`
	Location   Location           `json:"location"`
	CalleeType string             `json:"callee_type,omitempty"`
	FieldHint  string             `json:"field_hint,omitempty"`
	Candidates []ResolveCandidate `json:"candidates"`
	Total      int                `json:"total"`
}

func (s *Server) handleResolveIndirectCall(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	siteID, err := req.RequireInt("site_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var callerID int64
	var caller SymbolRef
	var file string
	var line, col int
	var calleeType, fieldHint string
	if err := s.db.QueryRow(`
		SELECT ics.caller_id, s.usr, s.name, s.kind, IFNULL(s.signature, ''),
		       IFNULL(ics.file, ''), IFNULL(ics.line, 0), IFNULL(ics.column, 0),
		       IFNULL(ics.callee_type, ''), IFNULL(ics.field_hint, '')
		FROM indirect_call_sites ics
		JOIN symbols s ON s.id = ics.caller_id
		WHERE ics.id = ?`, siteID,
	).Scan(&callerID, &caller.USR, &caller.Name, &caller.Kind, &caller.Signature,
		&file, &line, &col, &calleeType, &fieldHint); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return mcp.NewToolResultError(fmt.Sprintf("unknown indirect call site id %d", siteID)), nil
		}
		return mcp.NewToolResultError("resolve_indirect_call lookup: " + err.Error()), nil
	}
	loc := Location{Path: file, Line: line, Column: col}
	s.enrichLocation(&loc, true)

	resp := ResolveIndirectCallResponse{
		SiteID:     int64(siteID),
		Caller:     caller,
		Location:   loc,
		CalleeType: calleeType,
		FieldHint:  fieldHint,
	}

	seen := map[string]int{} // usr → index into resp.Candidates
	upsert := func(sym SymbolRef, evidence, confidence string) {
		if i, ok := seen[sym.USR]; ok {
			// A stronger evidence wins over a weaker one for the same
			// symbol; the ordering below is authoritative.
			if evidenceRank(evidence) > evidenceRank(resp.Candidates[i].Evidence) {
				resp.Candidates[i].Evidence = evidence
				resp.Candidates[i].Confidence = confidence
			}
			return
		}
		seen[sym.USR] = len(resp.Candidates)
		resp.Candidates = append(resp.Candidates, ResolveCandidate{Symbol: sym, Evidence: evidence, Confidence: confidence})
	}

	// (1) GCC's devirt hints — the strongest signal.
	devRows, err := s.db.Query(`
		SELECT s.usr, s.name, s.kind, IFNULL(s.signature, ''), d.confidence
		FROM devirt_hints d
		JOIN symbols s ON s.id = d.callee_id
		WHERE d.site_id = ?`, siteID)
	if err != nil {
		return mcp.NewToolResultError("devirt hints: " + err.Error()), nil
	}
	for devRows.Next() {
		var sym SymbolRef
		var confidence string
		if err := devRows.Scan(&sym.USR, &sym.Name, &sym.Kind, &sym.Signature, &confidence); err != nil {
			devRows.Close()
			return mcp.NewToolResultError("scan devirt: " + err.Error()), nil
		}
		if confidence == "" {
			confidence = "speculative"
		}
		upsert(sym, "devirt", confidence)
	}
	if err := devRows.Err(); err != nil {
		devRows.Close()
		return mcp.NewToolResultError("devirt iterate: " + err.Error()), nil
	}
	devRows.Close()

	// (2) Type-compatibility narrowing. Only useful when callee_type is
	// populated. Match against symbols.signature (address_taken=1).
	if calleeType != "" {
		typRows, err := s.db.Query(`
			SELECT s.usr, s.name, s.kind, IFNULL(s.signature, '')
			FROM symbols s
			WHERE s.kind = 'function' AND s.address_taken = 1 AND s.signature = ?`,
			calleeType)
		if err != nil {
			return mcp.NewToolResultError("type match: " + err.Error()), nil
		}
		for typRows.Next() {
			var sym SymbolRef
			if err := typRows.Scan(&sym.USR, &sym.Name, &sym.Kind, &sym.Signature); err != nil {
				typRows.Close()
				return mcp.NewToolResultError("scan type match: " + err.Error()), nil
			}
			upsert(sym, "type_match", "candidate")
		}
		if err := typRows.Err(); err != nil {
			typRows.Close()
			return mcp.NewToolResultError("type match iterate: " + err.Error()), nil
		}
		typRows.Close()
	} else {
		// No type info → fall back to every address-taken function.
		// Broad but honest: with an empty callee_type there is no
		// narrowing signal to apply, so the agent gets the full
		// candidate pool tagged 'address_taken'.
		anyRows, err := s.db.Query(`
			SELECT s.usr, s.name, s.kind, IFNULL(s.signature, '')
			FROM symbols s
			WHERE s.kind = 'function' AND s.address_taken = 1`)
		if err != nil {
			return mcp.NewToolResultError("address-taken fallback: " + err.Error()), nil
		}
		for anyRows.Next() {
			var sym SymbolRef
			if err := anyRows.Scan(&sym.USR, &sym.Name, &sym.Kind, &sym.Signature); err != nil {
				anyRows.Close()
				return mcp.NewToolResultError("scan address-taken: " + err.Error()), nil
			}
			upsert(sym, "address_taken", "candidate")
		}
		if err := anyRows.Err(); err != nil {
			anyRows.Close()
			return mcp.NewToolResultError("address-taken iterate: " + err.Error()), nil
		}
		anyRows.Close()
	}

	resp.Total = len(resp.Candidates)
	return jsonResult(resp)
}

func evidenceRank(e string) int {
	switch e {
	case "devirt":
		return 3
	case "type_match":
		return 2
	case "address_taken":
		return 1
	}
	return 0
}

// -- list_devirt_hints -----------------------------------------------

// DevirtHint is one row of list_devirt_hints.
type DevirtHint struct {
	SiteID     int64     `json:"site_id"`
	Caller     SymbolRef `json:"caller"`
	Callee     SymbolRef `json:"callee"`
	Location   Location  `json:"location"`
	Confidence string    `json:"confidence"`
}

// ListDevirtHintsResponse is what list_devirt_hints returns.
type ListDevirtHintsResponse struct {
	Hints []DevirtHint `json:"hints"`
	Total int          `json:"total"`
}

func (s *Server) handleListDevirtHints(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target := req.GetString("target", "")

	sqlText := `
		SELECT d.site_id,
		       caller.usr, caller.name, caller.kind, IFNULL(caller.signature, ''),
		       callee.usr, callee.name, callee.kind, IFNULL(callee.signature, ''),
		       IFNULL(ics.file, ''), IFNULL(ics.line, 0), IFNULL(ics.column, 0),
		       d.confidence
		FROM devirt_hints d
		JOIN indirect_call_sites ics ON ics.id = d.site_id
		JOIN symbols caller ON caller.id = ics.caller_id
		JOIN symbols callee ON callee.id = d.callee_id
		WHERE 1=1`
	var args []any
	if target != "" {
		sqlText += ` AND EXISTS (
			SELECT 1 FROM symbol_reachability r
			JOIN targets t ON t.id = r.target_id
			WHERE r.symbol_id = caller.id AND r.reachable = 1 AND t.name = ?
		)`
		args = append(args, target)
	}
	sqlText += ` ORDER BY ics.file, ics.line, ics.column`

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return mcp.NewToolResultError("list_devirt_hints: " + err.Error()), nil
	}
	defer rows.Close()
	var out []DevirtHint
	for rows.Next() {
		var h DevirtHint
		var file string
		var line, col int
		if err := rows.Scan(
			&h.SiteID,
			&h.Caller.USR, &h.Caller.Name, &h.Caller.Kind, &h.Caller.Signature,
			&h.Callee.USR, &h.Callee.Name, &h.Callee.Kind, &h.Callee.Signature,
			&file, &line, &col, &h.Confidence,
		); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		h.Location = Location{Path: file, Line: line, Column: col}
		s.enrichLocation(&h.Location, false)
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}
	return jsonResult(ListDevirtHintsResponse{Hints: out, Total: len(out)})
}
