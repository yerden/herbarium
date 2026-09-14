package mcp_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
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

// callFindSymbol runs find_symbol and decodes the payload.
func callFindSymbol(t *testing.T, client *mcpclient.Client, args map[string]any) herbmcp.FindSymbolResponse {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "find_symbol"
	req.Params.Arguments = args
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
	return payload
}

// describeSymbolFor runs describe_symbol for one USR.
func describeSymbolFor(t *testing.T, client *mcpclient.Client, usr string) herbmcp.DescribeSymbolResponse {
	t.Helper()
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
	return payload
}

func TestFindSymbolExactIsLiteral(t *testing.T) {
	client := startClient(t, fixtureHBR(t))

	exact := callFindSymbol(t, client, map[string]any{"query": "add_ints", "exact": true})
	if exact.Total == 0 {
		t.Fatalf("exact add_ints found nothing; note=%q", exact.Note)
	}
	for _, h := range exact.Hits {
		if h.Name != "add_ints" {
			t.Errorf("exact mode returned %q, want only add_ints", h.Name)
		}
	}
	if exact.FTSQuery != "" {
		t.Errorf("fts_query = %q, want empty in exact mode", exact.FTSQuery)
	}

	// The point of the mode: a prefix that FTS happily expands must not
	// match anything unless a symbol carries that literal name.
	fuzzy := callFindSymbol(t, client, map[string]any{"query": "add"})
	if fuzzy.Total == 0 {
		t.Fatal("fuzzy 'add' found nothing; fixture assumption broken")
	}
	if got := callFindSymbol(t, client, map[string]any{"query": "add", "exact": true}); got.Total != 0 {
		t.Errorf("exact 'add' returned %d hits, want 0 (fuzzy returns %d)", got.Total, fuzzy.Total)
	}
}

// A clone name appears only in linkage_names — symbols_fts indexes
// symbols.name — so FTS cannot resolve a name read out of objdump or a
// map file. Exact mode is the only route.
func TestFindSymbolExactResolvesCloneLinkageName(t *testing.T) {
	client := startClient(t, fixtureHBR(t))

	// Discover the exact clone name the pinned GCC produced.
	base := callFindSymbol(t, client, map[string]any{"query": "use_dispatch"})
	var cloneName string
	for _, h := range base.Hits {
		if h.Name != "use_dispatch" {
			continue
		}
		d := describeSymbolFor(t, client, h.USR)
		for _, n := range d.LinkageNames {
			if strings.HasPrefix(n, "use_dispatch.constprop") {
				cloneName = n
			}
		}
	}
	if cloneName == "" {
		t.Skip("fixture built without a use_dispatch constprop clone")
	}

	got := callFindSymbol(t, client, map[string]any{"query": cloneName, "exact": true})
	if got.Total == 0 {
		t.Fatalf("exact %q found nothing; note=%q", cloneName, got.Note)
	}
	for _, h := range got.Hits {
		if h.Name != "use_dispatch" {
			t.Errorf("clone lookup returned %q, want the source symbol use_dispatch", h.Name)
		}
	}
}

// The reported failure: an agent searches for a type, gets hits: [], and
// reads it as "absent from the codebase". The note must say which kind
// of nothing this is.
func TestFindSymbolEmptyResultCarriesNote(t *testing.T) {
	client := startClient(t, fixtureHBR(t))

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"fuzzy miss", map[string]any{"query": "no_such_identifier_anywhere"}},
		{"exact miss", map[string]any{"query": "no_such_identifier_anywhere", "exact": true}},
		{"punctuation only", map[string]any{"query": "!!!"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := callFindSymbol(t, client, tc.args)
			if got.Total != 0 {
				t.Fatalf("Total = %d, want 0", got.Total)
			}
			if !strings.Contains(got.Note, "search_source") {
				t.Errorf("note does not point at the source plane: %q", got.Note)
			}
		})
	}

	// A target-scoped miss has the same ambiguity for a different
	// reason, and must say so on top of the base note.
	scoped := callFindSymbol(t, client, map[string]any{"query": "no_such_identifier_anywhere", "target": "app1"})
	if !strings.Contains(scoped.Note, "internal-linkage") {
		t.Errorf("target-scoped note omits the linkage caveat: %q", scoped.Note)
	}
}
