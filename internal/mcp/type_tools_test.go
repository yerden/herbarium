package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
)

func callFindType(t *testing.T, client *mcpclient.Client, args map[string]any) herbmcp.FindTypeResponse {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "find_type"
	req.Params.Arguments = args
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var p herbmcp.FindTypeResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return p
}

func callDescribeType(t *testing.T, client *mcpclient.Client, usr string) herbmcp.DescribeTypeResponse {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_type"
	req.Params.Arguments = map[string]any{"usr": usr}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var p herbmcp.DescribeTypeResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return p
}

// The whole point of the plane: an agent searching for an enum constant
// finds it without knowing it is an enumerator rather than a macro or a
// type. This is the query that returned nothing before v10.
func TestFindTypeFindsEnumConstant(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	got := callFindType(t, client, map[string]any{"query": "OP_STATUS_OVERFLOW"})
	if got.Total != 1 {
		t.Fatalf("Total = %d, want 1; note=%q", got.Total, got.Note)
	}
	h := got.Hits[0]
	if h.Kind != "enum_constant" {
		t.Errorf("kind = %q, want enum_constant", h.Kind)
	}
	if h.Value == nil || *h.Value != 7 {
		t.Errorf("value = %v, want 7", h.Value)
	}
	if h.EnumName != "op_status" {
		t.Errorf("enum_name = %q, want op_status", h.EnumName)
	}
	if !strings.HasSuffix(h.Location.Path, "include/dispatch.h") {
		t.Errorf("location = %q, want include/dispatch.h", h.Location.Path)
	}
}

// One query returns every kind that shares a name stem, so an agent that
// does not know what it is looking for still lands on it.
func TestFindTypeSpansKinds(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	got := callFindType(t, client, map[string]any{"query": "op_status"})
	kinds := map[string]string{}
	for _, h := range got.Hits {
		kinds[h.Kind] = h.Name
	}
	for _, want := range []string{"enum", "typedef", "enum_constant"} {
		if _, ok := kinds[want]; !ok {
			t.Errorf("no %s in results: %+v", want, kinds)
		}
	}
	if n := len(got.Hits); n != 5 {
		t.Errorf("hits = %d, want 5 (enum + typedef + 3 constants): %+v", n, kinds)
	}

	if k := callFindType(t, client, map[string]any{"query": "op_status", "kind": "typedef"}); k.Total != 1 {
		t.Errorf("kind=typedef returned %d, want 1", k.Total)
	}
	if k := callFindType(t, client, map[string]any{"query": "op_status", "kind": "enum_constant"}); k.Total != 3 {
		t.Errorf("kind=enum_constant returned %d, want 3", k.Total)
	}
}

func TestFindTypeExact(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	if got := callFindType(t, client, map[string]any{"query": "op_status_t", "exact": true}); got.Total != 1 {
		t.Errorf("exact op_status_t = %d hits, want 1", got.Total)
	}
	// 'op' is a live prefix for the fuzzy search and must not be for exact.
	if fuzzy := callFindType(t, client, map[string]any{"query": "op"}); fuzzy.Total == 0 {
		t.Fatal("fuzzy 'op' found nothing; fixture assumption broken")
	}
	if got := callFindType(t, client, map[string]any{"query": "op", "exact": true}); got.Total != 0 {
		t.Errorf("exact 'op' = %d hits, want 0", got.Total)
	}
}

// An empty result must not read as "no such type" — DWARF only carries
// types some TU used.
func TestFindTypeEmptyNoteNamesTheCaveat(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	got := callFindType(t, client, map[string]any{"query": "no_such_type_anywhere"})
	if got.Total != 0 {
		t.Fatalf("Total = %d, want 0", got.Total)
	}
	for _, want := range []string{"no translation unit used this type", "search_source"} {
		if !strings.Contains(got.Note, want) {
			t.Errorf("note missing %q: %q", want, got.Note)
		}
	}
}

