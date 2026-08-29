package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
	"github.com/yerden/herbarium/internal/store"
)

func TestReadSourceFull(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "read_source"
	req.Params.Arguments = map[string]any{"path": "app1/main.c"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ReadSourceResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Path != "app1/main.c" {
		t.Errorf("Path = %q, want app1/main.c", payload.Path)
	}
	if payload.LineCount == 0 {
		t.Error("LineCount = 0")
	}
	// Content should include something from the fixture — e.g., 'main'.
	if !strings.Contains(payload.Content, "main") {
		t.Errorf("Content does not mention 'main':\n%s", payload.Content)
	}
	if payload.BlobHash == "" {
		t.Error("BlobHash empty")
	}
}

func TestReadSourceSlice(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "read_source"
	req.Params.Arguments = map[string]any{
		"path":       "app1/main.c",
		"start_line": float64(1),
		"end_line":   float64(2),
	}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ReadSourceResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.StartLine != 1 || payload.EndLine != 2 {
		t.Errorf("range = %d..%d, want 1..2", payload.StartLine, payload.EndLine)
	}
	lines := strings.Count(payload.Content, "\n")
	// 2 lines join with 1 newline separator; +0 or +1 acceptable.
	if lines > 2 {
		t.Errorf("more than 2 lines in slice: %s", payload.Content)
	}
}

func TestReadSourceMissing(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "read_source"
	req.Params.Arguments = map[string]any{"path": "no/such/file.c"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true, got: %s", textOf(t, res))
	}
}

func TestListSourceFilesAll(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_source_files"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListSourceFilesResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total != 9 {
		t.Errorf("Total = %d, want 9 (fixture files)", payload.Total)
	}
	// Every file must carry BlobHash and Size.
	for _, f := range payload.Files {
		if f.BlobHash == "" {
			t.Errorf("%s: BlobHash empty", f.Path)
		}
		if f.Size == 0 {
			t.Errorf("%s: Size = 0", f.Path)
		}
	}
}

func TestListSourceFilesFilters(t *testing.T) {
	client := startClient(t, fixtureHBR(t))

	// Filter by target = app1: should include the .c files app1
	// listed as sources, and NOT include app2/main.c.
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_source_files"
	req.Params.Arguments = map[string]any{"target": "app1"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListSourceFilesResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range payload.Files {
		seen[f.Path] = true
	}
	if !seen["app1/main.c"] {
		t.Error("app1 filter missing app1/main.c")
	}
	if seen["app2/main.c"] {
		t.Error("app1 filter includes app2/main.c")
	}

	// Filter by kind = header: should include only .h files.
	req.Params.Arguments = map[string]any{"kind": "header"}
	res, err = client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, f := range payload.Files {
		if !strings.HasSuffix(f.Path, ".h") {
			t.Errorf("kind=header returned non-header %q", f.Path)
		}
	}
	// Fixture has 2 headers (include/dispatch.h, lib/shared_utils.h).
	if payload.Total != 2 {
		t.Errorf("header count = %d, want 2", payload.Total)
	}

	// Filter by path_prefix = "app": only app1/*, app2/*.
	req.Params.Arguments = map[string]any{"path_prefix": "app"}
	res, err = client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, f := range payload.Files {
		if !strings.HasPrefix(f.Path, "app") {
			t.Errorf("path_prefix=app returned %q", f.Path)
		}
	}
}

func TestVerifySourceExpectedHash(t *testing.T) {
	client := startClient(t, fixtureHBR(t))

	// Grab the indexed hash via list_source_files, then feed it back
	// through verify_source. Round-trip must report matches=true.
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_source_files"
	req.Params.Arguments = map[string]any{"path_prefix": "app1/main.c"}
	res, _ := client.CallTool(context.Background(), req)
	var listing herbmcp.ListSourceFilesResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &listing); err != nil {
		t.Fatalf("unmarshal listing: %v", err)
	}
	if len(listing.Files) == 0 {
		t.Fatal("no app1/main.c in listing")
	}
	hash := listing.Files[0].BlobHash

	req.Params.Name = "verify_source"
	req.Params.Arguments = map[string]any{
		"path":          "app1/main.c",
		"expected_hash": hash,
	}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.VerifySourceResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !payload.Matches {
		t.Errorf("Matches = false for round-trip hash %s", hash)
	}
	if payload.Source != "expected_hash" {
		t.Errorf("Source = %q, want expected_hash", payload.Source)
	}

	// Wrong hash → matches=false.
	req.Params.Arguments = map[string]any{
		"path":          "app1/main.c",
		"expected_hash": strings.Repeat("0", 64),
	}
	res, err = client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Matches {
		t.Error("Matches = true for zero-hash; want false")
	}
}

