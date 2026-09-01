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

func TestDescribeInliningCgraphEdges(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "main")
	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_inlining"
	req.Params.Arguments = map[string]any{"caller_usr": usr}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.DescribeInliningResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var inlined, notInlined int
	for _, d := range payload.CgraphEdges {
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
func TestDescribeInliningRecords(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "scaled_compute")
	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_inlining"
	req.Params.Arguments = map[string]any{"caller_usr": usr}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.DescribeInliningResponse
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

// list_inline_instances answers the reverse question for a symbol that has
// no runtime callers because it was folded into its one caller.
func TestListInlineSites(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "scale_by_two")
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_inline_instances"
	req.Params.Arguments = map[string]any{"callee_usr": usr}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListInlineInstancesResponse
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

// explain_call collapses the planes into one verdict. The fixture can
// produce three of the four non-mixed values, each from a different
// mechanism, so they are asserted together.
func TestExplainCall(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	explain := func(t *testing.T, caller, callee string) herbmcp.ExplainCallResponse {
		t.Helper()
		req := mcp.CallToolRequest{}
		req.Params.Name = "explain_call"
		req.Params.Arguments = map[string]any{
			"caller_usr": symbolUSR(t, client, caller),
			"callee_usr": symbolUSR(t, client, callee),
		}
		res, err := client.CallTool(context.Background(), req)
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("error: %s", textOf(t, res))
		}
		var payload herbmcp.ExplainCallResponse
		if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return payload
	}

	t.Run("inlined and present", func(t *testing.T) {
		got := explain(t, "scaled_compute", "scale_by_two")
		if got.Verdict != herbmcp.VerdictInlinedAndPresent {
			t.Errorf("verdict = %q, want %q (%+v)", got.Verdict, herbmcp.VerdictInlinedAndPresent, got.PerObject)
		}
		if len(got.Evidence.Instances) == 0 {
			t.Error("verdict claims a surviving body but cites no instance")
		}
		// The early inliner folded it, so the legacy plane never saw it.
		if got.Evidence.CgraphEdgeInlined != nil && *got.Evidence.CgraphEdgeInlined {
			t.Error("cgraph plane claims to have inlined a pre-IPA fold")
		}
	})

	t.Run("inlined then folded", func(t *testing.T) {
		got := explain(t, "icf_bump_by_one", "icf_add_one")
		if got.Verdict != herbmcp.VerdictInlinedThenFolded {
			t.Errorf("verdict = %q, want %q (%+v)", got.Verdict, herbmcp.VerdictInlinedThenFolded, got.PerObject)
		}
		if len(got.Evidence.Instances) != 0 {
			t.Errorf("verdict says folded away but cites instances: %+v", got.Evidence.Instances)
		}
	})

	t.Run("declined with the compiler's reason", func(t *testing.T) {
		got := explain(t, "compute", "add_ints")
		if got.Verdict != herbmcp.VerdictDeclined {
			t.Fatalf("verdict = %q, want %q (%+v)", got.Verdict, herbmcp.VerdictDeclined, got.PerObject)
		}
		if got.Reason != "function body can be overwritten at link time" {
			t.Errorf("reason = %q", got.Reason)
		}
		// The IPA pass decided last, so its verdict is the one reported.
		if got.PerObject[0].Pass != "inline" {
			t.Errorf("pass = %q, want inline (the later pass)", got.PerObject[0].Pass)
		}
		if !got.Evidence.LinkedEdge {
			t.Error("call was declined, so objdump should still see the edge")
		}
	})

	t.Run("no decision logged", func(t *testing.T) {
		got := explain(t, "never_called", "compute")
		if got.Verdict != herbmcp.VerdictNoDecisionLogged {
			t.Errorf("verdict = %q, want %q", got.Verdict, herbmcp.VerdictNoDecisionLogged)
		}
		if len(got.PerObject) != 0 {
			t.Errorf("per_object = %+v, want empty", got.PerObject)
		}
	})
}
