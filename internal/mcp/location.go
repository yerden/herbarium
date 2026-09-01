package mcp

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/mark3labs/mcp-go/mcp"
)

// snippetContext is the ±line window every location-returning tool
// wraps around a hit. Kept small because MCP responses are billed as
// tokens; 5 lines each side captures the immediate scope in almost
// every C body.
const snippetContext = 5

// Location is the uniform shape herbarium-plan.md § Convention
// mandates for every tool response that references a code position.
// Path is always project-relative and guaranteed to exist in the blob
// store — an agent can quote it directly and hand the user a
// file:line reference that also resolves in their live checkout.
type Location struct {
	Path         string   `json:"path"`
	Line         int      `json:"line,omitempty"`
	Column       int      `json:"column,omitempty"`
	BlobHash     string   `json:"blob_hash,omitempty"`
	Snippet      *Snippet `json:"snippet,omitempty"`
	AbsolutePath string   `json:"absolute_path,omitempty"` // only when serve --project-root is set
}

// Conventional row-cap bounds shared by the tools that can return an
// unbounded set. A response the client truncates is worse than a capped
// one: the caller cannot tell what it lost, or that it lost anything.
const rowLimitMax = 2000

// rowLimit reads the conventional `limit` argument.
func rowLimit(req mcp.CallToolRequest, def int) int {
	return clampRange(req.GetInt("limit", def), 1, rowLimitMax)
}

// wantSnippets reads the conventional `include_snippets` argument. Off by
// default everywhere it appears: a ±5-line window roughly doubles a row,
// and read_source fetches context for the one location that matters.
func wantSnippets(req mcp.CallToolRequest) bool {
	return req.GetBool("include_snippets", false)
}

// limitArg and snippetArg declare the two conventional options, kept
// here so the wording stays identical across the tool set.
func limitArg(def int) mcp.ToolOption {
	return mcp.WithNumber("limit",
		mcp.Description(fmt.Sprintf("Cap on returned rows; default %d, max %d. `truncated` says when it bit.", def, rowLimitMax)),
		mcp.Min(1))
}

func snippetArg() mcp.ToolOption {
	return mcp.WithBoolean("include_snippets",
		mcp.Description("Attach a ±5-line source window to each location. Off by default: it roughly doubles the payload, and read_source fetches context for the one location you care about."))
}

// Snippet is the ±snippetContext window returned inside a Location.
type Snippet struct {
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Text      string `json:"text"`
}

// enrichLocation looks up path in the blob store and fills BlobHash,
// Snippet (centered on line), and AbsolutePath (when the server has
// --project-root). Silent no-op when path is empty or has no blob —
// call sites should still return the base Location so the agent knows
// where herbarium thinks the fact came from.
func (s *Server) enrichLocation(loc *Location, includeSnippet bool) {
	if loc.Path == "" {
		return
	}
	if s.opts.ProjectRoot != "" && !filepath.IsAbs(loc.Path) {
		loc.AbsolutePath = filepath.Join(s.opts.ProjectRoot, filepath.FromSlash(loc.Path))
	}

	if loc.BlobHash == "" {
		var hash sql.NullString
		if err := s.db.QueryRow(
			`SELECT blob_hash FROM sources WHERE path = ?`, loc.Path,
		).Scan(&hash); err == nil && hash.Valid {
			loc.BlobHash = hash.String
		}
	}
	if !includeSnippet || loc.Line == 0 || loc.BlobHash == "" {
		return
	}
	content, err := s.readBlob(loc.BlobHash)
	if err != nil {
		// Corrupted index is not fatal for one tool call; the caller
		// still gets a usable Location minus the snippet.
		return
	}
	loc.Snippet = extractSnippet(content, loc.Line, snippetContext)
}

// readBlob fetches and decompresses the blob keyed by hash. Not
// exported because callers should go through readSource / snippet
// helpers rather than dealing with zstd frames themselves.
func (s *Server) readBlob(hash string) ([]byte, error) {
	var compressed []byte
	if err := s.db.QueryRow(
		`SELECT content FROM blobs WHERE hash = ?`, hash,
	).Scan(&compressed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("blob %s not found", hash)
		}
		return nil, fmt.Errorf("blob %s: %w", hash, err)
	}
	dec, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()
	raw, err := io.ReadAll(dec)
	if err != nil {
		return nil, fmt.Errorf("zstd decode %s: %w", hash, err)
	}
	return raw, nil
}

// readSourceByPath returns the raw source content for a path, or an
// error if the blob or the mapping is missing. Dispatch rules:
//   - Absolute paths (leading '/') resolve against external_sources.
//   - Project-relative paths resolve against sources first; on miss,
//     fall through to generated_sources keyed on the same relative path.
//     Fall-through matters when a builddir sits outside --project-root
//     so its build-tree files (config.h etc.) live in generated_sources
//     instead of sources.
func (s *Server) readSourceByPath(path string) ([]byte, string, error) {
	if strings.HasPrefix(path, "/") {
		return s.readByColumn("external_sources", "abs_path", path)
	}
	content, hash, err := s.readByColumn("sources", "path", path)
	if err == nil {
		return content, hash, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}
	// Fall through: might live in generated_sources under the same key.
	content, hash, err2 := s.readByColumn("generated_sources", "builddir_rel", path)
	if err2 != nil {
		if errors.Is(err2, sql.ErrNoRows) {
			return nil, "", fmt.Errorf("no indexed source at path %q (looked in sources and generated_sources)", path)
		}
		return nil, "", err2
	}
	return content, hash, nil
}

// readByColumn is a small helper that fetches a blob_hash from any of the
// three source tables and decompresses it. Returns sql.ErrNoRows verbatim
// so callers can implement fall-through.
func (s *Server) readByColumn(table, keyCol, key string) ([]byte, string, error) {
	var hash string
	q := fmt.Sprintf(`SELECT blob_hash FROM %s WHERE %s = ?`, table, keyCol)
	if err := s.db.QueryRow(q, key).Scan(&hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("lookup %s %q: %w", table, key, err)
	}
	content, err := s.readBlob(hash)
	if err != nil {
		return nil, "", err
	}
	return content, hash, nil
}

// extractSnippet returns the ±context window around line (1-based).
// Off-by-one-safe on empty files and past-end line numbers.
func extractSnippet(content []byte, line, context int) *Snippet {
	if line <= 0 {
		return nil
	}
	lines := bytes.Split(content, []byte{'\n'})
	// A trailing newline gives one extra empty entry; drop it so line
	// numbers match the file's own count.
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	start := max(line-context, 1)
	end := min(line+context, len(lines))
	if start > end {
		return nil
	}
	// slice is inclusive on both ends → +1 on end index.
	body := bytes.Join(lines[start-1:end], []byte{'\n'})
	return &Snippet{
		StartLine: start,
		EndLine:   end,
		Text:      string(body),
	}
}

// sliceLines returns lines[start..end] (1-based, inclusive) as text.
// end<=0 means "to end of file"; start<=0 means "start of file". Both
// bounds clamp to the file range. Empty file → empty string.
func sliceLines(content []byte, start, end int) (string, int) {
	lines := bytes.Split(content, []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if total == 0 {
		return "", 0
	}
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > total {
		end = total
	}
	if start > end {
		return "", total
	}
	body := bytes.Join(lines[start-1:end], []byte{'\n'})
	return string(body), total
}
