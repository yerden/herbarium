package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
)

// symbolUSR is a small helper used across call-graph tests: resolve a
// USR by name via find_symbol.
func symbolUSR(t *testing.T, client mcpClient, name string) string {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "find_symbol"
	req.Params.Arguments = map[string]any{"query": name}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("find_symbol %s: %v", name, err)
	}
	var payload herbmcp.FindSymbolResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, h := range payload.Hits {
		if h.Name == name {
			return h.USR
		}
	}
	t.Fatalf("no USR for %q in %+v", name, payload.Hits)
	return ""
}

func TestListCallers(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "compute")

	req := mcp.CallToolRequest{}
	req.Params.Name = "list_callers"
	req.Params.Arguments = map[string]any{"callee_usr": usr}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListCallersResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// main → compute is the one cgraph caller in the fixture.
	if payload.Total != 1 || payload.Callers[0].Caller.Name != "main" {
		t.Errorf("Callers = %+v, want [main]", payload.Callers)
	}
	if len(payload.Callers[0].Targets) == 0 {
		t.Errorf("main.Targets empty; want ≥1")
	}
}

func TestListCallees(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "main")

	req := mcp.CallToolRequest{}
	req.Params.Name = "list_callees"
	req.Params.Arguments = map[string]any{"caller_usr": usr}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListCalleesResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// main calls: compute, hook, printf, use_dispatch (cgraph).
	names := map[string]bool{}
	for _, e := range payload.Callees {
		names[e.Callee.Name] = true
	}
	for _, want := range []string{"compute", "hook", "printf", "use_dispatch"} {
		if !names[want] {
			t.Errorf("Callees missing %q; got %v", want, names)
		}
	}
}

func TestListCallPaths(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	from := symbolUSR(t, client, "main")
	to := symbolUSR(t, client, "add_ints")

	req := mcp.CallToolRequest{}
	req.Params.Name = "list_call_paths"
	req.Params.Arguments = map[string]any{"from_usr": from, "to_usr": to}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListCallPathsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Expected path: main → compute → add_ints (length 2 edges).
	if payload.Total == 0 {
		t.Fatal("no paths found")
	}
	var seen bool
	for _, p := range payload.Paths {
		if len(p.Steps) == 3 &&
			p.Steps[0].Name == "main" &&
			p.Steps[1].Name == "compute" &&
			p.Steps[2].Name == "add_ints" {
			seen = true
			break
		}
	}
	if !seen {
		t.Errorf("expected main → compute → add_ints path in %+v", payload.Paths)
	}
}

func TestListCallPathsSelf(t *testing.T) {
	// Trivial start==end case: one path of length 0.
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "main")
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_call_paths"
	req.Params.Arguments = map[string]any{"from_usr": usr, "to_usr": usr}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var payload herbmcp.ListCallPathsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total != 1 || payload.Paths[0].Depth != 0 {
		t.Errorf("self-path = %+v, want single depth-0 path", payload.Paths)
	}
}

func TestListCallPathsUnreachable(t *testing.T) {
	// never_called has no callers; add_ints has no path from never_called.
	client := startClient(t, fixtureHBR(t))
	from := symbolUSR(t, client, "never_called")
	to := symbolUSR(t, client, "add_ints")
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_call_paths"
	req.Params.Arguments = map[string]any{"from_usr": from, "to_usr": to}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var payload herbmcp.ListCallPathsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total != 0 {
		t.Errorf("Total = %d, want 0 (unreachable)", payload.Total)
	}
}

func TestCallerToolsRejectUnknownUSR(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_callers"
	req.Params.Arguments = map[string]any{"callee_usr": "c:@F@nope_no_such_thing"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true; got %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "no symbol") {
		t.Errorf("error message not user-friendly: %s", textOf(t, res))
	}
}

// mcpClient is an alias for the tiny subset of the client interface
// call-graph tests use. Kept private to this file so tests read cleanly.
type mcpClient interface {
	CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
}
