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

// The records plane must carry what the .cgraph plane structurally
// cannot: the early inliner's always_inline fold, and the rejections
// with GCC's reason.
func TestDescribeInlineDecisionsRecords(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "scaled_compute")
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

	var early, rejected bool
	for _, r := range payload.Records {
		if r.Callee.Name == "scale_by_two" && r.Pass == "einline" && r.Inlined {
			early = true
			if r.Location.Path != "lib/shared_utils.c" || r.Location.Line == 0 {
				t.Errorf("early inline location = %+v", r.Location)
			}
		}
		if r.Callee.Name == "compute" && !r.Inlined {
			if r.Reason == "" {
				t.Error("rejection carries no reason")
			}
			rejected = true
		}
	}
	if !early {
		t.Errorf("no einline success for scale_by_two: %+v", payload.Records)
	}
	if !rejected {
		t.Errorf("no rejection recorded for compute: %+v", payload.Records)
	}

	var instance bool
	for _, i := range payload.Instances {
		if i.Callee.Name == "scale_by_two" && i.Depth == 1 && i.ParentCallee == nil {
			instance = true
		}
	}
	if !instance {
		t.Errorf("no surviving inlined body for scale_by_two: %+v", payload.Instances)
	}
}

// list_inline_sites answers the reverse question for a symbol that has
// no runtime callers because it was folded into its one caller.
func TestListInlineSites(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "scale_by_two")
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_inline_sites"
	req.Params.Arguments = map[string]any{"callee_usr": usr}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListInlineSitesResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Instances) != 1 {
		t.Fatalf("instances = %+v, want 1", payload.Instances)
	}
	if got := payload.Instances[0].Caller.Name; got != "scaled_compute" {
		t.Errorf("inlined into %q, want scaled_compute", got)
	}
	if payload.Instances[0].Object == "" {
		t.Error("instance has no object attribution")
	}
	if len(payload.Records) == 0 {
		t.Error("no decision records for scale_by_two")
	}
}
