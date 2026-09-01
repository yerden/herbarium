package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
)

func TestListIndirectCallSites(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_indirect_call_sites"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListIndirectCallSitesResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The fixture has 2 indirect sites, both under use_dispatch at
	// app1/main.c:6:13 and 7:13.
	if payload.Total != 2 {
		t.Errorf("Total = %d, want 2", payload.Total)
	}
	// Both dispatch through const struct ops g_ops, so DWARF plus the
	// call instruction's relocation pin the exact member.
	hints := map[string]string{}
	for _, site := range payload.Sites {
		if site.Caller.Name != "use_dispatch" {
			t.Errorf("caller = %q, want use_dispatch", site.Caller.Name)
		}
		if site.Location.Path != "app1/main.c" {
			t.Errorf("location.path = %q, want app1/main.c", site.Location.Path)
		}
		if site.CalleeType != "int (int, int)" {
			t.Errorf("callee_type = %q, want %q", site.CalleeType, "int (int, int)")
		}
		hints[site.FieldHint] = site.Location.Path
	}
	for _, want := range []string{"ops.add", "ops.mul"} {
		if _, ok := hints[want]; !ok {
			t.Errorf("no site with field_hint %q; got %v", want, hints)
		}
	}
}

func TestListIndirectCallSitesTypeFilter(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_indirect_call_sites"
	req.Params.Arguments = map[string]any{"callee_type": "int (int, int)"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var payload herbmcp.ListIndirectCallSitesResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total != 2 {
		t.Errorf("Total = %d, want 2", payload.Total)
	}
}

func TestListIndirectCallSitesCallerFilter(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	// Filter by a caller USR that has no indirect sites (main).
	usr := symbolUSR(t, client, "main")
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_indirect_call_sites"
	req.Params.Arguments = map[string]any{"caller_usr": usr}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var payload herbmcp.ListIndirectCallSitesResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total != 0 {
		t.Errorf("Total = %d, want 0 for main (no indirect sites)", payload.Total)
	}
}

func TestListAddressTakenFunctions(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_address_taken_functions"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListAddressTakenFunctionsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Fixture has add_ints + mul_ints as address-taken via g_ops.
	names := map[string]bool{}
	for _, f := range payload.Functions {
		names[f.Symbol.Name] = true
	}
	for _, want := range []string{"add_ints", "mul_ints"} {
		if !names[want] {
			t.Errorf("address-taken listing missing %q", want)
		}
	}
}

func TestListAddressTakenBySignature(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_address_taken_functions"
	req.Params.Arguments = map[string]any{"fn_ptr_type": "int (int, int)"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var payload herbmcp.ListAddressTakenFunctionsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total < 2 {
		t.Errorf("Total = %d, want ≥2 (add_ints, mul_ints)", payload.Total)
	}
	for _, f := range payload.Functions {
		if f.Symbol.Signature != "int (int, int)" {
			t.Errorf("signature filter leaked: %q", f.Symbol.Signature)
		}
	}
}

func TestResolveIndirectCall(t *testing.T) {
	// Grab a site id from list_indirect_call_sites first.
	client := startClient(t, fixtureHBR(t))
	listReq := mcp.CallToolRequest{}
	listReq.Params.Name = "list_indirect_call_sites"
	listRes, _ := client.CallTool(context.Background(), listReq)
	var listing herbmcp.ListIndirectCallSitesResponse
	if err := json.Unmarshal([]byte(textOf(t, listRes)), &listing); err != nil {
		t.Fatalf("unmarshal listing: %v", err)
	}
	if listing.Total == 0 {
		t.Fatal("no indirect sites in fixture")
	}
	siteID := listing.Sites[0].SiteID

	req := mcp.CallToolRequest{}
	req.Params.Name = "resolve_indirect_call"
	req.Params.Arguments = map[string]any{"site_id": float64(siteID)}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ResolveIndirectCallResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// callee_type is known for this site, so candidates narrow to the
	// address-taken functions with a matching signature — not the whole
	// address-taken pool.
	if payload.CalleeType != "int (int, int)" {
		t.Fatalf("callee_type = %q, want %q", payload.CalleeType, "int (int, int)")
	}
	names := map[string]string{}
	for _, c := range payload.Candidates {
		names[c.Symbol.Name] = c.Evidence
	}
	for _, want := range []string{"add_ints", "mul_ints"} {
		if names[want] != "type_match" {
			t.Errorf("candidate %q evidence = %q, want type_match", want, names[want])
		}
	}
	for name, ev := range names {
		if ev == "address_taken" {
			t.Errorf("candidate %q fell back to the untyped pool", name)
		}
	}
	if payload.Total != 2 {
		t.Errorf("Total = %d, want 2 (add_ints, mul_ints); got %v", payload.Total, names)
	}
}

func TestResolveIndirectCallUnknown(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "resolve_indirect_call"
	req.Params.Arguments = map[string]any{"site_id": float64(999999)}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown site_id; got %s", textOf(t, res))
	}
}

func TestListDevirtHintsEmpty(t *testing.T) {
	// The fixture has no devirt hits (pure C, IPA-devirt rarely fires).
	// Tool must return Total=0 without erroring.
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_devirt_hints"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListDevirtHintsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total != 0 {
		t.Errorf("Total = %d, want 0 for fixture", payload.Total)
	}
}

// Snippets are the dominant per-row cost, and this tool can return one
// row per indirect call in a whole target.
func TestListIndirectCallSitesPayloadLimits(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	call := func(t *testing.T, args map[string]any) herbmcp.ListIndirectCallSitesResponse {
		t.Helper()
		req := mcp.CallToolRequest{}
		req.Params.Name = "list_indirect_call_sites"
		req.Params.Arguments = args
		res, err := client.CallTool(context.Background(), req)
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("error: %s", textOf(t, res))
		}
		var payload herbmcp.ListIndirectCallSitesResponse
		if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return payload
	}

	bare := call(t, map[string]any{})
	if bare.Total < 2 {
		t.Fatalf("need ≥2 sites to exercise the cap; got %d", bare.Total)
	}
	for _, s := range bare.Sites {
		if s.Location.Snippet != nil {
			t.Fatalf("snippet without include_snippets: %+v", s.Location)
		}
	}

	withSnip := call(t, map[string]any{"include_snippets": true})
	var sawSnippet bool
	for _, s := range withSnip.Sites {
		if s.Location.Snippet != nil {
			sawSnippet = true
		}
	}
	if !sawSnippet {
		t.Error("include_snippets=true returned no snippets")
	}

	capped := call(t, map[string]any{"limit": 1})
	if len(capped.Sites) != 1 || !capped.Truncated {
		t.Errorf("limit=1 -> %d sites, truncated=%v", len(capped.Sites), capped.Truncated)
	}
}
