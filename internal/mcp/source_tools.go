package mcp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// registerSourceTools wires the four source-reference tools listed in
// herbarium-plan.md § Source reference. list_source_drift and the
// bare-form verify_source live-hash comparison require the server to
// have been launched with --project-root; the tools register
// unconditionally, but return a clear error when the option is missing.
func (s *Server) registerSourceTools() {
	s.mcp.AddTool(newTool("read_source",
		mcp.WithDescription(
			"Return the source content at the given path from the embedded blob store, "+
				"optionally sliced by 1-based inclusive line range. A project-relative "+
				"path resolves against `sources`; an absolute path (starting with '/') "+
				"resolves against `external_sources` (populated only when collect was "+
				"invoked with --include-external). Every path any other tool returns is "+
				"guaranteed to resolve here.",
		),
		mcp.WithString("path", mcp.Required(),
			mcp.Description("Project-relative path (forward slashes) OR absolute path for an external header.")),
		mcp.WithNumber("start_line",
			mcp.Description("1-based first line to include; omit or 0 for start of file."),
			mcp.Min(0)),
		mcp.WithNumber("end_line",
			mcp.Description("1-based last line to include; omit or 0 for end of file."),
			mcp.Min(0)),
	), s.handleReadSource)

	s.mcp.AddTool(newTool("list_source_files",
		mcp.WithDescription(
			"Enumerate every file the .hbr has content for. Filter by target membership, "+
				"path prefix, or file kind ('source' | 'header' | 'generated'). "+
				"Build-tree files from generated_sources (out-of-tree build outputs like "+
				"config.h) are included by default with IsGenerated=true and no target "+
				"membership. External headers packed via --include-external are excluded "+
				"by default; set include_external=true to union them in.",
		),
		mcp.WithString("target",
			mcp.Description("Restrict to files listed as sources of this target (target_sources join). Excludes external headers.")),
		mcp.WithString("path_prefix",
			mcp.Description("Restrict to files whose path starts with this prefix. Applied to both project and external paths.")),
		mcp.WithString("kind",
			mcp.Description("'source' (.c/.C/.cpp/etc.), 'header' (.h/.hpp/.hxx), or 'generated' (is_generated=1).")),
		mcp.WithBoolean("include_external",
			mcp.Description("If true, union external_sources rows into the result. Default false.")),
	), s.handleListSourceFiles)

	s.mcp.AddTool(newTool("verify_source",
		mcp.WithDescription(
			"Check whether a file's indexed content matches an expected hash. "+
				"When expected_hash is supplied, compares against that. Otherwise, if serve was "+
				"launched with --project-root, live-hashes the on-disk file and reports the comparison.",
		),
		mcp.WithString("path", mcp.Required(),
			mcp.Description("Project-relative path.")),
		mcp.WithString("expected_hash",
			mcp.Description("Optional SHA-256 hex to compare against; omit to compare against the live file (requires --project-root).")),
	), s.handleVerifySource)

	s.mcp.AddTool(newTool("list_source_drift",
		mcp.WithDescription(
			"Walk the live checkout at --project-root and return every file whose live content "+
				"differs from the indexed blob. Only available when serve was launched with --project-root.",
		),
		mcp.WithString("target",
			mcp.Description("Restrict to files linked into this target.")),
		mcp.WithString("path_prefix",
			mcp.Description("Restrict to files whose project-relative path starts with this prefix.")),
	), s.handleListSourceDrift)
}

// -- read_source ------------------------------------------------------

