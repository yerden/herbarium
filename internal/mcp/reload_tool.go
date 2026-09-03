package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/yerden/herbarium/internal/store"
)

// reloadToolName is referenced by pinIndex, which must not wrap this
// tool in the read lock it would then deadlock against.
const reloadToolName = "reload_index"

// registerReloadTool wires reload_index. The tool exists because an
// agent editing code mid-session has no way to restart its own MCP
// server: on the stdio transport the client owns the process lifetime,
// so without this the only route to a fresh index is dropping the
// session.
func (s *Server) registerReloadTool() {
	tool := newTool(reloadToolName,
		mcp.WithDescription(
			"Reopen the .hbr this server is serving, picking up an index rebuilt "+
				"since the session started. herbarium never builds or re-indexes "+
				"anything itself, so the refresh is: `ninja -C <builddir>`, then "+
				"`herbarium collect --builddir <builddir> --project-root <root> "+
				"--out <the served path> --replace`, then this tool. The served "+
				"path is absolute, fixed for the life of the process, and returned "+
				"as `path` here — call this tool first if you need to know where to "+
				"write, and pass that exact value to --out. Do not delete the old "+
				"index before collecting: --replace swaps the new one in atomically, "+
				"and that is what keeps this session answering while the collect "+
				"runs. Queries in flight are unaffected — they finish against the "+
				"old index, and every call after this one sees the new one. "+
				"`changed` is false when the file on disk still carries the "+
				"indexed_at this server already had, which almost always means the "+
				"re-collect did not happen or wrote somewhere else. If the file is "+
				"missing, unreadable, or was written by a herbarium with a different "+
				"schema_version, the reload is refused and the previous index keeps "+
				"serving — a failed reload never costs you the session.",
		),
		mcp.WithIdempotentHintAnnotation(true),
	)
	s.mcp.AddTool(tool, s.handleReload)
}

// ReloadResponse is what reload_index returns. The counts are there so
// an agent can see at a glance that the swap moved something, without a
// follow-up sql_query.
type ReloadResponse struct {
	Path              string `json:"path"`
	SchemaVersion     string `json:"schema_version"`
	IndexedAt         string `json:"indexed_at"`
	PreviousIndexedAt string `json:"previous_indexed_at,omitempty"`
	Changed           bool   `json:"changed"`
	Symbols           int    `json:"symbols"`
	Targets           int    `json:"targets"`
	SourceFiles       int    `json:"source_files"`
}

func (s *Server) handleReload(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := s.Reload()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(resp)
}

// Reload opens Options.IndexPath afresh, validates it, and swaps it in
// as the served index. It is safe to call while tools are running: the
// new handle is opened before the write lock is taken, and the old one
// is closed only once the lock proves no handler still holds it.
//
// Every failure path leaves the previous index in place — a reload that
// cannot produce a usable handle is a no-op, not an outage.
func (s *Server) Reload() (ReloadResponse, error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	if s.opts.IndexPath == "" {
		return ReloadResponse{}, errors.New(
			"reload_index: this server was constructed without an index path, so " +
				"there is nothing to reopen. " + servingNote)
	}
	path := s.opts.IndexPath

	// Diagnose before opening. SQLite answers both "no such file" and
	// "not readable by this user" with a bare "unable to open database
	// file (14)", and the caller cannot even see the path serve is
	// pinned to, so an errno leaves it with nothing to act on.
	if err := store.DiagnosePath(path); err != nil {
		return ReloadResponse{}, fmt.Errorf("reload_index: %w.\n%s", err, collectHint(path))
	}
	next, err := store.OpenReadOnly(path)
	if err != nil {
		return ReloadResponse{}, fmt.Errorf("reload_index: %w\n%s", err, collectHint(path))
	}
	resp, err := describeIndex(next, path)
	if err != nil {
		next.Close()
		return ReloadResponse{}, fmt.Errorf("reload_index: %w (the previous index is still serving)", err)
	}
	resp.PreviousIndexedAt = s.indexedAt()
	resp.Changed = resp.IndexedAt != resp.PreviousIndexedAt

	s.mu.Lock()
	old := s.db
	s.db = next
	s.mu.Unlock()

	// No handler can still be inside a query on `old`: pinIndex holds
	// RLock for the duration of a call, so the Lock above waited them
	// out, and every call admitted since reads the new handle.
	old.Close()
	return resp, nil
}

const servingNote = "The previous index is still serving, so the session is intact."

// collectHint spells out the command that produces the file this server
// is pinned to. serve resolves --hbr to an absolute path at startup and
// never looks anywhere else, so a collect whose --out differs by even a
// relative prefix leaves the reload with nothing to open.
func collectHint(path string) string {
	return "This server serves that exact path and no other, so the re-collect has to target it:\n" +
		"  herbarium collect --builddir <builddir> --project-root <root> --out " + path + " --replace\n" +
		"Do not remove the old index first — --replace swaps the new one in atomically, " +
		"which is what keeps this session answering while the collect runs.\n" + servingNote
}

// indexedAt reads the currently served index's stamp. Takes the read
// lock itself — Reload calls it outside a handler, where pinIndex's
// lock is not held.
func (s *Server) indexedAt() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var v string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key='indexed_at'`).Scan(&v); err != nil {
		return ""
	}
	return v
}

// describeIndex validates a freshly opened handle and reads back the
// summary reload_index reports. The schema check is the important part:
// a herbarium upgrade that bumped store.SchemaVersion must be refused
// here rather than discovered later as a missing-column error inside
// some unrelated tool.
func describeIndex(db *sql.DB, path string) (ReloadResponse, error) {
	resp := ReloadResponse{Path: path}
	if err := db.QueryRow(
		`SELECT value FROM meta WHERE key='schema_version'`,
	).Scan(&resp.SchemaVersion); err != nil {
		return resp, fmt.Errorf("%s does not appear to be a herbarium index: %w", path, err)
	}
	if resp.SchemaVersion != store.SchemaVersion {
		return resp, fmt.Errorf("%s has schema_version %q, this herbarium serves %q; re-collect with a matching herbarium",
			path, resp.SchemaVersion, store.SchemaVersion)
	}
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='indexed_at'`).Scan(&resp.IndexedAt); err != nil {
		return resp, fmt.Errorf("%s: reading indexed_at: %w", path, err)
	}
	for _, c := range []struct {
		query string
		into  *int
	}{
		{`SELECT COUNT(*) FROM symbols`, &resp.Symbols},
		{`SELECT COUNT(*) FROM targets`, &resp.Targets},
		{`SELECT COUNT(*) FROM sources`, &resp.SourceFiles},
	} {
		if err := db.QueryRow(c.query).Scan(c.into); err != nil {
			return resp, fmt.Errorf("%s: %s: %w", path, c.query, err)
		}
	}
	return resp, nil
}
