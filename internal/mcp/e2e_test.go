package mcp_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
)

// TestE2EFixtureContract is the plan's "End-to-end test" from Phase 8:
// build the fixture .hbr, open an MCP server, and walk through the tools
// an agent would actually chain to answer real questions. Each assertion
// locks in one behavior the fixture was constructed to exercise, so any
// regression in ingest OR MCP surface trips it.
//
// Covers:
//   - Target enumeration + describe_target sources.
//   - Symbol lookup, multi-def surface (hook = weak fallback + strong
//     override), clone linkage_names (use_dispatch.constprop).
//   - Source-view + runtime-view call graph asymmetry induced by
//     inlining (use_dispatch inlined into main).
//   - Indirect callsite recording under use_dispatch.
//   - Per-target link resolution (hook resolves differently in app1 vs app2).
//   - Reachability: dead-strip (never_called), inlined-away (use_dispatch).
//   - Undefined externals (printf).
func TestE2EFixtureContract(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	ctx := context.Background()

	// --- list_targets ---
	{
		var resp herbmcp.ListTargetsResponse
		call(t, client, ctx, "list_targets", nil, &resp)
		if resp.Total != 3 {
			t.Errorf("list_targets: %d, want 3", resp.Total)
		}
	}

	// --- describe_target app1 has main entry ---
	{
		var resp herbmcp.DescribeTargetResponse
		call(t, client, ctx, "describe_target", map[string]any{"name": "app1"}, &resp)
		var hasMain bool
		for _, ep := range resp.EntryPoints {
			if ep.Name == "main" && ep.Location.Path == "app1/main.c" {
				hasMain = true
			}
		}
		if !hasMain {
			t.Errorf("describe_target(app1): no main entry point in %+v", resp.EntryPoints)
		}
		var seenIcf bool
		for _, s := range resp.Sources {
			if s.Path == "app1/main.c" && s.BlobHash == "" {
				t.Error("app1/main.c missing blob_hash")
			}
			if s.Path == "lib/icf_pair.c" {
				seenIcf = true
			}
		}
		if seenIcf {
			t.Error("describe_target(app1).sources includes lib/icf_pair.c; app1 does not list it directly (came via archive)")
		}
	}

	// --- Resolve hook USR, then describe_symbol(hook) surfaces weak+strong ---
	hookUSR := symbolUSR(t, client, "hook")
	{
		var resp herbmcp.DescribeSymbolResponse
		call(t, client, ctx, "describe_symbol", map[string]any{"usr": hookUSR}, &resp)
		if len(resp.Definitions) != 2 {
			t.Errorf("hook definitions = %d, want 2 (weak fallback + strong override)", len(resp.Definitions))
		}
		var weak, strong int
		for _, d := range resp.Definitions {
			if d.IsWeak {
				weak++
			} else {
				strong++
			}
		}
		if weak != 1 || strong != 1 {
			t.Errorf("hook weak/strong = %d/%d, want 1/1", weak, strong)
		}
		if !slices.Contains(resp.Targets, "app1") || !slices.Contains(resp.Targets, "app2") {
			t.Errorf("hook targets = %v, want [app1 app2]", resp.Targets)
		}
	}

	// --- use_dispatch is a clone whose linkage_names include the constprop variant ---
	useDispatchUSR := symbolUSR(t, client, "use_dispatch")
	{
		var resp herbmcp.DescribeSymbolResponse
		call(t, client, ctx, "describe_symbol", map[string]any{"usr": useDispatchUSR}, &resp)
		var hasClone bool
		for _, n := range resp.LinkageNames {
			if n == "use_dispatch.constprop" || n == "use_dispatch.constprop.0" ||
				(len(n) > len("use_dispatch.constprop") && n[:len("use_dispatch.constprop")] == "use_dispatch.constprop") {
				hasClone = true
				break
			}
		}
		if !hasClone {
			t.Errorf("use_dispatch.linkage_names missing constprop clone: %v", resp.LinkageNames)
		}
	}

	// --- Source-view: main → use_dispatch is a cgraph edge ---
	mainUSR := symbolUSR(t, client, "main")
	{
		var resp herbmcp.ListCalleesResponse
		call(t, client, ctx, "list_callees", map[string]any{"caller_usr": mainUSR}, &resp)
		var hit bool
		for _, e := range resp.Callees {
			if e.Callee.Name == "use_dispatch" {
				hit = true
			}
		}
		if !hit {
			t.Errorf("main cgraph callees missing use_dispatch: %+v", resp.Callees)
		}
	}

	// --- Runtime-view: main → use_dispatch is gone (inlined away in app1) ---
	{
		var resp herbmcp.ListLinkedCalleesResponse
		call(t, client, ctx, "list_linked_callees", map[string]any{"caller_usr": mainUSR, "target": "app1"}, &resp)
		for _, e := range resp.Callees {
			if e.Callee.Name == "use_dispatch" {
				t.Errorf("linked_callees(main, app1) still contains use_dispatch; expected inlined away")
			}
		}
	}

	// --- Inline decision confirms it explicitly ---
	{
		var resp herbmcp.DescribeInlineDecisionsResponse
		call(t, client, ctx, "describe_inline_decisions", map[string]any{"caller_usr": mainUSR}, &resp)
		var sawInlined bool
		for _, d := range resp.Decisions {
			if d.Callee.Name == "use_dispatch" && d.Inlined {
				sawInlined = true
			}
		}
		if !sawInlined {
			t.Errorf("no inlined decision for use_dispatch under main: %+v", resp.Decisions)
		}
	}

	// --- Indirect callsites under use_dispatch ---
	{
		var resp herbmcp.ListIndirectCallSitesResponse
		call(t, client, ctx, "list_indirect_call_sites", map[string]any{"caller_usr": useDispatchUSR}, &resp)
		if resp.Total != 2 {
			t.Errorf("indirect sites under use_dispatch = %d, want 2", resp.Total)
		}
	}

	// --- Per-target link resolution: hook in app1 (strong) vs app2 (weak) ---
	{
		var resp herbmcp.DescribeLinkResolutionResponse
		call(t, client, ctx, "describe_link_resolution", map[string]any{"usr": hookUSR, "target": "app1"}, &resp)
		if resp.LinkageKind == "" {
			t.Errorf("hook@app1: linkage_kind empty")
		}
		if !resp.Reachable {
			t.Errorf("hook@app1: not reachable")
		}
	}

	// --- Reachability: use_dispatch and never_called both = 0 in app1 ---
	{
		var resp herbmcp.ListUnreachableSymbolsResponse
		call(t, client, ctx, "list_unreachable_symbols", map[string]any{"target": "app1"}, &resp)
		names := map[string]bool{}
		for _, u := range resp.Symbols {
			names[u.Symbol.Name] = true
		}
		for _, want := range []string{"use_dispatch", "never_called", "printf"} {
			if !names[want] {
				t.Errorf("unreachable(app1) missing %q; got %v", want, names)
			}
		}
	}

	// --- Undefined externals: printf appears ---
	{
		var resp herbmcp.ListUndefinedSymbolsResponse
		call(t, client, ctx, "list_undefined_symbols", map[string]any{"target": "app1"}, &resp)
		var seen bool
		for _, u := range resp.Symbols {
			if u.Symbol.Name == "printf" {
				seen = true
			}
		}
		if !seen {
			t.Errorf("undefined(app1) missing printf")
		}
	}

	// --- ICF symbols exist as source functions and are found by FTS ---
	{
		var resp herbmcp.FindSymbolResponse
		call(t, client, ctx, "find_symbol", map[string]any{"query": "icf"}, &resp)
		names := map[string]bool{}
		for _, h := range resp.Hits {
			names[h.Name] = true
		}
		for _, want := range []string{"icf_add_one", "icf_bump_by_one"} {
			if !names[want] {
				t.Errorf("find_symbol('icf') missing %q; got %v", want, names)
			}
		}
	}

	// --- Weak-symbols listing includes hook ---
	{
		var resp herbmcp.ListWeakSymbolsResponse
		call(t, client, ctx, "list_weak_symbols", nil, &resp)
		var seen bool
		for _, w := range resp.Symbols {
			if w.Symbol.Name == "hook" {
				seen = true
			}
		}
		if !seen {
			t.Errorf("list_weak_symbols missing hook")
		}
	}
}

// call is an in-test convenience: invoke tool with args, unmarshal
// the text-content payload into out, fail on any error.
func call(t *testing.T, client mcpClient, ctx context.Context, tool string, args map[string]any, out any) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = tool
	if args != nil {
		req.Params.Arguments = args
	}
	res, err := client.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("%s: transport error: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error: %s", tool, textOf(t, res))
	}
	if err := json.Unmarshal([]byte(textOf(t, res)), out); err != nil {
		t.Fatalf("%s: unmarshal: %v", tool, err)
	}
}