// Field byte offsets are the fact that identifies which slot of a
// dispatch table a call goes through — what GCC's .devirt dump reported
// as "Type:const struct ops, offset 8l" before that plane was removed.
func TestDescribeTypeStructFieldOffsets(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	got := callDescribeType(t, client, "c:include/dispatch.h@S@ops")
	if got.Kind != "struct" {
		t.Errorf("kind = %q, want struct", got.Kind)
	}
	if got.ByteSize != 32 {
		t.Errorf("byte_size = %d, want 32", got.ByteSize)
	}
	want := []herbmcp.TypeField{
		{Name: "add", Type: "int (*)(int, int)", ByteOffset: 0},
		{Name: "mul", Type: "int (*)(int, int)", ByteOffset: 8},
		{Name: "name", Type: "const char *", ByteOffset: 16},
		{Name: "last_status", Type: "op_status_t", ByteOffset: 24},
	}
	if len(got.Fields) != len(want) {
		t.Fatalf("fields = %d, want %d: %+v", len(got.Fields), len(want), got.Fields)
	}
	for i, w := range want {
		if got.Fields[i] != w {
			t.Errorf("field %d = %+v, want %+v", i, got.Fields[i], w)
		}
	}
}

func TestDescribeTypeEnumConstants(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	got := callDescribeType(t, client, "c:include/dispatch.h@E@op_status")
	if got.Kind != "enum" {
		t.Errorf("kind = %q, want enum", got.Kind)
	}
	want := map[string]int64{"OP_STATUS_OK": 0, "OP_STATUS_OVERFLOW": 7, "OP_STATUS_UNSUPPORTED": 9}
	if len(got.Constants) != len(want) {
		t.Fatalf("constants = %d, want %d: %+v", len(got.Constants), len(want), got.Constants)
	}
	for _, c := range got.Constants {
		if v, ok := want[c.Name]; !ok || c.Value != v {
			t.Errorf("%s = %d, want %d (present=%v)", c.Name, c.Value, v, ok)
		}
		if !strings.Contains(c.USR, "@E@op_status@") {
			t.Errorf("%s usr = %q, want the appendix's @E@<enum>@<member> form", c.Name, c.USR)
		}
	}
}

func TestDescribeTypeTypedefUnderlying(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	got := callDescribeType(t, client, "c:include/dispatch.h@T@op_status_t")
	if got.Kind != "typedef" {
		t.Errorf("kind = %q, want typedef", got.Kind)
	}
	if got.Underlying != "enum op_status" {
		t.Errorf("underlying = %q, want \"enum op_status\"", got.Underlying)
	}
}

func TestDescribeTypeUnknownUSRExplainsWhy(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_type"
	req.Params.Arguments = map[string]any{"usr": "c:nope.h@S@nope"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError for unknown USR, got %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "no translation unit used this type") {
		t.Errorf("error does not explain the used-types caveat: %s", textOf(t, res))
	}
}

// A type declared outside --project-root belongs to a system or vendored
// header; indexing those would bury the project's own types under libc's.
func TestTypesExcludeSystemHeaders(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	resp := runSQL(t, client, `SELECT decl_file FROM types WHERE decl_file LIKE '/%' OR decl_file LIKE '%/usr/include/%'`, nil)
	if len(resp.Rows) != 0 {
		t.Errorf("types from outside project-root leaked in: %v", resp.Rows)
	}
	all := runSQL(t, client, `SELECT COUNT(*) FROM types`, nil)
	if len(all.Rows) == 0 {
		t.Fatal("no count returned")
	}
	t.Logf("types indexed: %v", all.Rows[0])
}

// The header is included by several TUs and yields an identical DIE in
// each; the file-scoped USR must collapse them to one row with one set
// of fields, not one per including object.
func TestTypesDedupAcrossTranslationUnits(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	resp := runSQL(t, client, `
		SELECT COUNT(*) FROM types WHERE usr = 'c:include/dispatch.h@S@ops'`, nil)
	if got := resp.Rows[0][0]; toInt(got) != 1 {
		t.Errorf("struct ops rows = %v, want 1", got)
	}
	fields := runSQL(t, client, `
		SELECT COUNT(*) FROM type_fields f
		JOIN types t ON t.id = f.type_id WHERE t.usr = 'c:include/dispatch.h@S@ops'`, nil)
	if got := fields.Rows[0][0]; toInt(got) != 4 {
		t.Errorf("struct ops field rows = %v, want 4", got)
	}
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	}
	return -1
}