func TestVerifySourceLiveNoProjectRoot(t *testing.T) {
	// Without --project-root, expected_hash omitted must error.
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "verify_source"
	req.Params.Arguments = map[string]any{"path": "app1/main.c"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true when --project-root not set; got %s", textOf(t, res))
	}
}

func TestListSourceDriftClean(t *testing.T) {
	// Serve with --project-root pointing at the real fixture — nothing
	// has drifted, so the drift list must be empty.
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	proot := filepath.Join(repo, "testdata", "fixture")

	hbr := fixtureHBR(t)
	client := startClientWithRoot(t, hbr, proot)
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_source_drift"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListSourceDriftResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Checked == 0 {
		t.Error("Checked = 0")
	}
	if len(payload.Drifted) != 0 {
		t.Errorf("Drifted = %v, want empty", payload.Drifted)
	}
}

func TestListSourceDriftDetects(t *testing.T) {
	// Copy the fixture to a tempdir, run collect against the copy so
	// blobs match, then mutate a file and expect drift to fire.
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	realBd := filepath.Join(repo, "testdata", "fixture", "builddir")

	// Just use the real proot for the initial collect (drift comparison
	// happens against the same live tree). Then mutate one file and
	// restore it via t.Cleanup so parallel test runs stay isolated.
	proot := filepath.Join(repo, "testdata", "fixture")
	target := filepath.Join(proot, "app1", "main.c")
	orig, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	origInfo, _ := os.Stat(target)
	t.Cleanup(func() {
		_ = os.WriteFile(target, orig, origInfo.Mode())
	})

	// Build the .hbr against the untouched fixture.
	out := filepath.Join(t.TempDir(), "test.hbr")
	if code := collectForTest(realBd, proot, out); code != 0 {
		t.Fatalf("collectForTest exit=%d", code)
	}

	// Now mutate the live file.
	if err := os.WriteFile(target, append(orig, []byte("\n// drift\n")...), origInfo.Mode()); err != nil {
		t.Fatalf("mutate: %v", err)
	}

	client := startClientWithRoot(t, out, proot)
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_source_drift"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var payload herbmcp.ListSourceDriftResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Drifted) != 1 || payload.Drifted[0].Path != "app1/main.c" {
		t.Errorf("Drifted = %+v, want exactly app1/main.c", payload.Drifted)
	}
	if payload.Drifted[0].LiveHash == payload.Drifted[0].IndexedHash {
		t.Error("LiveHash == IndexedHash on mutated file")
	}
}

func TestListSourceDriftNoProjectRoot(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_source_drift"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true, got %s", textOf(t, res))
	}
}

// TestReadSourceExternalHeader collects with --include-external '/usr/include/**'
// and asserts that read_source resolves an absolute path against
// external_sources with content that round-trips the on-disk file.
func TestReadSourceExternalHeader(t *testing.T) {
	if _, err := os.Stat("/usr/include/stdio.h"); err != nil {
		t.Skip("host lacks /usr/include/stdio.h; skipping external-header MCP test")
	}
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")
	out := filepath.Join(t.TempDir(), "test.hbr")
	if err := collectForTestWithGlobs(bdir, proot, out, []string{"/usr/include/**"}); err != nil {
		t.Fatalf("collectForTestWithGlobs: %v", err)
	}

	client := startClient(t, out)

	req := mcp.CallToolRequest{}
	req.Params.Name = "read_source"
	req.Params.Arguments = map[string]any{"path": "/usr/include/stdio.h"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("read_source error: %s", textOf(t, res))
	}
	var payload herbmcp.ReadSourceResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Path != "/usr/include/stdio.h" {
		t.Errorf("Path = %q, want /usr/include/stdio.h", payload.Path)
	}
	if payload.BlobHash == "" {
		t.Error("BlobHash empty")
	}
	live, err := os.ReadFile("/usr/include/stdio.h")
	if err != nil {
		t.Fatalf("read live stdio.h: %v", err)
	}
	// sliceLines strips the trailing newline; normalize before comparing.
	liveTrimmed := strings.TrimSuffix(string(live), "\n")
	if payload.Content != liveTrimmed {
		t.Error("read_source content differs from on-disk /usr/include/stdio.h")
	}
}

