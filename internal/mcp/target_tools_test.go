package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
)

func TestListTargets(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_targets"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListTargetsResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The fixture has 3 targets: app1, app2, shared.
	if payload.Total != 3 {
		t.Errorf("Total = %d, want 3", payload.Total)
	}
	byName := map[string]herbmcp.TargetSummary{}
	for _, tt := range payload.Targets {
		byName[tt.Name] = tt
	}
	for _, name := range []string{"app1", "app2", "shared"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("missing target %q in list", name)
		}
	}
	if byName["app1"].Kind != "executable" {
		t.Errorf("app1.kind = %q, want executable", byName["app1"].Kind)
	}
	if byName["shared"].Kind != "static_library" {
		t.Errorf("shared.kind = %q, want static_library", byName["shared"].Kind)
	}
	if byName["app1"].LinkedSymbols == 0 {
		t.Error("app1.LinkedSymbols = 0, want >0")
	}
}

func TestDescribeTarget(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_target"
	req.Params.Arguments = map[string]any{"name": "app1"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.DescribeTargetResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Name != "app1" || payload.Kind != "executable" {
		t.Errorf("Name/Kind = %q/%q, want app1/executable", payload.Name, payload.Kind)
	}
	// Sources should include app1/main.c and app1/strong_override.c;
	// each carries a blob_hash the agent can pipe into read_source.
	seen := map[string]bool{}
	for _, sr := range payload.Sources {
		seen[sr.Path] = true
		if sr.BlobHash == "" {
			t.Errorf("%s: BlobHash empty", sr.Path)
		}
	}
	for _, want := range []string{"app1/main.c", "app1/strong_override.c"} {
		if !seen[want] {
			t.Errorf("Sources missing %q", want)
		}
	}
	// Entry point summary must include the main defined in app1/main.c.
	var hasMain bool
	for _, ep := range payload.EntryPoints {
		if ep.Name == "main" && ep.Location.Path == "app1/main.c" {
			hasMain = true
			if ep.Signature == "" {
				t.Error("main signature empty in EntryPoints")
			}
		}
	}
	if !hasMain {
		t.Errorf("EntryPoints missing main for app1/main.c; got %+v", payload.EntryPoints)
	}
}

func TestDescribeTargetUnknown(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "describe_target"
	req.Params.Arguments = map[string]any{"name": "no_such_target"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown target; got %s", textOf(t, res))
	}
}
