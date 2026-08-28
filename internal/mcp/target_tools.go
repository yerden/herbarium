package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
)

// registerTargetTools wires list_targets + describe_target.
func (s *Server) registerTargetTools() {
	s.mcp.AddTool(newTool("list_targets",
		mcp.WithDescription(
			"Enumerate every executable, static library, and shared library the "+
				"build produces. Returns kind, source count, and entry-point summary "+
				"per target — the agent's first move when it needs to scope a query "+
				"to a specific binary.",
		),
	), s.handleListTargets)

	s.mcp.AddTool(newTool("describe_target",
		mcp.WithDescription(
			"Full profile of one target: link command, source list (each with "+
				"blob_hash + membership), archives pulled in, and a compact "+
				"entry-point summary. Use list_entry_points for the full root-set list.",
		),
		mcp.WithString("name", mcp.Required(),
			mcp.Description("Target name as returned by list_targets.")),
	), s.handleDescribeTarget)
}

// -- list_targets -----------------------------------------------------

// TargetSummary is one row of list_targets.
type TargetSummary struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	SourceCount int    `json:"source_count"`
	// LinkedSymbols is the number of symbols the linker resolved into
	// this target — a rough size indicator (0 for static_library).
	LinkedSymbols int `json:"linked_symbols"`
}

// ListTargetsResponse is what list_targets returns.
type ListTargetsResponse struct {
	Targets []TargetSummary `json:"targets"`
	Total   int             `json:"total"`
}

func (s *Server) handleListTargets(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rows, err := s.db.Query(`
		SELECT t.name, t.kind,
		       (SELECT COUNT(*) FROM target_sources ts WHERE ts.target_id = t.id) AS srcs,
		       (SELECT COUNT(*) FROM link_resolutions lr WHERE lr.target_id = t.id) AS resolved
		FROM targets t
		ORDER BY t.name`)
	if err != nil {
		return mcp.NewToolResultError("list_targets: " + err.Error()), nil
	}
	defer rows.Close()
	var out []TargetSummary
	for rows.Next() {
		var t TargetSummary
		if err := rows.Scan(&t.Name, &t.Kind, &t.SourceCount, &t.LinkedSymbols); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}
	return jsonResult(ListTargetsResponse{Targets: out, Total: len(out)})
}

// -- describe_target --------------------------------------------------

// TargetSourceEntry is one source file listed under a target, carrying
// its blob hash so the agent can pipe it into read_source without a
// round-trip through list_source_files.
type TargetSourceEntry struct {
	Path     string `json:"path"`
	BlobHash string `json:"blob_hash,omitempty"`
}

// TargetEntryPoint summarizes one root-set symbol for describe_target
// (list_entry_points in a later batch will give the full detail).
type TargetEntryPoint struct {
	USR         string   `json:"usr"`
	Name        string   `json:"name"`
	Signature   string   `json:"signature,omitempty"`
	Location    Location `json:"location"`
	Reason      string   `json:"reason"` // "main" | "exported" | ...
}

// DescribeTargetResponse is what describe_target returns.
type DescribeTargetResponse struct {
	Name          string              `json:"name"`
	Kind          string              `json:"kind"`
	LinkCommand   string              `json:"link_command"`
	Sources       []TargetSourceEntry `json:"sources"`
	Archives      []string            `json:"archives,omitempty"`
	LinkedSymbols int                 `json:"linked_symbols"`
	Reachable     int                 `json:"reachable"`
	EntryPoints   []TargetEntryPoint  `json:"entry_points"`
}

