package mcp_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
)

func TestFindSymbolByName(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "find_symbol"
	req.Params.Arguments = map[string]any{"query": "add_ints"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.FindSymbolResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total == 0 {
		t.Fatalf("no hits for add_ints; fts_query=%q", payload.FTSQuery)
	}
	var found bool
	for _, h := range payload.Hits {
		if h.Name == "add_ints" && h.Kind == "function" {
			found = true
			if h.Signature == "" {
				t.Error("add_ints has empty signature")
			}
		}
	}
	if !found {
		t.Errorf("add_ints not in hits: %+v", payload.Hits)
	}
}

func TestFindSymbolByKindFilter(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "find_symbol"
	req.Params.Arguments = map[string]any{
		"query": "g_ops",
		"kind":  "variable",
	}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.FindSymbolResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total == 0 {
		t.Fatalf("no hits for g_ops variable")
	}
	for _, h := range payload.Hits {
		if h.Kind != "variable" {
			t.Errorf("kind filter leaked: got %q", h.Kind)
		}
	}
}

func TestFindSymbolByTargetFilter(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	// hook has a weak def in lib/weak_impl.c + strong override in
	// app1/strong_override.c. Only app1 pulls the strong override; app2
	// pulls the weak fallback. Filtering by target = app2 must still
	// return hook (it's linked in) but scoping by app1 must too — this
	// test just checks the filter runs and returns >0 rows.
	req := mcp.CallToolRequest{}
	req.Params.Name = "find_symbol"
	req.Params.Arguments = map[string]any{
		"query":  "hook",
		"target": "app1",
	}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.FindSymbolResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total == 0 {
		t.Fatal("target=app1 filter dropped hook")
	}
	// Targets field on each hit should include app1.
	for _, h := range payload.Hits {
		if h.Name == "hook" && !slices.Contains(h.Targets, "app1") {
			t.Errorf("hook.Targets = %v, want to include app1", h.Targets)
		}
	}
}

func TestFindSymbolEmptyQuery(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "find_symbol"
	req.Params.Arguments = map[string]any{"query": "!!!"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.FindSymbolResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total != 0 {
		t.Errorf("Total = %d, want 0 for punctuation-only query", payload.Total)
	}
}

func TestDescribeSymbolHook(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	// Resolve USR via find_symbol first.
	freq := mcp.CallToolRequest{}
	freq.Params.Name = "find_symbol"
	freq.Params.Arguments = map[string]any{"query": "hook", "kind": "function"}
	fres, _ := client.CallTool(context.Background(), freq)
	var fp herbmcp.FindSymbolResponse
	if err := json.Unmarshal([]byte(textOf(t, fres)), &fp); err != nil {
		t.Fatalf("find unmarshal: %v", err)
	}
	if fp.Total == 0 {
		t.Fatal("no hook in fixture")
	}
	var usr string
	for _, h := range fp.Hits {
		if h.Name == "hook" {
			usr = h.USR
			break
		}
	}
	if usr == "" {
		t.Fatalf("no hook USR in %+v", fp.Hits)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_symbol"
	req.Params.Arguments = map[string]any{"usr": usr}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.DescribeSymbolResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Name != "hook" {
		t.Errorf("Name = %q, want hook", payload.Name)
	}
	// Multi-def: weak fallback + strong override.
	if len(payload.Definitions) != 2 {
		t.Errorf("Definitions = %d, want 2 (weak + strong)", len(payload.Definitions))
	}
	var strong, weak int
	for _, d := range payload.Definitions {
		if d.IsWeak {
			weak++
		} else {
			strong++
		}
		if d.Location.Path == "" {
			t.Error("Location.Path empty on a def")
		}
	}
	if strong != 1 || weak != 1 {
		t.Errorf("strong=%d weak=%d, want 1/1", strong, weak)
	}
	// Both targets should link hook.
	if !slices.Contains(payload.Targets, "app1") || !slices.Contains(payload.Targets, "app2") {
		t.Errorf("Targets = %v, want to include app1 + app2", payload.Targets)
	}
	// link_resolutions has per-target rows too.
	if len(payload.LinkResolutions) < 2 {
		t.Errorf("LinkResolutions = %d, want ≥2", len(payload.LinkResolutions))
	}
}

func TestDescribeSymbolUnknown(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_symbol"
	req.Params.Arguments = map[string]any{"usr": "c:@F@no_such_symbol"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown USR; got %s", textOf(t, res))
	}
}

func TestDescribeSymbolStaticInline(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	// use_dispatch is a static function in app1/main.c that GCC IPA
	// clones. describe_symbol must surface the clone linkage names.
	freq := mcp.CallToolRequest{}
	freq.Params.Name = "find_symbol"
	freq.Params.Arguments = map[string]any{"query": "use_dispatch"}
	fres, _ := client.CallTool(context.Background(), freq)
	var fp herbmcp.FindSymbolResponse
	if err := json.Unmarshal([]byte(textOf(t, fres)), &fp); err != nil {
		t.Fatalf("find unmarshal: %v", err)
	}
	if fp.Total == 0 {
		t.Fatal("no use_dispatch in fixture")
	}
	usr := fp.Hits[0].USR

	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_symbol"
	req.Params.Arguments = map[string]any{"usr": usr}
	res, _ := client.CallTool(context.Background(), req)
	var payload herbmcp.DescribeSymbolResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	joined := strings.Join(payload.LinkageNames, ",")
	if !strings.Contains(joined, "use_dispatch.constprop") {
		t.Errorf("LinkageNames does not include a constprop clone: %v", payload.LinkageNames)
	}
}
