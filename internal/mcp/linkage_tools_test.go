package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
)

func TestDescribeLinkResolution(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "add_ints")
	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_link_resolution"
	req.Params.Arguments = map[string]any{"usr": usr, "target": "app1"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.DescribeLinkResolutionResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.LinkageKind == "" {
		t.Errorf("LinkageKind empty; got %+v", payload)
	}
	if !payload.Reachable {
		t.Errorf("Reachable = false; expected true for add_ints in app1")
	}
}

func TestDescribeLinkResolutionMissing(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	usr := symbolUSR(t, client, "never_called")
	// never_called is not linked into any target's link_resolutions in a
	// static library only used indirectly. If the fixture doesn't have a
	// row, describe_link_resolution should surface a clean error.
	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_link_resolution"
	req.Params.Arguments = map[string]any{"usr": usr, "target": "app2"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	// Accept either a valid resolution or a "no row" error — both are
	// correct behavior depending on how the fixture links.
	if res.IsError {
		if !strings.Contains(textOf(t, res), "no link_resolutions") {
			t.Errorf("error message not user-friendly: %s", textOf(t, res))
		}
	}
}

func TestListWeakSymbols(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_weak_symbols"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListWeakSymbolsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// hook has a weak def in lib/weak_impl.c.
	names := map[string]bool{}
	for _, w := range payload.Symbols {
		names[w.Symbol.Name] = true
		if len(w.Definitions) == 0 {
			t.Errorf("%s: no weak definitions listed", w.Symbol.Name)
		}
	}
	if !names["hook"] {
		t.Errorf("weak symbols missing hook: %v", names)
	}
}

func TestListUndefinedSymbols(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_undefined_symbols"
	req.Params.Arguments = map[string]any{"target": "app1"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListUndefinedSymbolsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	names := map[string]int{}
	for _, u := range payload.Symbols {
		names[u.Symbol.Name] = u.CalledCount
	}
	// printf is called by main via objdump edge and has no def anywhere.
	if _, ok := names["printf"]; !ok {
		t.Errorf("undefined list missing printf; got %v", names)
	}
	// add_ints is defined in lib/shared_utils.c — must NOT appear.
	if _, ok := names["add_ints"]; ok {
		t.Errorf("undefined list should not include add_ints (has def)")
	}
}

func TestListICFGroups(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_icf_groups"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListICFGroupsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// icf_pair.c forces one fold: icf_add_one wins, icf_bump_by_one loses.
	if payload.Total != 1 {
		t.Fatalf("Total = %d, want 1 (icf_pair.c should force one fold); groups=%+v", payload.Total, payload.Groups)
	}
	g := payload.Groups[0]
	if g.Winner.Name != "icf_add_one" {
		t.Errorf("winner = %q, want icf_add_one", g.Winner.Name)
	}
	if len(g.Losers) != 1 || g.Losers[0].Name != "icf_bump_by_one" {
		t.Errorf("losers = %+v, want [icf_bump_by_one]", g.Losers)
	}
	if !strings.Contains(g.ObjectFile, "icf_pair.c.o") {
		t.Errorf("object_file = %q, want to contain icf_pair.c.o", g.ObjectFile)
	}
}

func TestListICFGroupsTargetFilter(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_icf_groups"
	req.Params.Arguments = map[string]any{"target": "app1"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListICFGroupsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// app1 calls icf_add_one directly, so the winner has a link_resolutions
	// row for app1 and the group survives the filter.
	if payload.Total != 1 {
		t.Fatalf("Total = %d, want 1 (app1 links against libshared with the fold)", payload.Total)
	}
}

func TestListUnreachableSymbols(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_unreachable_symbols"
	req.Params.Arguments = map[string]any{"target": "app1"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListUnreachableSymbolsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	names := map[string]bool{}
	for _, u := range payload.Symbols {
		names[u.Symbol.Name] = true
	}
	// use_dispatch is inlined out of app1 → reachable=0.
	// printf is an external ref → reachable=0.
	for _, want := range []string{"use_dispatch", "printf"} {
		if !names[want] {
			t.Errorf("unreachable list missing %q; got %v", want, names)
		}
	}
}

func TestListEntryPoints(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_entry_points"
	req.Params.Arguments = map[string]any{"target": "app1"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListEntryPointsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var hasMain bool
	for _, p := range payload.Points {
		if p.Symbol.Name == "main" && p.Reason == "main" {
			hasMain = true
			break
		}
	}
	if !hasMain {
		t.Errorf("entry points missing main: %+v", payload.Points)
	}
}

// Same payload contract as the inlining tools: these two return sets
// that scale with the project, so they cap and say when they did.
func TestLinkageToolsRowCaps(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	callTool := func(t *testing.T, name string, args map[string]any, out any) {
		t.Helper()
		req := mcp.CallToolRequest{}
		req.Params.Name = name
		req.Params.Arguments = args
		res, err := client.CallTool(context.Background(), req)
		if err != nil {
			t.Fatalf("CallTool %s: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s error: %s", name, textOf(t, res))
		}
		if err := json.Unmarshal([]byte(textOf(t, res)), out); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
	}

	t.Run("list_unreachable_symbols", func(t *testing.T) {
		var full herbmcp.ListUnreachableSymbolsResponse
		callTool(t, "list_unreachable_symbols", map[string]any{"target": "app1"}, &full)
		if full.Truncated {
			t.Fatalf("fixture truncated at the default cap: %d rows", full.Total)
		}
		if full.Total < 2 {
			t.Fatalf("need ≥2 unreachable symbols to exercise the cap; got %d", full.Total)
		}
		var capped herbmcp.ListUnreachableSymbolsResponse
		callTool(t, "list_unreachable_symbols", map[string]any{"target": "app1", "limit": 1}, &capped)
		if len(capped.Symbols) != 1 || !capped.Truncated {
			t.Errorf("limit=1 -> %d rows, truncated=%v", len(capped.Symbols), capped.Truncated)
		}
	})

	t.Run("list_entry_points", func(t *testing.T) {
		var full herbmcp.ListEntryPointsResponse
		callTool(t, "list_entry_points", map[string]any{"target": "app1"}, &full)
		if full.Truncated {
			t.Fatalf("fixture truncated at the default cap: %d rows", full.Total)
		}
		for _, p := range full.Points {
			if p.Location.Snippet != nil {
				t.Fatalf("snippet without include_snippets: %+v", p.Location)
			}
		}
		if full.Total < 2 {
			t.Fatalf("need ≥2 entry points to exercise the cap; got %d", full.Total)
		}
		var capped herbmcp.ListEntryPointsResponse
		callTool(t, "list_entry_points", map[string]any{"target": "app1", "limit": 1}, &capped)
		if len(capped.Points) != 1 || !capped.Truncated {
			t.Errorf("limit=1 -> %d rows, truncated=%v", len(capped.Points), capped.Truncated)
		}

		var withSnip herbmcp.ListEntryPointsResponse
		callTool(t, "list_entry_points", map[string]any{"target": "app1", "include_snippets": true}, &withSnip)
		var sawSnippet bool
		for _, p := range withSnip.Points {
			if p.Location.Snippet != nil {
				sawSnippet = true
			}
		}
		if !sawSnippet {
			t.Error("include_snippets=true returned no snippets")
		}
	})
}
