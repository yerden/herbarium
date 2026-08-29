package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

const findSymbolLimit = 100

// registerSymbolTools wires find_symbol + describe_symbol.
func (s *Server) registerSymbolTools() {
	s.mcp.AddTool(newTool("find_symbol",
		mcp.WithDescription(
			"Fuzzy FTS lookup over symbol names and signatures. Handles identifier-"+
				"boundary tokenization (add_ints → 'add ints') and prefix matches. "+
				"Scopeable by symbol kind or by target membership.",
		),
		mcp.WithString("query", mcp.Required(),
			mcp.Description("Substring or partial identifier. Split on non-alphanumeric boundaries (so 'add_ints' tokenizes to 'add ints') and each token gets a trailing '*' for prefix matching — 'add' matches 'add_ints', 'adder', 'quick_add'. All-punctuation input matches nothing.")),
		mcp.WithString("kind",
			mcp.Description("Filter by symbols.kind. Common values: 'function', 'variable', 'typedef' — call describe_schema for the full enum.")),
		mcp.WithString("target",
			mcp.Description("Restrict to symbols linked into this target.")),
	), s.handleFindSymbol)

	s.mcp.AddTool(newTool("describe_symbol",
		mcp.WithDescription(
			"Canonical profile of one symbol by USR: name, kind, linkage, signature, "+
				"address-taken flag, every observed definition, every link-time name, "+
				"and per-target link-resolution outcome. This is the one-shot check "+
				"for 'is this symbol dead in target T?' — the per-target "+
				"link_resolutions list is exactly what list_unreachable_symbols "+
				"draws on, so a symbol with a resolution here is not dead there.",
		),
		mcp.WithString("usr", mcp.Required(),
			mcp.Description("The stable USR ('c:@F@name' or 'c:<path>@F@name') as returned by find_symbol.hits[].usr.")),
	), s.handleDescribeSymbol)
}

// -- find_symbol ------------------------------------------------------

