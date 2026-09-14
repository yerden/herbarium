package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

const findTypeLimit = 100

// usedTypesCaveat is the one thing every type tool has to say. DWARF
// records a type only where emitted code uses it, so an empty result is
// not evidence the declaration is absent — the same reached-the-assembler
// property symbols has, and the same failure mode if it goes unstated.
const usedTypesCaveat = "This plane holds types the compiler emitted, " +
	"not types the source declares: DWARF records a type only where some " +
	"variable, parameter, member or signature uses it, so a declared-but-" +
	"unused typedef, struct or enum has no row at any -g level. An empty " +
	"result means 'no translation unit used this type', never 'no such " +
	"type' — fall back to search_source before concluding it does not exist."

// registerTypeTools wires find_type + describe_type.
func (s *Server) registerTypeTools() {
	s.mcp.AddTool(newTool("find_type",
		mcp.WithDescription(
			"Find a type or an enum constant by name. Covers typedefs, struct "+
				"and union tags, enum tags, and — deliberately in the same tool — "+
				"individual enum constants, because an agent looking for something "+
				"like MAX_RETRIES does not know whether it is an enumerator, a "+
				"macro or a type, and guessing wrong is how this search fails. "+
				"Enum constants report the value the compiler assigned. Macros are "+
				"not here at all: they leave no DWARF at the -g level herbarium "+
				"requires, so search_source is the only route to one. "+usedTypesCaveat,
		),
		mcp.WithString("query", mcp.Required(),
			mcp.Description("Identifier or fragment. Tokenized on non-alphanumeric boundaries with prefix matching, exactly like find_symbol — 'op status' and 'op_status' both match 'op_status_t'. With exact=true it is instead the literal name.")),
		mcp.WithBoolean("exact",
			mcp.Description("Match the name literally (types.name = query, or enum_constants.name = query) instead of by FTS. Use to confirm a name rather than to discover one.")),
		mcp.WithString("kind",
			mcp.Description("Filter to one kind: 'typedef', 'struct', 'union', 'enum', or 'enum_constant'. Omit for all five.")),
		limitArg(findTypeLimit),
	), s.handleFindType)

	s.mcp.AddTool(newTool("describe_type",
		mcp.WithDescription(
			"One type in full: kind, declaration location, byte size, and its "+
				"members. For a struct or union, every field in declaration order "+
				"with its rendered type and byte offset — the offset is what "+
				"identifies which field a dispatch table's slot corresponds to. "+
				"For an enum, every constant with the value the compiler assigned. "+
				"For a typedef, the underlying type as rendered by DWARF.",
		),
		mcp.WithString("usr", mcp.Required(),
			mcp.Description("The type USR from find_type.hits[].usr — 'c:<path>@T@name' for a typedef, '@S@'/'@U@'/'@E@' for a struct/union/enum tag.")),
	), s.handleDescribeType)
}

// -- find_type --------------------------------------------------------

// TypeHit is one row of find_type. Kind distinguishes a type row from an
// enum constant, which is why the two can share one result array.
type TypeHit struct {
	USR        string   `json:"usr"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"` // typedef|struct|union|enum|enum_constant
	Location   Location `json:"location"`
	Underlying string   `json:"underlying,omitempty"` // typedef only
	ByteSize   int      `json:"byte_size,omitempty"`
	Value      *int64   `json:"value,omitempty"`     // enum_constant only
	EnumName   string   `json:"enum_name,omitempty"` // enum_constant only
}

// FindTypeResponse is what find_type returns.
type FindTypeResponse struct {
	Query     string    `json:"query"`
	Exact     bool      `json:"exact,omitempty"`
	Hits      []TypeHit `json:"hits"`
	Total     int       `json:"total"`
	Truncated bool      `json:"truncated"`
	Note      string    `json:"note,omitempty"`
}

func (s *Server) handleFindType(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	exact := req.GetBool("exact", false)
	kind := req.GetString("kind", "")
	limit := rowLimit(req, findTypeLimit)

	var hits []TypeHit
	if kind == "" || kind != "enum_constant" {
		rows, err := s.queryTypes(query, exact, kind, limit+1)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		hits = append(hits, rows...)
	}
	if kind == "" || kind == "enum_constant" {
		rows, err := s.queryEnumConstants(query, exact, limit+1)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		hits = append(hits, rows...)
	}

	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}
	for i := range hits {
		s.enrichLocation(&hits[i].Location, false)
	}

	resp := FindTypeResponse{Query: query, Exact: exact, Hits: hits, Total: len(hits), Truncated: truncated}
	if len(hits) == 0 {
		resp.Note = emptyFindTypeNote(query, exact)
	}
	return jsonResult(resp)
}

