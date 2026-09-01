package mcp

import (
	"context"
	"database/sql"
	"sort"
	"strconv"

	"github.com/mark3labs/mcp-go/mcp"
)

const (
	// inlineRowDefaultLimit caps each row array. It is deliberately small:
	// a row costs ~400 bytes without a snippet and ~700 with one, three
	// arrays ship in one response, and an aggressively-inlined caller can
	// have thousands of rows — enough to blow an MCP client's output limit
	// and get the whole payload truncated by the harness. The summary
	// block carries exact totals regardless of this cap, so the compact
	// answer stays complete even when the rows are a sample.
	inlineRowDefaultLimit = 50
	inlineRowMaxLimit     = 1000
	// verdictScanLimit bounds the rows explain_call reads to decide a
	// verdict. It is not the user-facing cap: a verdict computed from a
	// truncated set could be flatly wrong (the one object with a surviving
	// body might be the row that got cut), so the decision always sees
	// everything and only the echoed evidence is capped.
	verdictScanLimit = 10000
)

// inlineQuery carries the two knobs the row helpers share.
type inlineQuery struct {
	limit    int
	snippets bool
}

// inlineQueryFromRequest reads the limit / include_snippets arguments.
// Snippets default to off: they roughly double a row and every location
// can be re-read with read_source when the agent actually wants context.
func inlineQueryFromRequest(req mcp.CallToolRequest) inlineQuery {
	return inlineQuery{
		limit:    clampRange(req.GetInt("limit", inlineRowDefaultLimit), 1, inlineRowMaxLimit),
		snippets: req.GetBool("include_snippets", false),
	}
}

