package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// registerLinkageTools wires the linkage + reachability tools:
// describe_link_resolution, list_weak_symbols, list_undefined_symbols,
// list_icf_groups, list_unreachable_symbols, list_entry_points.
func (s *Server) registerLinkageTools() {
	s.mcp.AddTool(newTool("describe_link_resolution",
		mcp.WithDescription(
			"Which object supplied a symbol in the given target: winning object, "+
				"linkage kind, losing objects (when a linker map is available), and "+
				"containing archive. The only reliable way to know which malloc "+
				"actually runs in a binary.",
		),
		mcp.WithString("usr", mcp.Required(),
			mcp.Description("USR of the symbol (from find_symbol.hits[].usr or describe_symbol.usr).")),
		mcp.WithString("target", mcp.Required(),
			mcp.Description("Target binary name — link resolution is per-target.")),
	), s.handleDescribeLinkResolution)

	s.mcp.AddTool(newTool("list_weak_symbols",
		mcp.WithDescription(
			"Every weak-linkage definition in the index, with per-target resolution "+
				"status when available.",
		),
		mcp.WithString("target",
			mcp.Description("Restrict to weak symbols reachable in this target.")),
	), s.handleListWeakSymbols)

	s.mcp.AddTool(newTool("list_undefined_symbols",
		mcp.WithDescription(
			"Externals this target references via a direct call but has no observed "+
				"definition for anywhere in the index (typical example: libc's printf). "+
				"Derived from objdump call edges minus symbol_definitions.",
		),
		mcp.WithString("target", mcp.Required(),
			mcp.Description("Target binary name — undefined externals are per-target.")),
	), s.handleListUndefinedSymbols)

	s.mcp.AddTool(newTool("list_icf_groups",
		mcp.WithDescription(
			"Functions merged by GCC's IPA identical-code folding (from "+
				"-fdump-ipa-icf). Each group has one winner (the surviving symbol) "+
				"and one or more losers whose bodies were rewritten to tail-call "+
				"winner.localalias. Linker-level ICF (gold/lld --icf=all) is a "+
				"separate pass and NOT tracked here — if the linker folded further, "+
				"this tool will underreport. Use with a target arg to restrict to "+
				"groups whose winner reaches that binary.",
		),
		mcp.WithString("target",
			mcp.Description("Restrict to groups whose winner has a link_resolutions row for this target.")),
	), s.handleListICFGroups)

	s.mcp.AddTool(newTool("list_unreachable_symbols",
		mcp.WithDescription(
			"Symbols with a definition but no link_resolutions row for this target — "+
				"a first-pass dead-code signal. Unfiltered, so expect false positives: "+
				"(1) internal-linkage symbols (static functions, header-defined "+
				"statics) are never surfaced in link_resolutions by design and always "+
				"appear here; (2) IPA-ICF losers appear here — cross-reference with "+
				"list_icf_groups, and the winner is what actually ships; (3) symbols "+
				"reachable only from __attribute__((constructor)) / .init_array chains "+
				"appear here because list_entry_points does not classify those; (4) "+
				"symbols inlined at every call site appear here — compare list_callees "+
				"against list_linked_callees on their callers to distinguish. Verify "+
				"any surprising hit with describe_symbol (per-target link_resolutions "+
				"in one call) before calling it dead.",
		),
		mcp.WithString("target", mcp.Required(),
			mcp.Description("Target binary name — reachability is per-target.")),
	), s.handleListUnreachableSymbols)

	s.mcp.AddTool(newTool("list_entry_points",
		mcp.WithDescription(
			"Root set for reachability queries in this target: 'main' + externally-"+
				"visible symbols that the linker resolved into this target. Initializer "+
				"arrays / constructor-attributed functions are not currently classified "+
				"(would require an additional DWARF pass) — if a symbol looks "+
				"unreachable but is reached from a constructor, describe_symbol on it "+
				"and inspect whether its callers carry __attribute__((constructor)).",
		),
		mcp.WithString("target", mcp.Required(),
			mcp.Description("Target binary name — entry points are per-target.")),
	), s.handleListEntryPoints)
}

// -- describe_link_resolution -----------------------------------------

// DescribeLinkResolutionResponse is what describe_link_resolution returns.
type DescribeLinkResolutionResponse struct {
	USR           string   `json:"usr"`
	Target        string   `json:"target"`
	WinningObject string   `json:"winning_object,omitempty"`
	LinkageKind   string   `json:"linkage_kind,omitempty"`
	LosingObjects []string `json:"losing_objects,omitempty"`
	Archive       string   `json:"archive,omitempty"`
	Reachable     bool     `json:"reachable"`
}

