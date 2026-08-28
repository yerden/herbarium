package mcp_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
	"github.com/yerden/herbarium/internal/store"
)

// startClient boots an in-process MCP client against a fresh server
// wrapping the given .hbr. Panics on infrastructure failures — this is
// test setup, not code under test.
func startClient(t *testing.T, hbrPath string) *mcpclient.Client {
	t.Helper()
	db, err := store.OpenReadOnly(hbrPath)
	if err != nil {
		t.Fatalf("open hbr: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	srv := herbmcp.New(db, herbmcp.Options{Version: "test"})
	client, err := mcpclient.NewInProcessClient(srv.MCP())
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	ctx := context.Background()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "1"}
	if _, err := client.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return client
}

// fixtureHBR builds a fresh .hbr from the checked-in fixture. Every
// batch-1 test runs against the same well-known content.
func fixtureHBR(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")
	out := filepath.Join(t.TempDir(), "test.hbr")
	if code := collectForTest(bdir, proot, out); code != 0 {
		t.Fatalf("collectForTest exit=%d", code)
	}
	return out
}

func TestDescribeSchema(t *testing.T) {
	client := startClient(t, fixtureHBR(t))

	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_schema"

	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %+v", res.Content)
	}

	text := textOf(t, res)
	// Structured payload must round-trip: version, DDL, enum glossary,
	// canonical joins.
	var parsed herbmcp.SchemaResponse
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, text)
	}
	if parsed.SchemaVersion != store.SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", parsed.SchemaVersion, store.SchemaVersion)
	}
	if !strings.Contains(parsed.DDL, "CREATE TABLE symbols") {
		t.Errorf("DDL missing symbols table")
	}
	if !strings.Contains(parsed.DDL, "CREATE TABLE call_edges") {
		t.Errorf("DDL missing call_edges table")
	}
	if len(parsed.Enums) == 0 {
		t.Error("Enums glossary empty")
	}
	if len(parsed.JoinRecipes) == 0 {
		t.Error("JoinRecipes empty")
	}
	// Spot-check one enum and one recipe stay stable.
	if !enumHas(parsed.Enums, "call_edges.source", "compiler_cgraph") {
		t.Error("call_edges.source enum missing compiler_cgraph")
	}
}

func TestSQLQueryHappyPath(t *testing.T) {
	client := startClient(t, fixtureHBR(t))

	// A query the fixture answers deterministically: count symbols by
	// linkage. The fixture's compiler ingest produces >0 rows.
	req := mcp.CallToolRequest{}
	req.Params.Name = "sql_query"
	req.Params.Arguments = map[string]any{
		"sql": "SELECT linkage, COUNT(*) FROM symbols GROUP BY linkage ORDER BY linkage",
	}

	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %s", textOf(t, res))
	}
	var payload herbmcp.SQLResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Columns) != 2 {
		t.Errorf("Columns = %v, want 2", payload.Columns)
	}
	if payload.Rowcount == 0 {
		t.Error("Rowcount = 0, want >0 (fixture has symbols)")
	}
	if payload.Truncated {
		t.Error("Truncated=true on a tiny aggregate query")
	}
	// First column is 'linkage' — a text value that must round-trip as
	// a string, not as an obscure []byte / base64 blob.
	for _, row := range payload.Rows {
		if _, ok := row[0].(string); !ok {
			t.Errorf("row[0] = %T %v, want string", row[0], row[0])
		}
	}
}

func TestSQLQueryRejectsWrite(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "sql_query"
	req.Params.Arguments = map[string]any{
		"sql": "DELETE FROM symbols",
	}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	// Writes must surface as tool-level errors (IsError=true), not as
	// silent no-ops. The driver's query_only pragma + mode=ro do the
	// enforcing; we just verify the error propagates.
	if !res.IsError {
		t.Fatalf("expected IsError=true for write attempt; got: %s", textOf(t, res))
	}
	msg := textOf(t, res)
	if !strings.Contains(strings.ToLower(msg), "readonly") &&
		!strings.Contains(strings.ToLower(msg), "read-only") &&
		!strings.Contains(strings.ToLower(msg), "read only") {
		t.Errorf("error message doesn't cite read-only rejection: %s", msg)
	}
}

