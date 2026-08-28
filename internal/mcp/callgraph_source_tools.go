package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

const (
	callGraphNeighborLimit  = 500
	callPathDefaultDepth    = 6
	callPathMaxDepth        = 16
	callPathResultLimit     = 50
)

// registerCallGraphSourceTools wires the source-view call-graph tools:
// list_callers, list_callees, list_call_paths. All three answer against
// call_edges rows with source='compiler_cgraph' — GCC's pre-optimization
// callgraph, which is target-agnostic. Post-inlining reality lives in
// list_linked_callers / list_linked_callees.
func (s *Server) registerCallGraphSourceTools() {
	s.mcp.AddTool(newTool("list_callers",
		mcp.WithDescription(
			"Who calls this symbol per GCC's cgraph (source view; pre-inlining, "+
				"target-agnostic). Use list_linked_callers for the post-optimization "+
				"per-binary reality.",
		),
		mcp.WithString("callee_usr", mcp.Required(),
			mcp.Description("USR of the callee (from find_symbol / describe_symbol).")),
		mcp.WithString("target",
			mcp.Description("Restrict to callers reachable in this target.")),
	), s.handleListCallers)

	s.mcp.AddTool(newTool("list_callees",
		mcp.WithDescription(
			"Direct callees from this symbol per GCC's cgraph (source view).",
		),
		mcp.WithString("caller_usr", mcp.Required(),
			mcp.Description("USR of the caller.")),
		mcp.WithString("target",
			mcp.Description("Restrict to callees reachable in this target.")),
	), s.handleListCallees)

	s.mcp.AddTool(newTool("list_call_paths",
		mcp.WithDescription(
			"Enumerate direct-call paths between two symbols via GCC's cgraph. "+
				"Bounded by max_depth (default 6, max 16). Returns up to 50 paths.",
		),
		mcp.WithString("from_usr", mcp.Required()),
		mcp.WithString("to_usr", mcp.Required()),
		mcp.WithNumber("max_depth",
			mcp.Description("Maximum path length (edges). Default 6, max 16."),
			mcp.Min(1),
			mcp.Max(float64(callPathMaxDepth))),
		mcp.WithString("target",
			mcp.Description("Restrict to paths whose every node is reachable in this target.")),
	), s.handleListCallPaths)
}

