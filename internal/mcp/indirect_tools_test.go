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
	for _, site := range payload.Sites {
		if site.Caller.Name != "use_dispatch" {
			t.Errorf("caller = %q, want use_dispatch", site.Caller.Name)
		}
		if site.Location.Path != "app1/main.c" {
			t.Errorf("location.path = %q, want app1/main.c", site.Location.Path)
		}
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
	// With empty callee_type (current ingest state), candidates fall
	// back to every address-taken function. Both add_ints and mul_ints
	// should appear tagged 'address_taken'.
	names := map[string]string{}
	for _, c := range payload.Candidates {
		names[c.Symbol.Name] = c.Evidence
	}
	for _, want := range []string{"add_ints", "mul_ints"} {
		if _, ok := names[want]; !ok {
			t.Errorf("resolve missing candidate %q", want)
		}
	}
	if payload.Total < 2 {
		t.Errorf("Total = %d, want ≥2", payload.Total)
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