func (s *Server) handleDescribeTarget(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var targetID int64
	var kind, linkCmd string
	if err := s.db.QueryRow(
		`SELECT id, kind, link_command FROM targets WHERE name = ?`, name,
	).Scan(&targetID, &kind, &linkCmd); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return mcp.NewToolResultError(fmt.Sprintf("unknown target %q", name)), nil
		}
		return mcp.NewToolResultError("target lookup: " + err.Error()), nil
	}

	sources, err := s.targetSources(targetID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	archives, err := s.targetArchives(targetID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	entries, err := s.targetEntryPoints(targetID, name)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var linked, reachable int
	_ = s.db.QueryRow(
		`SELECT COUNT(*) FROM link_resolutions WHERE target_id = ?`, targetID,
	).Scan(&linked)
	_ = s.db.QueryRow(
		`SELECT COUNT(*) FROM symbol_reachability WHERE target_id = ? AND reachable = 1`, targetID,
	).Scan(&reachable)

	return jsonResult(DescribeTargetResponse{
		Name:          name,
		Kind:          kind,
		LinkCommand:   linkCmd,
		Sources:       sources,
		Archives:      archives,
		LinkedSymbols: linked,
		Reachable:     reachable,
		EntryPoints:   entries,
	})
}

func (s *Server) targetSources(targetID int64) ([]TargetSourceEntry, error) {
	rows, err := s.db.Query(`
		SELECT ts.file, IFNULL(src.blob_hash, '')
		FROM target_sources ts
		LEFT JOIN sources src ON src.path = ts.file
		WHERE ts.target_id = ?
		ORDER BY ts.file`, targetID)
	if err != nil {
		return nil, fmt.Errorf("target_sources: %w", err)
	}
	defer rows.Close()
	var out []TargetSourceEntry
	for rows.Next() {
		var e TargetSourceEntry
		if err := rows.Scan(&e.Path, &e.BlobHash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// targetArchives returns the distinct set of archive files this target
// pulled objects from (populated by the linker-map pass in Phase 4).
func (s *Server) targetArchives(targetID int64) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT archive
		FROM link_resolutions
		WHERE target_id = ? AND archive != ''
		ORDER BY archive`, targetID)
	if err != nil {
		return nil, fmt.Errorf("archives: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// targetEntryPoints returns a compact root-set: 'main' + any symbol
// linked into the target that is address_taken or externally visible
// AND has no incoming edge inside the target (a rough approximation
// until list_entry_points lands with the full contract).
func (s *Server) targetEntryPoints(targetID int64, name string) ([]TargetEntryPoint, error) {
	// Main is the canonical entry for executables. For libraries there
	// is no main — every exported symbol is an entry candidate, but
	// summarizing that here would drown out the useful signal. Stick
	// to 'main' for the summary; list_entry_points can fill in later.
	rows, err := s.db.Query(`
		SELECT s.usr, s.name, IFNULL(s.signature, ''),
		       IFNULL(sd.file, ''), IFNULL(sd.line, 0)
		FROM symbols s
		JOIN symbol_reachability r ON r.symbol_id = s.id
		LEFT JOIN symbol_definitions sd ON sd.symbol_id = s.id
		WHERE r.target_id = ?
		  AND r.reachable = 1
		  AND s.name = 'main'`, targetID)
	if err != nil {
		return nil, fmt.Errorf("entry-points: %w", err)
	}
	defer rows.Close()
	var seen = map[string]bool{}
	var out []TargetEntryPoint
	for rows.Next() {
		var ep TargetEntryPoint
		var file string
		var line int
		if err := rows.Scan(&ep.USR, &ep.Name, &ep.Signature, &file, &line); err != nil {
			return nil, err
		}
		// The multi-def `main` case means the same symbol yields two
		// rows (one per def file). Return one entry per def so the
		// agent can trace to the right file for this target.
		key := ep.USR + "|" + file
		if seen[key] {
			continue
		}
		seen[key] = true
		ep.Reason = "main"
		if file != "" {
			// Prefer the def whose file is under the target's source list.
			ep.Location = Location{Path: file, Line: line}
			s.enrichLocation(&ep.Location, true)
		}
		out = append(out, ep)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Prefer defs whose file appears in this target's source list so
	// the summary points at the right main for multi-executable projects.
	targetFiles, _ := s.filesForTargetName(name)
	if targetFiles != nil {
		sort.SliceStable(out, func(i, j int) bool {
			return targetFiles[out[i].Location.Path] && !targetFiles[out[j].Location.Path]
		})
	}
	return out, nil
}

// filesForTargetName is filesForTarget minus the "unknown target" error.
func (s *Server) filesForTargetName(name string) (map[string]bool, error) {
	out, err := s.filesForTarget(name)
	if err != nil {
		return nil, nil // filesForTarget only errors on unknown target
	}
	return out, nil
}

