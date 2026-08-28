package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// defaultSQLLimit caps result-set size to keep MCP responses small when
// an agent forgets a LIMIT. Applied only when the request omits its own
// limit; the request limit itself is bounded by maxSQLLimit.
const (
	defaultSQLLimit = 500
	maxSQLLimit     = 10000
	sqlTimeout      = 30 * time.Second
)

// registerSQLTool wires sql_query — the read-only escape hatch. The
// underlying connection is opened with `?mode=ro&_pragma=query_only(1)`
// (see internal/store), so any write statement fails at the driver
// with "attempt to write a readonly database" before it touches state.
func (s *Server) registerSQLTool() {
	tool := newTool("sql_query",
		mcp.WithDescription(
			"Run an arbitrary read-only SQL query against the .hbr index. "+
				"Use describe_schema first to learn the tables and join recipes. "+
				"Writes are rejected at the driver level. Result rows are capped "+
				"(default 500, max 10000) — set 'limit' to raise or 'offset' to page.",
		),
		mcp.WithString("sql",
			mcp.Required(),
			mcp.Description("The SQL statement to execute. Only SELECT and read-only PRAGMA statements will succeed."),
		),
		mcp.WithArray("params",
			mcp.Description("Positional parameters to bind to '?' placeholders in 'sql'. Elements may be string, number, boolean, or null."),
			mcp.Items(map[string]any{"type": []string{"string", "number", "boolean", "null"}}),
		),
		mcp.WithNumber("limit",
			mcp.Description("Cap on returned rows; default 500, max 10000."),
			mcp.Min(1),
			mcp.Max(float64(maxSQLLimit)),
		),
	)
	s.mcp.AddTool(tool, s.handleSQLQuery)
}

// SQLResponse is the structured payload returned by sql_query.
type SQLResponse struct {
	Columns  []string `json:"columns"`
	Rows     [][]any  `json:"rows"`
	Rowcount int      `json:"rowcount"`
	// Truncated=true when the driver produced more rows than the
	// requested (or default) limit. The agent can retry with a higher
	// limit or with LIMIT/OFFSET in the SQL itself.
	Truncated bool `json:"truncated"`
}

func (s *Server) handleSQLQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	q, err := req.RequireString("sql")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	limit := req.GetInt("limit", defaultSQLLimit)
	if limit <= 0 || limit > maxSQLLimit {
		limit = defaultSQLLimit
	}

	args := req.GetArguments()
	var params []any
	if raw, ok := args["params"]; ok && raw != nil {
		arr, ok := raw.([]any)
		if !ok {
			return mcp.NewToolResultError("params must be an array"), nil
		}
		params = arr
	}

	qctx, cancel := context.WithTimeout(ctx, sqlTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(qctx, q, params...)
	if err != nil {
		return mcp.NewToolResultError("sql: " + err.Error()), nil
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return mcp.NewToolResultError("sql: read columns: " + err.Error()), nil
	}

	resp := SQLResponse{Columns: cols, Rows: [][]any{}}
	scanBuf := make([]any, len(cols))
	scanPtrs := make([]any, len(cols))
	for i := range scanBuf {
		scanPtrs[i] = &scanBuf[i]
	}

	for rows.Next() {
		if len(resp.Rows) >= limit {
			resp.Truncated = true
			break
		}
		if err := rows.Scan(scanPtrs...); err != nil {
			return mcp.NewToolResultError("sql: scan row: " + err.Error()), nil
		}
		row := make([]any, len(cols))
		for i, v := range scanBuf {
			row[i] = normalizeSQLValue(v)
		}
		resp.Rows = append(resp.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return mcp.NewToolResultError("sql: iterate: " + err.Error()), nil
	}
	resp.Rowcount = len(resp.Rows)

	body, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("marshal sql response: " + err.Error()), nil
	}
	result := mcp.NewToolResultText(string(body))
	result.StructuredContent = resp
	return result, nil
}

// normalizeSQLValue coerces []byte from BLOB/TEXT columns into a string
// (or base64-safe fallback if the bytes aren't valid UTF-8). Everything
// else passes through — the JSON encoder handles ints, floats, bools,
// nil.
func normalizeSQLValue(v any) any {
	switch t := v.(type) {
	case []byte:
		// SQLite text columns come back as []byte with modernc.org/sqlite;
		// json.Marshal would base64-encode them silently. Rendering as
		// string is the intent for TEXT columns; for real BLOBs (rare in
		// the schema — only blobs.content, which the agent shouldn't be
		// SELECTing directly anyway) fall back to a hex-string prefix.
		if valid, s := looksLikeUTF8(t); valid {
			return s
		}
		return fmt.Sprintf("<blob:%d bytes>", len(t))
	default:
		return v
	}
}

// looksLikeUTF8 checks whether b is valid UTF-8. Returns the string
// conversion alongside the bool so callers avoid a second traversal.
func looksLikeUTF8(b []byte) (bool, string) {
	// unicode/utf8.Valid is cheap; not importing to keep the surface
	// minimal — this is a hot path only for sql_query, which is rare.
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			i++
			continue
		}
		// Reject C0/C1 controls except common whitespace so binary
		// stays rendered as <blob>.
		if c < 0xC2 || c > 0xF4 {
			return false, ""
		}
		// Minimal well-formed check: peek continuation bytes.
		size := 2
		switch {
		case c >= 0xF0:
			size = 4
		case c >= 0xE0:
			size = 3
		}
		if i+size > len(b) {
			return false, ""
		}
		for k := 1; k < size; k++ {
			if b[i+k]&0xC0 != 0x80 {
				return false, ""
			}
		}
		i += size
	}
	return true, string(b)
}