// registerCallGraphRuntimeTools wires the post-optimization view:
// list_linked_callers, list_linked_callees (objdump-derived edges,
// per-target) and describe_inlining (which callees were inlined
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

	s.mcp.AddTool(newTool("describe_inlining",
		mcp.WithDescription(
			"Everything that happened to the calls in one function, across three "+
				"planes. `records` is GCC's own optimization record: every pass's "+
				"decision, including the early inliner (pass 'einline') that folds "+
				"always_inline and trivial callees before any IPA pass runs, and "+
				"including the rejections with the compiler's reason for each. "+
				"`instances` is DWARF: the inlined bodies that actually survived "+
				"into the object's code — outcome rather than decision, so a callee "+
				"that folded to a constant afterwards has a record and no instance. "+
				"`cgraph_edges` is the older per-edge view from the .cgraph "+
				"`(inlined)` tag, IPA-stage only — a cross-check, not an answer. "+
				"For a verdict on one specific call rather than a survey of all of "+
				"them, use explain_call. Read `summary` first: it counts every row "+
				"across all three planes (totals, by pass, by inline depth) and "+
				"stays exact even when the row arrays are capped — for a heavily "+
				"inlined function those arrays are a sample, not the whole set, and "+
				"`truncated` says so.",
		),
		mcp.WithString("caller_usr", mcp.Required(),
			mcp.Description("USR of the caller (from find_symbol.hits[].usr or describe_symbol.usr).")),
		mcp.WithNumber("limit",
			mcp.Description("Cap on rows per array; default 50, max 1000. `summary` counts every row regardless."),
			mcp.Min(1)),
		mcp.WithBoolean("include_snippets",
			mcp.Description("Attach a ±5-line source window to each location. Off by default: it roughly doubles the payload, and read_source fetches context for the one location you care about.")),
	), s.handleDescribeInlining)

	s.mcp.AddTool(newTool("list_inline_instances",
		mcp.WithDescription(
			"The reverse of describe_inlining: everywhere one function's "+
				"body was inlined INTO another. Answers 'where did this helper go?' "+
				"for a symbol that has no runtime callers because it was folded "+
				"away. `instances` are the folds whose code survived into an object "+
				"(DWARF); `records` are the decisions GCC logged, rejections "+
				"included. A function with instances but no linked callers was "+
				"inlined everywhere, not dead. `summary` counts every row and is "+
				"exact even when the arrays are capped.",
		),
		mcp.WithString("callee_usr", mcp.Required(),
			mcp.Description("USR of the function that may have been inlined (from find_symbol.hits[].usr).")),
		mcp.WithNumber("limit",
			mcp.Description("Cap on rows per array; default 50, max 1000. `summary` counts every row regardless."),
			mcp.Min(1)),
		mcp.WithBoolean("include_snippets",
			mcp.Description("Attach a ±5-line source window to each location. Off by default: it roughly doubles the payload, and read_source fetches context for the one location you care about.")),
	), s.handleListInlineInstances)

	s.mcp.AddTool(newTool("explain_call",
		mcp.WithDescription(
			"One verdict for one call, with the evidence behind it. Use this "+
				"instead of assembling an answer from describe_inlining, "+
				"list_linked_callees and reachability by hand. verdict is one of: "+
				"'inlined_and_present' (a pass inlined it and the body is in the "+
				"object), 'inlined_then_folded' (a pass inlined it, then the copy "+
				"optimized away entirely — the call is gone but no body remains), "+
				"'declined' (GCC weighed it and kept the call; `reason` carries the "+
				"compiler's own words), 'no_decision_logged' (no pass reported on "+
				"this pair — usually means there is no such call, or it is "+
				"indirect: try list_indirect_call_sites), or 'mixed' when objects "+
				"disagree, in which case read per_object. Verdicts are per-object "+
				"because one USR can be compiled in several TUs — the fixture's two "+
				"`main`s, or a static-inline header — and the passes can decide "+
				"differently in each.",
		),
		mcp.WithString("caller_usr", mcp.Required(),
			mcp.Description("USR of the calling function (from find_symbol.hits[].usr).")),
		mcp.WithString("callee_usr", mcp.Required(),
			mcp.Description("USR of the called function (from find_symbol.hits[].usr).")),
		mcp.WithString("target",
			mcp.Description("Optional target name; restricts the linked_edge evidence to that binary's objdump view.")),
		mcp.WithNumber("limit",
			mcp.Description("Cap on evidence rows per array; default 50, max 1000. The verdict is always decided from the full set, capped or not."),
			mcp.Min(1)),
		mcp.WithBoolean("include_snippets",
			mcp.Description("Attach a ±5-line source window to each evidence location. Off by default.")),
	), s.handleExplainCall)
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

// -- describe_inlining ---------------------------------------