// TestListSourceFilesIncludeExternal: default excludes external headers;
// include_external=true unions them in as absolute-path entries with no
// target membership.
func TestListSourceFilesIncludeExternal(t *testing.T) {
	if _, err := os.Stat("/usr/include/stdio.h"); err != nil {
		t.Skip("host lacks /usr/include/stdio.h; skipping include_external MCP test")
	}
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	bdir := filepath.Join(repo, "testdata", "fixture", "builddir")
	proot := filepath.Join(repo, "testdata", "fixture")
	out := filepath.Join(t.TempDir(), "test.hbr")
	if err := collectForTestWithGlobs(bdir, proot, out, []string{"/usr/include/**"}); err != nil {
		t.Fatalf("collectForTestWithGlobs: %v", err)
	}
	client := startClient(t, out)

	// Default: no externals in the listing.
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_source_files"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("default CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("default error: %s", textOf(t, res))
	}
	var def herbmcp.ListSourceFilesResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &def); err != nil {
		t.Fatalf("default unmarshal: %v", err)
	}
	for _, f := range def.Files {
		if strings.HasPrefix(f.Path, "/") {
			t.Errorf("default listing leaked external path %q", f.Path)
		}
	}

	// include_external=true: externals appear with absolute paths.
	req2 := mcp.CallToolRequest{}
	req2.Params.Name = "list_source_files"
	req2.Params.Arguments = map[string]any{"include_external": true}
	res2, err := client.CallTool(context.Background(), req2)
	if err != nil {
		t.Fatalf("with-ext CallTool: %v", err)
	}
	if res2.IsError {
		t.Fatalf("with-ext error: %s", textOf(t, res2))
	}
	var ext herbmcp.ListSourceFilesResponse
	if err := json.Unmarshal([]byte(textOf(t, res2)), &ext); err != nil {
		t.Fatalf("with-ext unmarshal: %v", err)
	}
	if ext.Total <= def.Total {
		t.Errorf("include_external total %d not greater than default %d", ext.Total, def.Total)
	}
	var foundStdio bool
	for _, f := range ext.Files {
		if f.Path == "/usr/include/stdio.h" {
			foundStdio = true
			if len(f.Targets) != 0 {
				t.Errorf("external header carries targets: %v", f.Targets)
			}
			if f.IsGenerated {
				t.Error("external header flagged IsGenerated")
			}
		}
	}
	if !foundStdio {
		t.Error("/usr/include/stdio.h missing from include_external listing")
	}
}

// TestReadSourceGeneratedFallThrough packs a synthetic out-of-tree layout
// so a t.Generated file lands in generated_sources, then asserts that
// read_source resolves that file by its builddir-relative key via the
// sources→generated_sources fall-through.
func TestReadSourceGeneratedFallThrough(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "project")
	bdir := filepath.Join(root, "build")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "main.c"), []byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	confBody := []byte("#define GENERATED 1\n#define VERSION \"1.0\"\n")
	if err := os.WriteFile(filepath.Join(bdir, "config.h"), confBody, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "test.hbr")
	if err := runSyntheticSourcesIngest(bdir, proj, out); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	client := startClient(t, out)
	req := mcp.CallToolRequest{}
	req.Params.Name = "read_source"
	req.Params.Arguments = map[string]any{"path": "config.h"}
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("read_source error: %s", textOf(t, res))
	}
	var payload herbmcp.ReadSourceResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Path != "config.h" {
		t.Errorf("Path = %q, want config.h", payload.Path)
	}
	if strings.TrimSuffix(string(confBody), "\n") != payload.Content {
		t.Errorf("content mismatch:\n  got:  %q\n  want: %q",
			payload.Content, strings.TrimSuffix(string(confBody), "\n"))
	}
}

// startClientWithRoot boots an in-process MCP client whose server has
// ProjectRoot set — enables verify_source live-hash and list_source_drift.
func startClientWithRoot(t *testing.T, hbrPath, projectRoot string) *mcpclient.Client {
	t.Helper()
	db, err := store.OpenReadOnly(hbrPath)
	if err != nil {
		t.Fatalf("open hbr: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := herbmcp.New(db, herbmcp.Options{Version: "test", ProjectRoot: projectRoot})
	client, err := mcpclient.NewInProcessClient(srv.MCP())
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	ctx := context.Background()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "1"}
	if _, err := client.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return client
}