// SymbolRef is a compact identity payload used across the call-graph
// tools where the caller/callee doesn't need the full describe_symbol
// treatment.
type SymbolRef struct {
	USR       string `json:"usr"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Signature string `json:"signature,omitempty"`
}

// -- list_callers -----------------------------------------------------

// CallerEdge is one row of list_callers.
type CallerEdge struct {
	Caller  SymbolRef `json:"caller"`
	Targets []string  `json:"targets,omitempty"` // targets where the caller is reachable
}

// ListCallersResponse is what list_callers returns.
type ListCallersResponse struct {
	CalleeUSR string       `json:"callee_usr"`
	Callers   []CallerEdge `json:"callers"`
	Total     int          `json:"total"`
	Truncated bool         `json:"truncated"`
}

func (s *Server) handleListCallers(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	usr, err := req.RequireString("callee_usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	target := req.GetString("target", "")

	calleeID, err := s.symbolIDByUSR(usr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	sqlText := `
		SELECT DISTINCT c.usr, c.name, c.kind, IFNULL(c.signature, '')
		FROM call_edges e
		JOIN symbols c ON c.id = e.caller_id
		WHERE e.callee_id = ? AND e.source = 'compiler_cgraph'`
	args := []any{calleeID}
	if target != "" {
		sqlText += ` AND EXISTS (
			SELECT 1 FROM symbol_reachability r
			JOIN targets t ON t.id = r.target_id
			WHERE r.symbol_id = c.id AND r.reachable = 1 AND t.name = ?
		)`
		args = append(args, target)
	}
	sqlText += ` ORDER BY c.name LIMIT ?`
	args = append(args, callGraphNeighborLimit+1)

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return mcp.NewToolResultError("list_callers: " + err.Error()), nil
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
	truncated := len(refs) > callGraphNeighborLimit
	if truncated {
		refs = refs[:callGraphNeighborLimit]
	}
	targetsByUSR, err := s.targetsByReachability(usrsOf(refs))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	edges := make([]CallerEdge, 0, len(refs))
	for _, r := range refs {
		edges = append(edges, CallerEdge{Caller: r, Targets: targetsByUSR[r.USR]})
	}
	return jsonResult(ListCallersResponse{
		CalleeUSR: usr,
		Callers:   edges,
		Total:     len(edges),
		Truncated: truncated,
	})
}

// -- list_callees -----------------------------------------------------

// CalleeEdge is one row of list_callees.
type CalleeEdge struct {
	Callee  SymbolRef `json:"callee"`
	Targets []string  `json:"targets,omitempty"`
}

// ListCalleesResponse is what list_callees returns.
type ListCalleesResponse struct {
	CallerUSR string       `json:"caller_usr"`
	Callees   []CalleeEdge `json:"callees"`
	Total     int          `json:"total"`
	Truncated bool         `json:"truncated"`
}

func (s *Server) handleListCallees(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	usr, err := req.RequireString("caller_usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	target := req.GetString("target", "")

	callerID, err := s.symbolIDByUSR(usr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	sqlText := `
		SELECT DISTINCT c.usr, c.name, c.kind, IFNULL(c.signature, '')
		FROM call_edges e
		JOIN symbols c ON c.id = e.callee_id
		WHERE e.caller_id = ? AND e.source = 'compiler_cgraph'`
	args := []any{callerID}
	if target != "" {
		sqlText += ` AND EXISTS (
			SELECT 1 FROM symbol_reachability r
			JOIN targets t ON t.id = r.target_id
			WHERE r.symbol_id = c.id AND r.reachable = 1 AND t.name = ?
		)`
		args = append(args, target)
	}
	sqlText += ` ORDER BY c.name LIMIT ?`
	args = append(args, callGraphNeighborLimit+1)

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return mcp.NewToolResultError("list_callees: " + err.Error()), nil
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
	truncated := len(refs) > callGraphNeighborLimit
	if truncated {
		refs = refs[:callGraphNeighborLimit]
	}
	targetsByUSR, err := s.targetsByReachability(usrsOf(refs))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	edges := make([]CalleeEdge, 0, len(refs))
	for _, r := range refs {
		edges = append(edges, CalleeEdge{Callee: r, Targets: targetsByUSR[r.USR]})
	}
	return jsonResult(ListCalleesResponse{
		CallerUSR: usr,
		Callees:   edges,
		Total:     len(edges),
		Truncated: truncated,
	})
}

// -- list_call_paths --------------------------------------------------

// CallPath is one enumerated path (edges via cgraph).
type CallPath struct {
	Depth int         `json:"depth"`
	Steps []SymbolRef `json:"steps"`
}

// ListCallPathsResponse is what list_call_paths returns.
type ListCallPathsResponse struct {
	FromUSR   string     `json:"from_usr"`
	ToUSR     string     `json:"to_usr"`
	MaxDepth  int        `json:"max_depth"`
	Paths     []CallPath `json:"paths"`
	Total     int        `json:"total"`
	Truncated bool       `json:"truncated"`
}

func (s *Server) handleListCallPaths(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fromUSR, err := req.RequireString("from_usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	toUSR, err := req.RequireString("to_usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	maxDepth := req.GetInt("max_depth", callPathDefaultDepth)
	if maxDepth <= 0 || maxDepth > callPathMaxDepth {
		maxDepth = callPathDefaultDepth
	}
	target := req.GetString("target", "")

	fromID, err := s.symbolIDByUSR(fromUSR)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	toID, err := s.symbolIDByUSR(toUSR)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	adj, byID, err := s.loadCgraphAdjacency(target)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	paths := enumeratePaths(adj, fromID, toID, maxDepth, callPathResultLimit+1)
	truncated := len(paths) > callPathResultLimit
	if truncated {
		paths = paths[:callPathResultLimit]
	}

	out := make([]CallPath, 0, len(paths))
	for _, p := range paths {
		steps := make([]SymbolRef, 0, len(p))
		for _, id := range p {
			steps = append(steps, byID[id])
		}
		out = append(out, CallPath{Depth: len(p) - 1, Steps: steps})
	}
	return jsonResult(ListCallPathsResponse{
		FromUSR:   fromUSR,
		ToUSR:     toUSR,
		MaxDepth:  maxDepth,
		Paths:     out,
		Total:     len(out),
		Truncated: truncated,
	})
}

// loadCgraphAdjacency preloads the compiler_cgraph edge set into an
// in-memory adjacency map keyed by caller_id. Optionally restricts to
// symbols reachable in target. The identity payload is loaded in the
// same pass so path reconstruction doesn't need per-hop DB round-trips.
func (s *Server) loadCgraphAdjacency(target string) (map[int64][]int64, map[int64]SymbolRef, error) {
	// Load every symbol first — a symbol may be a hop node even if it
	// has no outgoing edges in the filter set.
	sqlSyms := `SELECT id, usr, name, kind, IFNULL(signature, '') FROM symbols`
	args := []any{}
	if target != "" {
		sqlSyms = `
			SELECT s.id, s.usr, s.name, s.kind, IFNULL(s.signature, '')
			FROM symbols s
			JOIN symbol_reachability r ON r.symbol_id = s.id
			JOIN targets t ON t.id = r.target_id
			WHERE r.reachable = 1 AND t.name = ?`
		args = append(args, target)
	}
	rows, err := s.db.Query(sqlSyms, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("load symbols: %w", err)
	}
	byID := map[int64]SymbolRef{}
	for rows.Next() {
		var id int64
		var r SymbolRef
		if err := rows.Scan(&id, &r.USR, &r.Name, &r.Kind, &r.Signature); err != nil {
			rows.Close()
			return nil, nil, err
		}
		byID[id] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	edgeRows, err := s.db.Query(`
		SELECT caller_id, callee_id FROM call_edges
		WHERE source = 'compiler_cgraph'`)
	if err != nil {
		return nil, nil, fmt.Errorf("load edges: %w", err)
	}
	defer edgeRows.Close()
	adj := map[int64][]int64{}
	seen := map[[2]int64]bool{}
	for edgeRows.Next() {
		var caller, callee int64
		if err := edgeRows.Scan(&caller, &callee); err != nil {
			return nil, nil, err
		}
		if target != "" {
			if _, ok := byID[caller]; !ok {
				continue
			}
			if _, ok := byID[callee]; !ok {
				continue
			}
		}
		k := [2]int64{caller, callee}
		if seen[k] {
			continue
		}
		seen[k] = true
		adj[caller] = append(adj[caller], callee)
	}
	if err := edgeRows.Err(); err != nil {
		return nil, nil, err
	}
	// Deterministic order for path enumeration.
	for k := range adj {
		sort.Slice(adj[k], func(i, j int) bool {
			return byID[adj[k][i]].Name < byID[adj[k][j]].Name
		})
	}
	return adj, byID, nil
}

// enumeratePaths runs DFS from start to end, up to maxDepth edges,
// caps at limit paths, and rejects cycles-within-path via a visited
// set. Trivial start==end returns a single-node path.
func enumeratePaths(adj map[int64][]int64, start, end int64, maxDepth, limit int) [][]int64 {
	var out [][]int64
	if start == end {
		out = append(out, []int64{start})
		return out
	}
	stack := []int64{start}
	visited := map[int64]bool{start: true}
	var dfs func()
	dfs = func() {
		if len(out) >= limit {
			return
		}
		if len(stack)-1 >= maxDepth {
			return
		}
		last := stack[len(stack)-1]
		for _, next := range adj[last] {
			if visited[next] {
				continue
			}
			stack = append(stack, next)
			if next == end {
				path := make([]int64, len(stack))
				copy(path, stack)
				out = append(out, path)
				if len(out) >= limit {
					stack = stack[:len(stack)-1]
					return
				}
			} else {
				visited[next] = true
				dfs()
				delete(visited, next)
			}
			stack = stack[:len(stack)-1]
		}
	}
	dfs()
	return out
}

// -- shared helpers ---------------------------------------------------

// symbolIDByUSR looks up a symbol row id from its USR. Returns a
// user-friendly error when the USR is unknown.
func (s *Server) symbolIDByUSR(usr string) (int64, error) {
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM symbols WHERE usr = ?`, usr).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("no symbol with USR %q", usr)
		}
		return 0, fmt.Errorf("lookup %q: %w", usr, err)
	}
	return id, nil
}