// CgraphInlineEdge is one row of describe_inlining.cgraph_edges — the
// .cgraph per-edge view. Corroborating only: it is IPA-stage, so an edge
// the early inliner folded never appears here at all.
type CgraphInlineEdge struct {
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
	Object   string   `json:"object"` // .o the record came from, builddir-relative
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

// DescribeInliningResponse is what describe_inlining returns. The three
// planes disagree by design — see the tool description for which one
// answers which question. Field order is deliberate: Records first
// because it is the only plane that sees every pass.
type DescribeInliningResponse struct {
	CallerUSR string `json:"caller_usr"`
	// Summary counts every matching row, so it stays true when the arrays
	// below are capped. Read it first.
	Summary InliningSummary `json:"summary"`
	// Records is every pass's verdict, the early inliner included, with
	// GCC's own reason on each rejection.
	Records []InlineRecordRow `json:"records"`
	// Instances is what actually survived into the object's code.
	Instances []InlineInstanceRow `json:"instances"`
	// CgraphEdges is the legacy IPA-stage per-edge view, kept as an
	// independent cross-check rather than as an answer in its own right.
	CgraphEdges []CgraphInlineEdge `json:"cgraph_edges"`
	// Truncated is true when any array hit the row cap. Summary still
	// holds the real totals; raise `limit` or aggregate with sql_query.
	Truncated bool `json:"truncated"`
}

// ListInlineInstancesResponse is what list_inline_instances returns.
type ListInlineInstancesResponse struct {
	CalleeUSR string              `json:"callee_usr"`
	Summary   InliningSummary     `json:"summary"`
	Instances []InlineInstanceRow `json:"instances"`
	Records   []InlineRecordRow   `json:"records"`
	Truncated bool                `json:"truncated"`
}

func (s *Server) handleDescribeInlining(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultError("describe_inlining: " + err.Error()), nil
	}
	defer rows.Close()
	var out []CgraphInlineEdge
	for rows.Next() {
		var d CgraphInlineEdge
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
	q := inlineQueryFromRequest(req)
	records, recTrunc, err := s.inlineRecords(q, "r.caller_id = ?", callerID)
	if err != nil {
		return mcp.NewToolResultError("describe_inlining: " + err.Error()), nil
	}
	instances, instTrunc, err := s.inlineInstances(q, "i.caller_id = ?", callerID)
	if err != nil {
		return mcp.NewToolResultError("describe_inlining: " + err.Error()), nil
	}
	summary, err := s.inliningSummary("caller_id", callerID)
	if err != nil {
		return mcp.NewToolResultError("describe_inlining: " + err.Error()), nil
	}
	edgesTrunc := len(out) > q.limit
	if edgesTrunc {
		out = out[:q.limit]
	}
	return jsonResult(DescribeInliningResponse{
		CallerUSR:   usr,
		Summary:     summary,
		Records:     records,
		Instances:   instances,
		CgraphEdges: out,
		Truncated:   recTrunc || instTrunc || edgesTrunc,
	})
}

// -- list_inline_instances -----------------------------------------------

func (s *Server) handleListInlineInstances(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	usr, err := req.RequireString("callee_usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	calleeID, err := s.symbolIDByUSR(usr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	q := inlineQueryFromRequest(req)
	instances, instTrunc, err := s.inlineInstances(q, "i.callee_id = ?", calleeID)
	if err != nil {
		return mcp.NewToolResultError("list_inline_instances: " + err.Error()), nil
	}
	records, recTrunc, err := s.inlineRecords(q, "r.callee_id = ?", calleeID)
	if err != nil {
		return mcp.NewToolResultError("list_inline_instances: " + err.Error()), nil
	}
	summary, err := s.inliningSummary("callee_id", calleeID)
	if err != nil {
		return mcp.NewToolResultError("list_inline_instances: " + err.Error()), nil
	}
	return jsonResult(ListInlineInstancesResponse{
		CalleeUSR: usr,
		Summary:   summary,
		Instances: instances,
		Records:   records,
		Truncated: instTrunc || recTrunc,
	})
}

// -- explain_call ----------------------------------------------------

// Verdict values for explain_call. Exported so tests and callers can
// compare against them rather than against string literals.
const (
	VerdictInlinedAndPresent = "inlined_and_present"
	VerdictInlinedThenFolded = "inlined_then_folded"
	VerdictDeclined          = "declined"
	VerdictNoDecisionLogged  = "no_decision_logged"
	VerdictMixed             = "mixed"
)

// ObjectInlineVerdict is one TU's answer. The per-object split is not a
// nicety: one USR can be compiled in several TUs, and the passes are free
// to decide differently in each.
type ObjectInlineVerdict struct {
	Object  string `json:"object"`
	Verdict string `json:"verdict"`
	Pass    string `json:"pass,omitempty"`   // pass that decided, when it declined
	Reason  string `json:"reason,omitempty"` // GCC's own words
}

// ExplainCallEvidence is everything the verdict was derived from, so an
// agent can audit it instead of trusting the label.
type ExplainCallEvidence struct {
	Records   []InlineRecordRow   `json:"records"`
	Instances []InlineInstanceRow `json:"instances"`
	// CgraphEdgeInlined is the legacy .cgraph tag for this pair, nil when
	// the pair has no cgraph edge at all.
	CgraphEdgeInlined *bool `json:"cgraph_edge_inlined,omitempty"`
	// LinkedEdge is whether objdump still sees a call instruction for this
	// pair in the linked binary (restricted to Target when one was given).
	// True alongside an inline verdict is normal: a callee can be inlined
	// at one site and called at another.
	LinkedEdge bool `json:"linked_edge"`
	// Truncated says the arrays above were capped. The verdict is not
	// affected — it is always decided from the full set.
	Truncated bool `json:"truncated"`
}

// ExplainCallResponse is what explain_call returns.
type ExplainCallResponse struct {
	Caller    SymbolRef             `json:"caller"`
	Callee    SymbolRef             `json:"callee"`
	Target    string                `json:"target,omitempty"`
	Verdict   string                `json:"verdict"`
	Reason    string                `json:"reason,omitempty"`
	PerObject []ObjectInlineVerdict `json:"per_object"`
	Evidence  ExplainCallEvidence   `json:"evidence"`
}

func (s *Server) handleExplainCall(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	callerUSR, err := req.RequireString("caller_usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	calleeUSR, err := req.RequireString("callee_usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	target := req.GetString("target", "")

	callerID, err := s.symbolIDByUSR(callerUSR)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	calleeID, err := s.symbolIDByUSR(calleeUSR)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	resp := ExplainCallResponse{Target: target}
	if err := s.db.QueryRow(
		`SELECT usr, name, kind, IFNULL(signature, '') FROM symbols WHERE id = ?`, callerID,
	).Scan(&resp.Caller.USR, &resp.Caller.Name, &resp.Caller.Kind, &resp.Caller.Signature); err != nil {
		return mcp.NewToolResultError("explain_call: caller: " + err.Error()), nil
	}
	if err := s.db.QueryRow(
		`SELECT usr, name, kind, IFNULL(signature, '') FROM symbols WHERE id = ?`, calleeID,
	).Scan(&resp.Callee.USR, &resp.Callee.Name, &resp.Callee.Kind, &resp.Callee.Signature); err != nil {
		return mcp.NewToolResultError("explain_call: callee: " + err.Error()), nil
	}

	// The verdict reads everything; only the echoed evidence is capped.
	// A verdict decided from a truncated set could be flatly wrong — the
	// one object with a surviving body might be the row that got cut.
	scan := inlineQuery{limit: verdictScanLimit}
	records, _, err := s.inlineRecords(scan, "r.caller_id = ? AND r.callee_id = ?", callerID, calleeID)
	if err != nil {
		return mcp.NewToolResultError("explain_call: " + err.Error()), nil
	}
	instances, _, err := s.inlineInstances(scan, "i.caller_id = ? AND i.callee_id = ?", callerID, calleeID)
	if err != nil {
		return mcp.NewToolResultError("explain_call: " + err.Error()), nil
	}

	q := inlineQueryFromRequest(req)
	resp.Evidence.Records, resp.Evidence.Instances = records, instances
	if len(records) > q.limit {
		resp.Evidence.Records = records[:q.limit]
		resp.Evidence.Truncated = true
	}
	if len(instances) > q.limit {
		resp.Evidence.Instances = instances[:q.limit]
		resp.Evidence.Truncated = true
	}
	if q.snippets {
		for i := range resp.Evidence.Records {
			s.enrichLocation(&resp.Evidence.Records[i].Location, true)
		}
		for i := range resp.Evidence.Instances {
			s.enrichLocation(&resp.Evidence.Instances[i].Location, true)
		}
	}

	// MAX() over no rows yields one NULL row rather than ErrNoRows, so
	// the nullable scan is what distinguishes "no cgraph edge" from
	// "edge, not inlined".
	var cgraph sql.NullInt64
	if err := s.db.QueryRow(
		`SELECT MAX(inlined) FROM inline_decisions WHERE caller_id = ? AND callee_id = ?`,
		callerID, calleeID,
	).Scan(&cgraph); err == nil && cgraph.Valid {
		v := cgraph.Int64 == 1
		resp.Evidence.CgraphEdgeInlined = &v
	}

	linkedQuery := `SELECT COUNT(*) FROM call_edges e
		WHERE e.caller_id = ? AND e.callee_id = ? AND e.source = 'objdump'`
	linkedArgs := []any{callerID, calleeID}
	if target != "" {
		linkedQuery += ` AND e.target_id = (SELECT id FROM targets WHERE name = ?)`
		linkedArgs = append(linkedArgs, target)
	}
	var linked int
	if err := s.db.QueryRow(linkedQuery, linkedArgs...).Scan(&linked); err != nil {
		return mcp.NewToolResultError("explain_call: linked edge: " + err.Error()), nil
	}
	resp.Evidence.LinkedEdge = linked > 0

	resp.PerObject = perObjectVerdicts(records, instances)
	resp.Verdict, resp.Reason = overallVerdict(resp.PerObject)
	return jsonResult(resp)
}

// perObjectVerdicts folds the two planes into one verdict per .o. An
// instance outranks a record: the body being physically present is
// stronger evidence than any pass's remark about it.
func perObjectVerdicts(records []InlineRecordRow, instances []InlineInstanceRow) []ObjectInlineVerdict {
	type acc struct {
		hasInstance bool
		inlined     bool
		declPass    string
		declReason  string
	}
	byObject := map[string]*acc{}
	order := []string{}
	get := func(obj string) *acc {
		if a, ok := byObject[obj]; ok {
			return a
		}
		a := &acc{}
		byObject[obj] = a
		order = append(order, obj)
		return a
	}
	for _, i := range instances {
		get(i.Object).hasInstance = true
	}
	for _, r := range records {
		a := get(r.Object)
		if r.Inlined {
			a.inlined = true
			continue
		}
		// The IPA pass runs last, so its reason is the one that stuck.
		if a.declPass != "inline" {
			a.declPass, a.declReason = r.Pass, r.Reason
		}
	}

	sort.Strings(order)
	out := make([]ObjectInlineVerdict, 0, len(order))
	for _, obj := range order {
		a := byObject[obj]
		v := ObjectInlineVerdict{Object: obj}
		switch {
		case a.hasInstance:
			v.Verdict = VerdictInlinedAndPresent
		case a.inlined:
			v.Verdict = VerdictInlinedThenFolded
		default:
			v.Verdict = VerdictDeclined
			v.Pass, v.Reason = a.declPass, a.declReason
		}
		out = append(out, v)
	}
	return out
}

// overallVerdict collapses the per-object verdicts, reporting 'mixed'
// rather than picking a winner when the TUs disagree.
func overallVerdict(per []ObjectInlineVerdict) (verdict, reason string) {
	if len(per) == 0 {
		return VerdictNoDecisionLogged, ""
	}
	first := per[0].Verdict
	for _, p := range per[1:] {
		if p.Verdict != first {
			return VerdictMixed, ""
		}
	}
	if first == VerdictDeclined {
		return first, per[0].Reason
	}
	return first, ""
}

// InliningSummary is the aggregate view, computed over every matching
// row regardless of the row cap. It exists because the useful default
// answer to "what happened to this function's calls" is counts, not a
// few hundred near-identical rows — and because a caller that hits the
// cap still needs a true total to know what it is not seeing.
type InliningSummary struct {
	Records         int            `json:"records"`
	RecordsInlined  int            `json:"records_inlined"`
	RecordsDeclined int            `json:"records_declined"`
	RecordsByPass   map[string]int `json:"records_by_pass"`
	Instances       int            `json:"instances"`
	// InstancesByDepth is keyed by depth as a string ("1", "2") because
	// JSON object keys are strings; depth 1 means inlined directly.
	InstancesByDepth map[string]int `json:"instances_by_depth"`
	CgraphEdges      int            `json:"cgraph_edges"`
}

// inliningSummary aggregates all three planes for one symbol. col is a
// literal from the call site — "caller_id" or "callee_id" — never user
// input.
func (s *Server) inliningSummary(col string, id int64) (InliningSummary, error) {
	sum := InliningSummary{
		RecordsByPass:    map[string]int{},
		InstancesByDepth: map[string]int{},
	}

	rows, err := s.db.Query(
		`SELECT pass, inlined, COUNT(*) FROM inline_records WHERE `+col+` = ? GROUP BY pass, inlined`, id)
	if err != nil {
		return sum, err
	}
	defer rows.Close()
	for rows.Next() {
		var pass string
		var inlined, n int
		if err := rows.Scan(&pass, &inlined, &n); err != nil {
			return sum, err
		}
		sum.Records += n
		sum.RecordsByPass[pass] += n
		if inlined == 1 {
			sum.RecordsInlined += n
		} else {
			sum.RecordsDeclined += n
		}
	}
	if err := rows.Err(); err != nil {
		return sum, err
	}

	irows, err := s.db.Query(
		`SELECT depth, COUNT(*) FROM inline_instances WHERE `+col+` = ? GROUP BY depth ORDER BY depth`, id)
	if err != nil {
		return sum, err
	}
	defer irows.Close()
	for irows.Next() {
		var depth, n int
		if err := irows.Scan(&depth, &n); err != nil {
			return sum, err
		}
		sum.Instances += n
		sum.InstancesByDepth[strconv.Itoa(depth)] = n
	}
	if err := irows.Err(); err != nil {
		return sum, err
	}

	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM inline_decisions WHERE `+col+` = ?`, id,
	).Scan(&sum.CgraphEdges); err != nil {
		return sum, err
	}
	return sum, nil
}

// inlineRecords reads inline_records under a caller_id or callee_id
// predicate. Both tools want the same columns, so the WHERE clause is
// the only thing that varies.
func (s *Server) inlineRecords(q inlineQuery, where string, args ...any) ([]InlineRecordRow, bool, error) {
	args = append(args, q.limit+1)
	rows, err := s.db.Query(`
		SELECT cr.usr, cr.name, cr.kind, IFNULL(cr.signature, ''),
		       ce.usr, ce.name, ce.kind, IFNULL(ce.signature, ''),
		       r.pass, r.inlined, IFNULL(r.reason, ''),
		       IFNULL(r.file, ''), IFNULL(r.line, 0), IFNULL(r.column, 0),
		       IFNULL(r.object, '')
		FROM inline_records r
		JOIN symbols cr ON cr.id = r.caller_id
		JOIN symbols ce ON ce.id = r.callee_id
		WHERE `+where+`
		ORDER BY r.pass, ce.name, r.file, r.line
		LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
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
			&rec.Pass, &inlined, &rec.Reason, &file, &line, &col, &rec.Object,
		); err != nil {
			return nil, false, err
		}
		rec.Inlined = inlined == 1
		rec.Location = Location{Path: file, Line: line, Column: col}
		s.enrichLocation(&rec.Location, q.snippets)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(out) > q.limit
	if truncated {
		out = out[:q.limit]
	}
	return out, truncated, nil
}

// inlineInstances reads inline_instances under a caller_id or callee_id
// predicate.
func (s *Server) inlineInstances(q inlineQuery, where string, args ...any) ([]InlineInstanceRow, bool, error) {
	args = append(args, q.limit+1)
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
		ORDER BY i.depth, ce.name, i.file, i.line
		LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
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
			return nil, false, err
		}
		if parent.USR != "" {
			inst.ParentCallee = &parent
		}
		inst.Location = Location{Path: file, Line: line, Column: col}
		s.enrichLocation(&inst.Location, q.snippets)
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(out) > q.limit
	if truncated {
		out = out[:q.limit]
	}
	return out, truncated, nil
}
