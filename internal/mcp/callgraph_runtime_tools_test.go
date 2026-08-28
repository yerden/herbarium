package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
)

func TestListLinkedCallers(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "compute")
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_linked_callers"
	req.Params.Arguments = map[string]any{"callee_usr": usr, "target": "app1"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListLinkedCallersResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total != 1 || payload.Callers[0].Caller.Name != "main" {
		t.Errorf("Callers = %+v, want [main]", payload.Callers)
	}
}

func TestListLinkedCallees(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "main")
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_linked_callees"
	req.Params.Arguments = map[string]any{"caller_usr": usr, "target": "app1"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListLinkedCalleesResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// main's runtime-view callees in app1 include compute, hook, printf.
	// use_dispatch is inlined out — it must NOT appear in the linked view.
	names := map[string]bool{}
	for _, e := range payload.Callees {
		names[e.Callee.Name] = true
	}
	for _, want := range []string{"compute", "hook", "printf"} {
		if !names[want] {
			t.Errorf("linked callees missing %q; got %v", want, names)
		}
	}
	if names["use_dispatch"] {
		t.Error("use_dispatch appears in linked view; expected it to be inlined away")
	}
}

func TestDescribeInlineDecisions(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "main")
	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_inline_decisions"
	req.Params.Arguments = map[string]any{"caller_usr": usr}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.DescribeInlineDecisionsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var inlined, notInlined int
	for _, d := range payload.Decisions {
		if d.Inlined {
			inlined++
			// use_dispatch is the only one inlined in the fixture.
			if d.Callee.Name != "use_dispatch" {
				t.Errorf("unexpected inlined callee %q", d.Callee.Name)
			}
		} else {
			notInlined++
		}
	}
	if inlined != 1 {
		t.Errorf("inlined count = %d, want 1", inlined)
	}
	if notInlined == 0 {
		t.Error("no not-inlined decisions; expected some (compute, hook, printf)")
	}
}