func (s *Server) handleDescribeLinkResolution(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	usr, err := req.RequireString("usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	target, err := req.RequireString("target")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	targetID, err := s.targetIDByName(target)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	resp := DescribeLinkResolutionResponse{USR: usr, Target: target}
	var losingJSON string
	if err := s.db.QueryRow(`
		SELECT IFNULL(winning_object, ''), IFNULL(linkage_kind, ''),
		       IFNULL(losing_objects, ''), IFNULL(archive, '')
		FROM link_resolutions
		WHERE usr = ? AND target_id = ?`, usr, targetID,
	).Scan(&resp.WinningObject, &resp.LinkageKind, &losingJSON, &resp.Archive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return mcp.NewToolResultError(fmt.Sprintf(
				"no link_resolutions row for %q in target %q — either the symbol is not linked into this target or it is unknown",
				usr, target)), nil
		}
		return mcp.NewToolResultError("describe_link_resolution: " + err.Error()), nil
	}
	if losingJSON != "" {
		var losers []string
		if err := json.Unmarshal([]byte(losingJSON), &losers); err == nil {
			resp.LosingObjects = losers
		}
	}
	// Reachability is a separate table; join in.
	var reach int
	_ = s.db.QueryRow(`
		SELECT r.reachable FROM symbol_reachability r
		JOIN symbols s ON s.id = r.symbol_id
		WHERE s.usr = ? AND r.target_id = ?`, usr, targetID,
	).Scan(&reach)
	resp.Reachable = reach == 1
	return jsonResult(resp)
}

// -- list_weak_symbols -----------------------------------------------

// WeakSymbol is one row of list_weak_symbols.
type WeakSymbol struct {
	Symbol      SymbolRef        `json:"symbol"`
	Definitions []Location       `json:"definitions"` // every observed def marked is_weak=1
	Resolutions []LinkResolution `json:"resolutions,omitempty"`
}

// ListWeakSymbolsResponse is what list_weak_symbols returns.
type ListWeakSymbolsResponse struct {
	Symbols []WeakSymbol `json:"symbols"`
	Total   int          `json:"total"`
}

