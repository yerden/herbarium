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

// Payload-size contract. An aggressively inlined caller produced enough
// rows to blow an MCP client's output limit and get the whole response
// truncated by the harness, so: snippets are off unless asked for, rows
// are capped, and the summary stays exact when they are.
func TestDescribeInliningPayloadLimits(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "main")
	call := func(t *testing.T, args map[string]any) (herbmcp.DescribeInliningResponse, int) {
		t.Helper()
		args["caller_usr"] = usr
		req := mcp.CallToolRequest{}
		req.Params.Name = "describe_inlining"
		req.Params.Arguments = args
		res, err := client.CallTool(context.Background(), req)
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("error: %s", textOf(t, res))
		}
		body := textOf(t, res)
		var payload herbmcp.DescribeInliningResponse
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return payload, len(body)
	}

	bare, bareBytes := call(t, map[string]any{})
	if bare.Truncated {
		t.Error("fixture response should not be truncated at the default limit")
	}
	for _, r := range bare.Records {
		if r.Location.Snippet != nil {
			t.Fatalf("snippet present without include_snippets: %+v", r.Location)
		}
	}
	for _, i := range bare.Instances {
		if i.Location.Snippet != nil {
			t.Fatalf("snippet present without include_snippets: %+v", i.Location)
		}
	}

	withSnippets, snippetBytes := call(t, map[string]any{"include_snippets": true})
	var sawSnippet bool
	for _, r := range withSnippets.Records {
		if r.Location.Snippet != nil {
			sawSnippet = true
		}
	}
	if !sawSnippet {
		t.Error("include_snippets=true returned no snippets")
	}
	if snippetBytes <= bareBytes {
		t.Errorf("snippets did not grow the payload: %d vs %d", snippetBytes, bareBytes)
	}

	// The cap trims rows; the summary keeps counting all of them.
	capped, _ := call(t, map[string]any{"limit": 1})
	if !capped.Truncated {
		t.Error("limit=1 did not report truncation")
	}
	if len(capped.Records) > 1 || len(capped.Instances) > 1 || len(capped.CgraphEdges) > 1 {
		t.Errorf("limit=1 returned more: %d/%d/%d",
			len(capped.Records), len(capped.Instances), len(capped.CgraphEdges))
	}
	if capped.Summary.Records != bare.Summary.Records || capped.Summary.Records == 0 {
		t.Errorf("summary moved with the cap: %d vs %d", capped.Summary.Records, bare.Summary.Records)
	}
	if capped.Summary.Instances != bare.Summary.Instances {
		t.Errorf("instance summary moved with the cap: %d vs %d", capped.Summary.Instances, bare.Summary.Instances)
	}
}

// The summary is the aggregate an agent would otherwise have to
// reconstruct with a GROUP BY through sql_query.
func TestDescribeInliningSummaryShape(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_inlining"
	req.Params.Arguments = map[string]any{"caller_usr": symbolUSR(t, client, "main")}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var payload herbmcp.DescribeInliningResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s := payload.Summary
	if s.Records != s.RecordsInlined+s.RecordsDeclined {
		t.Errorf("records %d != inlined %d + declined %d", s.Records, s.RecordsInlined, s.RecordsDeclined)
	}
	var byPass int
	for _, n := range s.RecordsByPass {
		byPass += n
	}
	if byPass != s.Records {
		t.Errorf("records_by_pass sums to %d, want %d", byPass, s.Records)
	}
	if s.RecordsByPass["inline"] == 0 {
		t.Errorf("no IPA-pass records counted: %+v", s.RecordsByPass)
	}
	if s.Instances == 0 || s.InstancesByDepth["1"] == 0 {
		t.Errorf("instances_by_depth missing depth 1: %+v", s)
	}
}

// The verdict must never move with the evidence cap: it is decided from
// the full set, and only the echoed rows are trimmed.
func TestExplainCallVerdictSurvivesEvidenceCap(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	verdict := func(t *testing.T, limit any) herbmcp.ExplainCallResponse {
		t.Helper()
		args := map[string]any{
			"caller_usr": symbolUSR(t, client, "compute"),
			"callee_usr": symbolUSR(t, client, "add_ints"),
		}
		if limit != nil {
			args["limit"] = limit
		}
		req := mcp.CallToolRequest{}
		req.Params.Name = "explain_call"
		req.Params.Arguments = args
		res, err := client.CallTool(context.Background(), req)
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		var payload herbmcp.ExplainCallResponse
		if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return payload
	}
	full := verdict(t, nil)
	capped := verdict(t, 1)
	if capped.Verdict != full.Verdict || capped.Reason != full.Reason {
		t.Errorf("verdict moved with the cap: %q/%q vs %q/%q",
			capped.Verdict, capped.Reason, full.Verdict, full.Reason)
	}
	if len(capped.PerObject) != len(full.PerObject) {
		t.Errorf("per_object moved with the cap: %d vs %d", len(capped.PerObject), len(full.PerObject))
	}
	if !capped.Evidence.Truncated {
		t.Error("evidence cap not reported")
	}
}