func (s *Server) queryTypes(query string, exact bool, kind string, limit int) ([]TypeHit, error) {
	var sqlText string
	var args []any
	if exact {
		sqlText = `SELECT t.usr, t.name, t.kind, IFNULL(t.decl_file,''), IFNULL(t.decl_line,0),
			IFNULL(t.underlying,''), IFNULL(t.byte_size,0)
			FROM types t WHERE t.name = ?`
		args = []any{query}
	} else {
		fts := buildFTSQuery(query)
		if fts == "" {
			return nil, nil
		}
		sqlText = `SELECT t.usr, t.name, t.kind, IFNULL(t.decl_file,''), IFNULL(t.decl_line,0),
			IFNULL(t.underlying,''), IFNULL(t.byte_size,0)
			FROM types_fts f JOIN types t ON t.id = f.rowid
			WHERE types_fts MATCH ?`
		args = []any{fts}
	}
	if kind != "" {
		sqlText += ` AND t.kind = ?`
		args = append(args, kind)
	}
	sqlText += ` ORDER BY t.name, t.usr LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("find_type query: %w", err)
	}
	defer rows.Close()
	var out []TypeHit
	for rows.Next() {
		var h TypeHit
		var file string
		var line, size int
		if err := rows.Scan(&h.USR, &h.Name, &h.Kind, &file, &line, &h.Underlying, &size); err != nil {
			return nil, fmt.Errorf("find_type scan: %w", err)
		}
		h.Location = Location{Path: file, Line: line}
		h.ByteSize = size
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Server) queryEnumConstants(query string, exact bool, limit int) ([]TypeHit, error) {
	// Enum constants have no FTS table of their own: there are few of
	// them relative to symbols, and a LIKE prefix scan over an indexed
	// name column is cheaper than a second content-mirror to maintain.
	sqlText := `SELECT c.usr, c.name, c.value, IFNULL(t.name,''), IFNULL(t.decl_file,''), IFNULL(t.decl_line,0)
		FROM enum_constants c LEFT JOIN types t ON t.id = c.type_id
		WHERE `
	var args []any
	if exact {
		sqlText += `c.name = ?`
		args = []any{query}
	} else {
		sqlText += `c.name LIKE ? ESCAPE '\'`
		args = []any{"%" + escapeLike(query) + "%"}
	}
	sqlText += ` ORDER BY c.name LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("find_type enum query: %w", err)
	}
	defer rows.Close()
	var out []TypeHit
	for rows.Next() {
		var h TypeHit
		var val int64
		var file string
		var line int
		if err := rows.Scan(&h.USR, &h.Name, &val, &h.EnumName, &file, &line); err != nil {
			return nil, fmt.Errorf("find_type enum scan: %w", err)
		}
		h.Kind = "enum_constant"
		h.Value = &val
		h.Location = Location{Path: file, Line: line}
		out = append(out, h)
	}
	return out, rows.Err()
}

// escapeLike neutralises the LIKE wildcards so a query containing % or _
// matches those characters literally. '_' matters in practice: C
// identifiers are full of it, and unescaped it would match any character.
func escapeLike(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(q)
}

func emptyFindTypeNote(query string, exact bool) string {
	var b strings.Builder
	if exact {
		b.WriteString("No type or enum constant is named " + strconv.Quote(query) + ". ")
	} else {
		b.WriteString("No type or enum constant matched. ")
	}
	b.WriteString(usedTypesCaveat)
	if !exact {
		b.WriteString(" For a literal name check rather than a fuzzy one, re-run with exact=true.")
	}
	return b.String()
}

// -- describe_type ----------------------------------------------------

// TypeField is one member of a struct or union.
type TypeField struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	ByteOffset int    `json:"byte_offset"`
}

// EnumConstant is one enumerator with the value the compiler assigned.
type EnumConstant struct {
	USR   string `json:"usr"`
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

// DescribeTypeResponse is what describe_type returns.
type DescribeTypeResponse struct {
	USR        string         `json:"usr"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Location   Location       `json:"location"`
	ByteSize   int            `json:"byte_size,omitempty"`
	Underlying string         `json:"underlying,omitempty"`
	Fields     []TypeField    `json:"fields,omitempty"`
	Constants  []EnumConstant `json:"constants,omitempty"`
}

func (s *Server) handleDescribeType(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	u, err := req.RequireString("usr")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var id int64
	var file string
	var line, size int
	resp := DescribeTypeResponse{USR: u}
	err = s.db.QueryRow(`
		SELECT id, name, kind, IFNULL(decl_file,''), IFNULL(decl_line,0),
		       IFNULL(byte_size,0), IFNULL(underlying,'')
		FROM types WHERE usr = ?`, u,
	).Scan(&id, &resp.Name, &resp.Kind, &file, &line, &size, &resp.Underlying)
	if errors.Is(err, sql.ErrNoRows) {
		return mcp.NewToolResultError(
			"no type with USR " + u + " — find_type returns the USRs this index holds. " +
				usedTypesCaveat), nil
	}
	if err != nil {
		return mcp.NewToolResultError("describe_type: " + err.Error()), nil
	}
	resp.Location = Location{Path: file, Line: line}
	s.enrichLocation(&resp.Location, false)
	resp.ByteSize = size

	fieldRows, err := s.db.Query(
		`SELECT name, type, byte_offset FROM type_fields WHERE type_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return mcp.NewToolResultError("describe_type fields: " + err.Error()), nil
	}
	defer fieldRows.Close()
	for fieldRows.Next() {
		var f TypeField
		if err := fieldRows.Scan(&f.Name, &f.Type, &f.ByteOffset); err != nil {
			return mcp.NewToolResultError("scan field: " + err.Error()), nil
		}
		resp.Fields = append(resp.Fields, f)
	}
	if err := fieldRows.Err(); err != nil {
		return mcp.NewToolResultError("iterate fields: " + err.Error()), nil
	}

	constRows, err := s.db.Query(
		`SELECT usr, name, value FROM enum_constants WHERE type_id = ? ORDER BY value, name`, id)
	if err != nil {
		return mcp.NewToolResultError("describe_type constants: " + err.Error()), nil
	}
	defer constRows.Close()
	for constRows.Next() {
		var c EnumConstant
		if err := constRows.Scan(&c.USR, &c.Name, &c.Value); err != nil {
			return mcp.NewToolResultError("scan constant: " + err.Error()), nil
		}
		resp.Constants = append(resp.Constants, c)
	}
	if err := constRows.Err(); err != nil {
		return mcp.NewToolResultError("iterate constants: " + err.Error()), nil
	}

	return jsonResult(resp)
}