func (s *Server) handleListWeakSymbols(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target := req.GetString("target", "")

	// A symbol is "weak" per the schema if any of its definitions has
	// is_weak=1 OR the aggregated linkage is 'weak'. Prefer the def-
	// level flag so we surface hooks that have both a weak fallback and
	// a strong override.
	sqlText := `
		SELECT DISTINCT s.id, s.usr, s.name, s.kind, IFNULL(s.signature, '')
		FROM symbols s
		WHERE s.id IN (
		    SELECT sd.symbol_id FROM symbol_definitions sd WHERE sd.is_weak = 1
		) OR s.linkage = 'weak'`
	var args []any
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
		return mcp.NewToolResultError("list_weak_symbols: " + err.Error()), nil
	}
	defer rows.Close()

	type row struct {
		id  int64
		sym SymbolRef
	}
	var items []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.sym.USR, &r.sym.Name, &r.sym.Kind, &r.sym.Signature); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}
	out := make([]WeakSymbol, 0, len(items))
	for _, it := range items {
		defs, err := s.weakDefinitions(it.id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		_, res, err := s.symbolTargets(it.sym.USR, it.id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		out = append(out, WeakSymbol{Symbol: it.sym, Definitions: defs, Resolutions: res})
	}
	return jsonResult(ListWeakSymbolsResponse{Symbols: out, Total: len(out)})
}

// weakDefinitions returns Location entries for every is_weak=1 def
// under a given symbol_id. Snippet expansion is on so the agent has
// the surrounding context.
func (s *Server) weakDefinitions(symbolID int64) ([]Location, error) {
	rows, err := s.db.Query(`
		SELECT file, line FROM symbol_definitions
		WHERE symbol_id = ? AND is_weak = 1 ORDER BY file, line`, symbolID)
	if err != nil {
		return nil, fmt.Errorf("weak defs: %w", err)
	}
	defer rows.Close()
	var out []Location
	for rows.Next() {
		var f string
		var l int
		if err := rows.Scan(&f, &l); err != nil {
			return nil, err
		}
		loc := Location{Path: f, Line: l}
		s.enrichLocation(&loc, true)
		out = append(out, loc)
	}
	return out, rows.Err()
}

// -- list_undefined_symbols ------------------------------------------

// UndefinedSymbol is one row of list_undefined_symbols.
type UndefinedSymbol struct {
	Symbol      SymbolRef `json:"symbol"`
	LikelyLib   string    `json:"likely_lib,omitempty"` // heuristic (empty on the current pass)
	CalledCount int       `json:"called_count"`         // objdump edges into this symbol from the target
}

// ListUndefinedSymbolsResponse is what list_undefined_symbols returns.
type ListUndefinedSymbolsResponse struct {
	Target  string            `json:"target"`
	Symbols []UndefinedSymbol `json:"symbols"`
	Total   int               `json:"total"`
}

func (s *Server) handleListUndefinedSymbols(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target, err := req.RequireString("target")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	targetID, err := s.targetIDByName(target)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	rows, err := s.db.Query(`
		SELECT s.usr, s.name, s.kind, IFNULL(s.signature, ''), COUNT(*) AS calls
		FROM call_edges e
		JOIN symbols s ON s.id = e.callee_id
		WHERE e.source = 'objdump' AND e.target_id = ?
		  AND NOT EXISTS (SELECT 1 FROM symbol_definitions sd WHERE sd.symbol_id = s.id)
		GROUP BY s.id
		ORDER BY s.name`, targetID)
	if err != nil {
		return mcp.NewToolResultError("list_undefined_symbols: " + err.Error()), nil
	}
	defer rows.Close()
	var out []UndefinedSymbol
	for rows.Next() {
		var u UndefinedSymbol
		if err := rows.Scan(&u.Symbol.USR, &u.Symbol.Name, &u.Symbol.Kind, &u.Symbol.Signature, &u.CalledCount); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}
	return jsonResult(ListUndefinedSymbolsResponse{Target: target, Symbols: out, Total: len(out)})
}

// -- list_icf_groups -------------------------------------------------

// ICFGroup is one folded-symbol cluster.
type ICFGroup struct {
	Winner     SymbolRef   `json:"winner"`
	Losers     []SymbolRef `json:"losers"`
	ObjectFile string      `json:"object_file"`
}

// ListICFGroupsResponse is what list_icf_groups returns.
type ListICFGroupsResponse struct {
	Groups []ICFGroup `json:"groups"`
	Total  int        `json:"total"`
}

func (s *Server) handleListICFGroups(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target := req.GetString("target", "")
	var targetID int64
	if target != "" {
		id, err := s.targetIDByName(target)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		targetID = id
	}

	// Fetch groups first, then members per group. Two-step keeps the
	// SymbolRef marshaling straightforward.
	var (
		rows *sql.Rows
		err  error
	)
	if target != "" {
		rows, err = s.db.Query(`
			SELECT g.id, g.object_file, w.usr, w.name, w.kind, IFNULL(w.signature, '')
			FROM icf_groups g
			JOIN symbols w ON w.id = g.winner_symbol_id
			WHERE EXISTS (
			  SELECT 1 FROM link_resolutions lr
			  WHERE lr.target_id = ? AND lr.usr = w.usr)
			ORDER BY w.name`, targetID)
	} else {
		rows, err = s.db.Query(`
			SELECT g.id, g.object_file, w.usr, w.name, w.kind, IFNULL(w.signature, '')
			FROM icf_groups g
			JOIN symbols w ON w.id = g.winner_symbol_id
			ORDER BY w.name`)
	}
	if err != nil {
		return mcp.NewToolResultError("list_icf_groups: " + err.Error()), nil
	}
	defer rows.Close()

	type groupRow struct {
		id  int64
		obj string
		win SymbolRef
	}
	var groups []groupRow
	for rows.Next() {
		var g groupRow
		if err := rows.Scan(&g.id, &g.obj, &g.win.USR, &g.win.Name, &g.win.Kind, &g.win.Signature); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}

	out := make([]ICFGroup, 0, len(groups))
	for _, g := range groups {
		lrows, err := s.db.Query(`
			SELECT s.usr, s.name, s.kind, IFNULL(s.signature, '')
			FROM icf_group_members m
			JOIN symbols s ON s.id = m.symbol_id
			WHERE m.group_id = ?
			ORDER BY s.name`, g.id)
		if err != nil {
			return mcp.NewToolResultError("list_icf_groups members: " + err.Error()), nil
		}
		var losers []SymbolRef
		for lrows.Next() {
			var r SymbolRef
			if err := lrows.Scan(&r.USR, &r.Name, &r.Kind, &r.Signature); err != nil {
				lrows.Close()
				return mcp.NewToolResultError("scan: " + err.Error()), nil
			}
			losers = append(losers, r)
		}
		if err := lrows.Err(); err != nil {
			lrows.Close()
			return mcp.NewToolResultError("iterate: " + err.Error()), nil
		}
		lrows.Close()
		out = append(out, ICFGroup{
			Winner:     g.win,
			Losers:     losers,
			ObjectFile: g.obj,
		})
	}
	return jsonResult(ListICFGroupsResponse{Groups: out, Total: len(out)})
}

// -- list_unreachable_symbols ----------------------------------------

// UnreachableSymbol is one row of list_unreachable_symbols.
type UnreachableSymbol struct {
	Symbol       SymbolRef `json:"symbol"`
	SectionKept  bool      `json:"section_kept"`
}

// ListUnreachableSymbolsResponse is what list_unreachable_symbols returns.
type ListUnreachableSymbolsResponse struct {
	Target  string              `json:"target"`
	Symbols []UnreachableSymbol `json:"symbols"`
	Total   int                 `json:"total"`
}

func (s *Server) handleListUnreachableSymbols(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target, err := req.RequireString("target")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	targetID, err := s.targetIDByName(target)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// symbol_reachability is a view derived from link_resolutions: absence
	// of a link_resolutions row for (target, symbol) is exactly the old
	// reachable=0 case. The kind guard preserves the old table's implicit
	// filter — the ingest write loop skipped static libraries entirely, so
	// asking about "unreachable symbols in a static library" returned empty.
	rows, err := s.db.Query(`
		SELECT s.usr, s.name, s.kind, IFNULL(s.signature, ''), 1
		FROM symbols s
		WHERE EXISTS (
		    SELECT 1 FROM targets t
		    WHERE t.id = ? AND t.kind IN ('executable', 'shared_library'))
		  AND NOT EXISTS (
		    SELECT 1 FROM link_resolutions lr
		    WHERE lr.target_id = ? AND lr.usr = s.usr)
		ORDER BY s.name`, targetID, targetID)
	if err != nil {
		return mcp.NewToolResultError("list_unreachable_symbols: " + err.Error()), nil
	}
	defer rows.Close()
	var out []UnreachableSymbol
	for rows.Next() {
		var u UnreachableSymbol
		var kept int
		if err := rows.Scan(&u.Symbol.USR, &u.Symbol.Name, &u.Symbol.Kind, &u.Symbol.Signature, &kept); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		u.SectionKept = kept == 1
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}
	return jsonResult(ListUnreachableSymbolsResponse{Target: target, Symbols: out, Total: len(out)})
}

// -- list_entry_points -----------------------------------------------

// EntryPoint is one row of list_entry_points.
type EntryPoint struct {
	Symbol   SymbolRef `json:"symbol"`
	Reason   string    `json:"reason"` // 'main' | 'exported'
	Location Location  `json:"location"`
}

// ListEntryPointsResponse is what list_entry_points returns.
type ListEntryPointsResponse struct {
	Target string       `json:"target"`
	Points []EntryPoint `json:"points"`
	Total  int          `json:"total"`
	Note   string       `json:"note,omitempty"`
}

func (s *Server) handleListEntryPoints(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target, err := req.RequireString("target")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	targetID, err := s.targetIDByName(target)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Two categories: main + external-linkage symbols. Constructor-
	// attributed functions and .init_array entries would need a DWARF
	// pass we don't currently do; noted in the response.
	rows, err := s.db.Query(`
		SELECT s.usr, s.name, s.kind, IFNULL(s.signature, ''), s.linkage,
		       IFNULL(sd.file, ''), IFNULL(sd.line, 0)
		FROM symbols s
		JOIN symbol_reachability r ON r.symbol_id = s.id
		LEFT JOIN symbol_definitions sd ON sd.symbol_id = s.id
		WHERE r.target_id = ? AND r.reachable = 1
		  AND (s.name = 'main' OR s.linkage = 'external')
		ORDER BY (s.name = 'main') DESC, s.name`, targetID)
	if err != nil {
		return mcp.NewToolResultError("list_entry_points: " + err.Error()), nil
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []EntryPoint
	for rows.Next() {
		var ep EntryPoint
		var linkage, file string
		var line int
		if err := rows.Scan(&ep.Symbol.USR, &ep.Symbol.Name, &ep.Symbol.Kind, &ep.Symbol.Signature, &linkage, &file, &line); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		if seen[ep.Symbol.USR] {
			continue
		}
		seen[ep.Symbol.USR] = true
		if ep.Symbol.Name == "main" {
			ep.Reason = "main"
		} else {
			ep.Reason = "exported"
		}
		if file != "" {
			ep.Location = Location{Path: file, Line: line}
			s.enrichLocation(&ep.Location, false)
		}
		out = append(out, ep)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}
	return jsonResult(ListEntryPointsResponse{
		Target: target, Points: out, Total: len(out),
		Note: "Constructor-attributed (__attribute__((constructor))) and .init_array entries are not currently classified.",
	})
}