// ReadSourceResponse is what read_source returns.
type ReadSourceResponse struct {
	Path      string `json:"path"`
	BlobHash  string `json:"blob_hash"`
	LineCount int    `json:"line_count"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Content   string `json:"content"`
}

func (s *Server) handleReadSource(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	start := req.GetInt("start_line", 0)
	end := req.GetInt("end_line", 0)

	content, hash, err := s.readSourceByPath(path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	body, total := sliceLines(content, start, end)
	resp := ReadSourceResponse{
		Path:      path,
		BlobHash:  hash,
		LineCount: total,
		Content:   body,
	}
	if start > 0 {
		resp.StartLine = start
	}
	if end > 0 {
		resp.EndLine = end
	}
	return jsonResult(resp)
}

// -- list_source_files ------------------------------------------------

// SourceFile is one row of list_source_files.
type SourceFile struct {
	Path        string   `json:"path"`
	BlobHash    string   `json:"blob_hash"`
	Size        int64    `json:"size"`
	IsGenerated bool     `json:"is_generated"`
	Targets     []string `json:"targets,omitempty"` // targets that list this file as a source
}

// ListSourceFilesResponse is what list_source_files returns.
type ListSourceFilesResponse struct {
	Files []SourceFile `json:"files"`
	Total int          `json:"total"`
}

func (s *Server) handleListSourceFiles(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target := req.GetString("target", "")
	prefix := req.GetString("path_prefix", "")
	kind := strings.ToLower(req.GetString("kind", ""))
	includeExternal := req.GetBool("include_external", false)

	// Base query returns every indexed source with its blob size and
	// generated flag; target/prefix/kind are filtered in Go so we can
	// apply the kind rule (extension-based) uniformly.
	rows, err := s.db.Query(`
		SELECT s.path, s.blob_hash, IFNULL(b.size, 0), s.is_generated
		FROM sources s
		LEFT JOIN blobs b ON b.hash = s.blob_hash
		ORDER BY s.path`)
	if err != nil {
		return mcp.NewToolResultError("sources query: " + err.Error()), nil
	}
	defer rows.Close()

	// Precompute target → files if a target filter is set.
	var wantedFiles map[string]bool
	if target != "" {
		f, err := s.filesForTarget(target)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		wantedFiles = f
	}

	// Also load target membership for every file so we can populate
	// SourceFile.Targets without a per-row query.
	targetsByFile, err := s.targetsByFile()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var files []SourceFile
	for rows.Next() {
		var f SourceFile
		var gen int
		if err := rows.Scan(&f.Path, &f.BlobHash, &f.Size, &gen); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		f.IsGenerated = gen == 1
		if prefix != "" && !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		if wantedFiles != nil && !wantedFiles[f.Path] {
			continue
		}
		if !matchesKind(kind, f.Path, f.IsGenerated) {
			continue
		}
		f.Targets = targetsByFile[f.Path]
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}

	// Generated build-tree files: always included when no target filter is
	// set (they have no target membership). Path is builddir-relative;
	// IsGenerated is implicit and set to true for every row.
	if wantedFiles == nil {
		genRows, err := s.db.Query(`
			SELECT gs.builddir_rel, gs.blob_hash, IFNULL(b.size, 0)
			FROM generated_sources gs
			LEFT JOIN blobs b ON b.hash = gs.blob_hash
			ORDER BY gs.builddir_rel`)
		if err != nil {
			return mcp.NewToolResultError("generated_sources query: " + err.Error()), nil
		}
		defer genRows.Close()
		for genRows.Next() {
			f := SourceFile{IsGenerated: true}
			if err := genRows.Scan(&f.Path, &f.BlobHash, &f.Size); err != nil {
				return mcp.NewToolResultError("scan generated: " + err.Error()), nil
			}
			if prefix != "" && !strings.HasPrefix(f.Path, prefix) {
				continue
			}
			if !matchesKind(kind, f.Path, true) {
				continue
			}
			files = append(files, f)
		}
		if err := genRows.Err(); err != nil {
			return mcp.NewToolResultError("iterate generated: " + err.Error()), nil
		}
	}

	// External headers: only when explicitly requested. Target filter
	// short-circuits the union because externals have no target membership.
	if includeExternal && wantedFiles == nil {
		exRows, err := s.db.Query(`
			SELECT es.abs_path, es.blob_hash, IFNULL(b.size, 0)
			FROM external_sources es
			LEFT JOIN blobs b ON b.hash = es.blob_hash
			ORDER BY es.abs_path`)
		if err != nil {
			return mcp.NewToolResultError("external_sources query: " + err.Error()), nil
		}
		defer exRows.Close()
		for exRows.Next() {
			var f SourceFile
			if err := exRows.Scan(&f.Path, &f.BlobHash, &f.Size); err != nil {
				return mcp.NewToolResultError("scan external: " + err.Error()), nil
			}
			if prefix != "" && !strings.HasPrefix(f.Path, prefix) {
				continue
			}
			if !matchesKind(kind, f.Path, false) {
				continue
			}
			files = append(files, f)
		}
		if err := exRows.Err(); err != nil {
			return mcp.NewToolResultError("iterate external: " + err.Error()), nil
		}
	}
	return jsonResult(ListSourceFilesResponse{Files: files, Total: len(files)})
}

// matchesKind resolves the plan's file-kind vocabulary against the
// filename and the is_generated flag. Empty kind means no filter.
func matchesKind(kind, path string, isGenerated bool) bool {
	switch kind {
	case "", "any", "all":
		return true
	case "generated":
		return isGenerated
	case "source":
		return isCSourceExt(path)
	case "header":
		return isHeaderExt(path)
	default:
		return true
	}
}

func isCSourceExt(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".c", ".cc", ".cpp", ".cxx", ".c++", ".m", ".mm":
		return true
	}
	return false
}

func isHeaderExt(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".h", ".hh", ".hpp", ".hxx", ".h++", ".inc":
		return true
	}
	return false
}

// filesForTarget returns the set of source files listed by
// target_sources for a given target name.
func (s *Server) filesForTarget(target string) (map[string]bool, error) {
	rows, err := s.db.Query(`
		SELECT ts.file
		FROM target_sources ts
		JOIN targets t ON t.id = ts.target_id
		WHERE t.name = ?`, target)
	if err != nil {
		return nil, fmt.Errorf("target %q: %w", target, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out[f] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("target %q has no source list (unknown target?)", target)
	}
	return out, nil
}

// targetsByFile pre-joins target_sources → targets so per-row target
// membership is a map lookup, not a per-row SQL round-trip.
func (s *Server) targetsByFile() (map[string][]string, error) {
	rows, err := s.db.Query(`
		SELECT ts.file, t.name
		FROM target_sources ts
		JOIN targets t ON t.id = ts.target_id
		ORDER BY t.name`)
	if err != nil {
		return nil, fmt.Errorf("targets-by-file: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var file, name string
		if err := rows.Scan(&file, &name); err != nil {
			return nil, err
		}
		out[file] = append(out[file], name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// -- verify_source ----------------------------------------------------

// VerifySourceResponse is what verify_source returns.
type VerifySourceResponse struct {
	Path         string `json:"path"`
	IndexedHash  string `json:"indexed_hash"`
	ExpectedHash string `json:"expected_hash,omitempty"`
	LiveHash     string `json:"live_hash,omitempty"`
	Matches      bool   `json:"matches"`
	LiveMissing  bool   `json:"live_missing,omitempty"`
	Source       string `json:"comparison_source"` // "expected_hash" | "live_file"
}

func (s *Server) handleVerifySource(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	expected := req.GetString("expected_hash", "")

	var indexed string
	if err := s.db.QueryRow(
		`SELECT blob_hash FROM sources WHERE path = ?`, path,
	).Scan(&indexed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return mcp.NewToolResultError(fmt.Sprintf("no indexed source at path %q", path)), nil
		}
		return mcp.NewToolResultError("lookup: " + err.Error()), nil
	}

	resp := VerifySourceResponse{Path: path, IndexedHash: indexed}
	if expected != "" {
		resp.ExpectedHash = expected
		resp.Matches = strings.EqualFold(indexed, expected)
		resp.Source = "expected_hash"
		return jsonResult(resp)
	}
	if s.opts.ProjectRoot == "" {
		return mcp.NewToolResultError(
			"verify_source: neither expected_hash nor --project-root is set; " +
				"either supply expected_hash or restart serve with --project-root",
		), nil
	}
	resp.Source = "live_file"
	abs := filepath.Join(s.opts.ProjectRoot, filepath.FromSlash(path))
	data, err := os.ReadFile(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			resp.LiveMissing = true
			return jsonResult(resp)
		}
		return mcp.NewToolResultError("read live " + abs + ": " + err.Error()), nil
	}
	sum := sha256.Sum256(data)
	resp.LiveHash = hex.EncodeToString(sum[:])
	resp.Matches = resp.LiveHash == indexed
	return jsonResult(resp)
}

// -- list_source_drift ------------------------------------------------

// DriftEntry is one row of list_source_drift.
type DriftEntry struct {
	Path        string `json:"path"`
	IndexedHash string `json:"indexed_hash"`
	LiveHash    string `json:"live_hash,omitempty"`
	LiveMissing bool   `json:"live_missing,omitempty"`
}

// ListSourceDriftResponse is what list_source_drift returns.
type ListSourceDriftResponse struct {
	ProjectRoot string       `json:"project_root"`
	Drifted     []DriftEntry `json:"drifted"`
	Checked     int          `json:"checked"`
}

func (s *Server) handleListSourceDrift(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.opts.ProjectRoot == "" {
		return mcp.NewToolResultError(
			"list_source_drift: serve was launched without --project-root; " +
				"drift comparison requires access to the live checkout",
		), nil
	}
	target := req.GetString("target", "")
	prefix := req.GetString("path_prefix", "")

	var wantedFiles map[string]bool
	if target != "" {
		f, err := s.filesForTarget(target)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		wantedFiles = f
	}

	rows, err := s.db.Query(`SELECT path, blob_hash FROM sources ORDER BY path`)
	if err != nil {
		return mcp.NewToolResultError("sources: " + err.Error()), nil
	}
	defer rows.Close()

	resp := ListSourceDriftResponse{ProjectRoot: s.opts.ProjectRoot}
	for rows.Next() {
		var path, indexedHash string
		if err := rows.Scan(&path, &indexedHash); err != nil {
			return mcp.NewToolResultError("scan: " + err.Error()), nil
		}
		if prefix != "" && !strings.HasPrefix(path, prefix) {
			continue
		}
		if wantedFiles != nil && !wantedFiles[path] {
			continue
		}
		resp.Checked++
		abs := filepath.Join(s.opts.ProjectRoot, filepath.FromSlash(path))
		data, err := os.ReadFile(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				resp.Drifted = append(resp.Drifted, DriftEntry{
					Path:        path,
					IndexedHash: indexedHash,
					LiveMissing: true,
				})
				continue
			}
			return mcp.NewToolResultError("read " + abs + ": " + err.Error()), nil
		}
		sum := sha256.Sum256(data)
		live := hex.EncodeToString(sum[:])
		if live != indexedHash {
			resp.Drifted = append(resp.Drifted, DriftEntry{
				Path:        path,
				IndexedHash: indexedHash,
				LiveHash:    live,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("iterate: " + err.Error()), nil
	}
	// Stable ordering — nice for humans reading the raw MCP transcript.
	sort.Slice(resp.Drifted, func(i, j int) bool { return resp.Drifted[i].Path < resp.Drifted[j].Path })
	return jsonResult(resp)
}

// -- shared -----------------------------------------------------------

// jsonResult formats v as pretty JSON and returns a tool result with
// both text (for humans) and structured content (for agents).
func jsonResult(v any) (*mcp.CallToolResult, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("marshal: " + err.Error()), nil
	}
	res := mcp.NewToolResultText(string(body))
	res.StructuredContent = v
	return res, nil
}