// SymbolHit is one row of find_symbol.
type SymbolHit struct {
	USR       string   `json:"usr"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Linkage   string   `json:"linkage"`
	Signature string   `json:"signature,omitempty"`
	Targets   []string `json:"targets,omitempty"` // targets where the symbol is reachable
}

// FindSymbolResponse is what find_symbol returns.
type FindSymbolResponse struct {
	Query     string      `json:"query"`
	FTSQuery  string      `json:"fts_query"`
	Hits      []SymbolHit `json:"hits"`
	Total     int         `json:"total"`
	Truncated bool        `json:"truncated"`
}

func (s *Server) handleFindSymbol(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	kind := req.GetString("kind", "")
	target := req.GetString("target", "")

	fts := buildFTSQuery(query)
	if fts == "" {
		return jsonResult(FindSymbolResponse{Query: query})
	}

	// Two-stage query: FTS narrows candidate ids; the join returns the
	// symbol row + linkage + signature. The target filter — when set —
	// restricts to symbols the linker resolved into that target
	// (link_resolutions rows), covering both defined-here and pulled-
	// from-archive cases.
	sqlText := `
		SELECT s.usr, s.name, s.kind, s.linkage, IFNULL(s.signature, '')
		FROM symbols_fts f
		JOIN symbols s ON s.id = f.rowid
		WHERE symbols_fts MATCH ?`
	args := []any{fts}
	if kind != "" {
		sqlText += ` AND s.kind = ?`
		args = append(args, kind)
	}
	if target != "" {
		sqlText += ` AND EXISTS (
			SELECT 1 FROM link_resolutions lr
			JOIN targets t ON t.id = lr.target_id
			WHERE lr.usr = s.usr AND t.name = ?
		)`
		args = append(args, target)
	}
	sqlText += ` ORDER BY s.name LIMIT ?`
	args = append(args, findSymbolLimit+1)

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return mcp.NewToolResultError("find_symbol query: " + err.Error()), nil
	}
	defer rows.Close()
	var hits []SymbolHit
	for rows.Next() {
		var h SymbolHit
		if err := rows.Scan(&h.USR, &h.Name, &h.Kind, &h.Linkage, &h.Signature); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}

	truncated := len(hits) > findSymbolLimit
	if truncated {
		hits = hits[:findSymbolLimit]
	}

	if err := s.attachTargets(hits); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return jsonResult(FindSymbolResponse{
		Query:     query,
		FTSQuery:  fts,
		Hits:      hits,
		Total:     len(hits),
		Truncated: truncated,
	})
}

// buildFTSQuery converts a user query into a safe FTS5 MATCH expression.
// The symbols_fts tokenizer treats '_' as a separator, so we split on
// non-alphanumeric to mirror it, then append '*' to each token for
// prefix matching. Returns "" when the input is all separators.
func buildFTSQuery(q string) string {
	var toks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String()+"*")
			cur.Reset()
		}
	}
	for _, r := range q {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return strings.Join(toks, " ")
}

// attachTargets fills SymbolHit.Targets from symbol_reachability
// (reachable=1) so the agent can see at a glance which binaries pull a
// symbol in. Batched to a single SQL round-trip.
func (s *Server) attachTargets(hits []SymbolHit) error {
	if len(hits) == 0 {
		return nil
	}
	// Build IN-list of USRs. SQLite has a compile-time limit on the
	// number of parameters (~999), so cap.
	usrs := make([]string, 0, len(hits))
	for _, h := range hits {
		usrs = append(usrs, h.USR)
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
		return fmt.Errorf("attach targets: %w", err)
	}
	defer rows.Close()
	byUSR := map[string][]string{}
	for rows.Next() {
		var u, tname string
		if err := rows.Scan(&u, &tname); err != nil {
			return err
		}
		byUSR[u] = append(byUSR[u], tname)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range hits {
		hits[i].Targets = byUSR[hits[i].USR]
	}
	return nil
}

// -- describe_symbol --------------------------------------------------

// SymbolDefinition is one row of symbol_definitions for a symbol.
type SymbolDefinition struct {
	Location    Location `json:"location"`
	DeclFile    string   `json:"decl_file,omitempty"`
	DeclLine    int      `json:"decl_line,omitempty"`
	IsWeak      bool     `json:"is_weak,omitempty"`
	LinkageName string   `json:"linkage_name,omitempty"`
}

// LinkResolution is one per-target link-resolution row (Phase 4 data).
type LinkResolution struct {
	Target        string   `json:"target"`
	WinningObject string   `json:"winning_object,omitempty"`
	LinkageKind   string   `json:"linkage_kind,omitempty"`
	LosingObjects []string `json:"losing_objects,omitempty"`
	Archive       string   `json:"archive,omitempty"`
	Reachable     bool     `json:"reachable"`
}

// DescribeSymbolResponse is what describe_symbol returns.
type DescribeSymbolResponse struct {
	USR             string             `json:"usr"`
	Name            string             `json:"name"`
	Kind            string             `json:"kind"`
	Linkage         string             `json:"linkage"`
	Signature       string             `json:"signature,omitempty"`
	AddressTaken    bool               `json:"address_taken"`
	LinkageNames    []string           `json:"linkage_names,omitempty"`
	Definitions     []SymbolDefinition `json:"definitions"`
	Targets         []string           `json:"targets,omitempty"`
	LinkResolutions []LinkResolution   `json:"link_resolutions,omitempty"`
}

func (s *Server) handleDescribeSymbol(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	usr, err := req.RequireString("usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var symID int64
	resp := DescribeSymbolResponse{USR: usr}
	var linkageNamesJSON sql.NullString
	var addr int
	if err := s.db.QueryRow(`
		SELECT id, name, kind, linkage, IFNULL(signature, ''), address_taken, linkage_names
		FROM symbols WHERE usr = ?`, usr,
	).Scan(&symID, &resp.Name, &resp.Kind, &resp.Linkage, &resp.Signature, &addr, &linkageNamesJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return mcp.NewToolResultError(fmt.Sprintf("no symbol with USR %q", usr)), nil
		}
		return mcp.NewToolResultError("symbol lookup: " + err.Error()), nil
	}
	resp.AddressTaken = addr == 1
	if linkageNamesJSON.Valid && linkageNamesJSON.String != "" {
		var names []string
		if err := json.Unmarshal([]byte(linkageNamesJSON.String), &names); err == nil {
			resp.LinkageNames = names
		}
	}

	defs, err := s.symbolDefinitions(symID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp.Definitions = defs

	targets, resolutions, err := s.symbolTargets(usr, symID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp.Targets = targets
	resp.LinkResolutions = resolutions

	return jsonResult(resp)
}

func (s *Server) symbolDefinitions(symbolID int64) ([]SymbolDefinition, error) {
	rows, err := s.db.Query(`
		SELECT file, line, IFNULL(decl_file, ''), IFNULL(decl_line, 0),
		       IFNULL(is_weak, 0), IFNULL(linkage_name, '')
		FROM symbol_definitions
		WHERE symbol_id = ?
		ORDER BY file, line`, symbolID)
	if err != nil {
		return nil, fmt.Errorf("definitions: %w", err)
	}
	defer rows.Close()
	var out []SymbolDefinition
	for rows.Next() {
		var d SymbolDefinition
		var file string
		var line int
		var weak int
		if err := rows.Scan(&file, &line, &d.DeclFile, &d.DeclLine, &weak, &d.LinkageName); err != nil {
			return nil, err
		}
		d.IsWeak = weak == 1
		d.Location = Location{Path: file, Line: line}
		s.enrichLocation(&d.Location, true)
		out = append(out, d)
	}
	return out, rows.Err()
}

// symbolTargets returns the sorted list of targets that link this
// symbol (reachable=1) plus a per-target link-resolution summary.
func (s *Server) symbolTargets(usr string, symbolID int64) ([]string, []LinkResolution, error) {
	// Reachability drives the "which binaries hold this symbol" list.
	rows, err := s.db.Query(`
		SELECT t.name, r.reachable
		FROM symbol_reachability r
		JOIN targets t ON t.id = r.target_id
		WHERE r.symbol_id = ?`, symbolID)
	if err != nil {
		return nil, nil, fmt.Errorf("reachability: %w", err)
	}
	reachByTarget := map[string]bool{}
	for rows.Next() {
		var name string
		var reach int
		if err := rows.Scan(&name, &reach); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if reach == 1 {
			reachByTarget[name] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var targets []string
	for t := range reachByTarget {
		targets = append(targets, t)
	}
	sort.Strings(targets)

	// link_resolutions is per-target too; join in target name.
	lrRows, err := s.db.Query(`
		SELECT t.name, IFNULL(lr.winning_object, ''), IFNULL(lr.linkage_kind, ''),
		       IFNULL(lr.losing_objects, ''), IFNULL(lr.archive, '')
		FROM link_resolutions lr
		JOIN targets t ON t.id = lr.target_id
		WHERE lr.usr = ?
		ORDER BY t.name`, usr)
	if err != nil {
		return targets, nil, fmt.Errorf("link_resolutions: %w", err)
	}
	defer lrRows.Close()
	var lrs []LinkResolution
	for lrRows.Next() {
		var lr LinkResolution
		var losingJSON string
		if err := lrRows.Scan(&lr.Target, &lr.WinningObject, &lr.LinkageKind, &losingJSON, &lr.Archive); err != nil {
			return targets, nil, err
		}
		if losingJSON != "" {
			var losers []string
			if err := json.Unmarshal([]byte(losingJSON), &losers); err == nil {
				lr.LosingObjects = losers
			}
		}
		lr.Reachable = reachByTarget[lr.Target]
		lrs = append(lrs, lr)
	}
	if err := lrRows.Err(); err != nil {
		return targets, lrs, err
	}
	return targets, lrs, nil
}