// targetIDByName is the target-name-side of symbolIDByUSR.
func (s *Server) targetIDByName(name string) (int64, error) {
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM targets WHERE name = ?`, name).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("unknown target %q", name)
		}
		return 0, fmt.Errorf("target lookup %q: %w", name, err)
	}
	return id, nil
}

// targetsByReachability returns, for each USR in the input set, the
// list of target names in which the symbol is reachable.
func (s *Server) targetsByReachability(usrs []string) (map[string][]string, error) {
	if len(usrs) == 0 {
		return map[string][]string{}, nil
	}
	placeholders := strings.Repeat("?,", len(usrs))
	placeholders = placeholders[:len(placeholders)-1]
	q := `
		SELECT s.usr, t.name
		FROM symbol_reachability r
		JOIN symbols s ON s.id = r.symbol_id
		JOIN targets t ON t.id = r.target_id
		WHERE r.reachable = 1 AND s.usr IN (` + placeholders + `)
		ORDER BY t.name`
	args := make([]any, len(usrs))
	for i, u := range usrs {
		args[i] = u
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("reachability lookup: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var u, t string
		if err := rows.Scan(&u, &t); err != nil {
			return nil, err
		}
		out[u] = append(out[u], t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func usrsOf(refs []SymbolRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.USR
	}
	return out
}