func TestSQLQueryLimit(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "sql_query"
	req.Params.Arguments = map[string]any{
		"sql":   "SELECT id FROM symbols ORDER BY id",
		"limit": float64(3),
	}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", textOf(t, res))
	}
	var payload herbmcp.SQLResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Rowcount != 3 {
		t.Errorf("Rowcount = %d, want 3", payload.Rowcount)
	}
	if !payload.Truncated {
		t.Error("Truncated=false, want true (fixture has more than 3 symbols)")
	}
}

// TestToolAnnotationsAreConsistent guards a subtle bug fixed after
// opencode reported "MCP error -32000: Connection closed": mcp.NewTool
// defaults DestructiveHint to true, and WithReadOnlyHintAnnotation(true)
// does NOT clear it. Some MCP clients (opencode included) reject the
// resulting "readOnly=true AND destructive=true" annotation combination
// as invalid and drop the transport. All herbarium tools go through the
// newTool helper which sets readOnly=true, destructive=false,
// openWorld=false. This test verifies every registered tool ends up
// with that shape.
func TestToolAnnotationsAreConsistent(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	list, err := client.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list.Tools) == 0 {
		t.Fatal("no tools registered")
	}
	for _, tool := range list.Tools {
		ann := tool.Annotations
		if ann.ReadOnlyHint == nil || !*ann.ReadOnlyHint {
			t.Errorf("%s: ReadOnlyHint != true", tool.Name)
		}
		if ann.DestructiveHint == nil || *ann.DestructiveHint {
			t.Errorf("%s: DestructiveHint != false (mcp.NewTool default not overridden — use newTool helper)", tool.Name)
		}
		if ann.OpenWorldHint == nil || *ann.OpenWorldHint {
			t.Errorf("%s: OpenWorldHint != false (herbarium tools do not touch external systems)", tool.Name)
		}
	}
}

// TestPromptsAndResourcesAdvertised guards the fix for opencode's
// "MCP error -32000: Connection closed": if the server does not
// advertise the prompt/resource capabilities, clients that blind-poll
// `prompts/list` and `resources/list` immediately after initialize
// receive `-32601 method not found`, which some clients treat as
// fatal. The server must advertise these (empty is fine) and return
// empty lists rather than an error.
func TestPromptsAndResourcesAdvertised(t *testing.T) {
	client := startClient(t, fixtureHBR(t))

	pRes, err := client.ListPrompts(context.Background(), mcp.ListPromptsRequest{})
	if err != nil {
		t.Errorf("ListPrompts returned error: %v (server should advertise empty prompt list)", err)
	} else if len(pRes.Prompts) != 0 {
		t.Errorf("expected 0 prompts, got %d", len(pRes.Prompts))
	}

	rRes, err := client.ListResources(context.Background(), mcp.ListResourcesRequest{})
	if err != nil {
		t.Errorf("ListResources returned error: %v (server should advertise empty resource list)", err)
	} else if len(rRes.Resources) != 0 {
		t.Errorf("expected 0 resources, got %d", len(rRes.Resources))
	}
}

func TestSQLQueryParams(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "sql_query"
	req.Params.Arguments = map[string]any{
		"sql":    "SELECT name, kind FROM symbols WHERE name = ?",
		"params": []any{"main"},
	}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", textOf(t, res))
	}
	var payload herbmcp.SQLResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Rowcount != 1 {
		t.Errorf("Rowcount = %d, want 1 (main is unique per USR)", payload.Rowcount)
	}
	if payload.Rowcount > 0 && payload.Rows[0][0] != "main" {
		t.Errorf("row[0][0] = %v, want main", payload.Rows[0][0])
	}
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("empty content")
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("first content item is %T, want TextContent", res.Content[0])
	}
	return tc.Text
}

func enumHas(enums []herbmcp.SchemaEnum, col, val string) bool {
	for _, e := range enums {
		if e.Column == col && slices.Contains(e.Values, val) {
			return true
		}
	}
	return false
}
