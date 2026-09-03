package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
	"github.com/yerden/herbarium/internal/store"
)

// TestReloadIndexSwapsArtifact is the session-survives-a-re-collect
// contract: an agent that edits code, rebuilds and re-collects over the
// served path gets the new index without dropping its MCP connection.
//
// It also guards the deadlock trap in pinIndex — if reload_index were
// wrapped in the same read lock as every other tool, the write lock it
// takes would never be granted and this test would hang rather than
// fail.
func TestReloadIndexSwapsArtifact(t *testing.T) {
	hbr := fixtureHBR(t)
	client := startClient(t, hbr)

	before := callTargets(t, client)
	if before == 0 {
		t.Fatal("fixture has no targets; nothing to prove a swap against")
	}

	const stamp = "2099-01-02T03:04:05Z"
	swapIn(t, hbr, rebuiltIndex(t, map[string]string{"indexed_at": stamp}))

	var resp herbmcp.ReloadResponse
	callJSON(t, client, "reload_index", nil, &resp)
	if !resp.Changed {
		t.Error("Changed = false, want true — the file on disk carries a new indexed_at")
	}
	if resp.IndexedAt != stamp {
		t.Errorf("IndexedAt = %q, want %q", resp.IndexedAt, stamp)
	}
	if resp.PreviousIndexedAt == "" || resp.PreviousIndexedAt == stamp {
		t.Errorf("PreviousIndexedAt = %q, want the pre-swap stamp", resp.PreviousIndexedAt)
	}
	if resp.SchemaVersion != store.SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", resp.SchemaVersion, store.SchemaVersion)
	}
	if resp.Symbols == 0 || resp.SourceFiles == 0 {
		t.Errorf("counts read back empty: %+v", resp)
	}

	// The same session keeps answering, now from the new handle.
	if after := callTargets(t, client); after != before {
		t.Errorf("targets after reload = %d, want %d", after, before)
	}

	// A second reload with nothing rebuilt in between must say so rather
	// than imply fresh data.
	var again herbmcp.ReloadResponse
	callJSON(t, client, "reload_index", nil, &again)
	if again.Changed {
		t.Error("Changed = true on a reload with no re-collect in between")
	}
}

// TestReloadIndexRefusesSchemaMismatch pins the failure mode that
// matters most: reload is the one tool that can take the whole session
// down, so an unusable file must leave the previous index serving.
func TestReloadIndexRefusesSchemaMismatch(t *testing.T) {
	hbr := fixtureHBR(t)
	client := startClient(t, hbr)

	before := callTargets(t, client)
	swapIn(t, hbr, rebuiltIndex(t, map[string]string{"schema_version": "999"}))

	req := mcp.CallToolRequest{}
	req.Params.Name = "reload_index"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("reload_index accepted a schema_version 999 index")
	}
	if after := callTargets(t, client); after != before {
		t.Errorf("targets after refused reload = %d, want %d (old index must keep serving)", after, before)
	}
}

// TestReloadIndexNeedsIndexPath covers the embedded-server case: a
// caller that constructed the Server from a bare *sql.DB gave us no
// path to reopen, and guessing one would be worse than refusing.
func TestReloadIndexNeedsIndexPath(t *testing.T) {
	db, err := store.OpenReadOnly(fixtureHBR(t))
	if err != nil {
		t.Fatalf("open hbr: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := herbmcp.New(db, herbmcp.Options{Version: "test"}).Reload(); err == nil {
		t.Error("Reload with no IndexPath returned nil error")
	}
}

// rebuiltIndex collects the fixture afresh and overrides the given meta
// keys, standing in for "the agent rebuilt and re-collected".
func rebuiltIndex(t *testing.T, meta map[string]string) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	out := filepath.Join(t.TempDir(), "rebuilt.hbr")
	if code := collectForTest(
		filepath.Join(repo, "testdata", "fixture", "builddir"),
		filepath.Join(repo, "testdata", "fixture"),
		out,
	); code != 0 {
		t.Fatalf("collectForTest exit=%d", code)
	}
	db, err := store.Open(out)
	if err != nil {
		t.Fatalf("reopen rebuilt index: %v", err)
	}
	for k, v := range meta {
		if _, err := db.Exec(`UPDATE meta SET value = ? WHERE key = ?`, v, k); err != nil {
			t.Fatalf("stamp meta[%q]: %v", k, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close rebuilt index: %v", err)
	}
	return out
}

// swapIn does what `collect --replace` does at the end of its run:
// rename the new index over the served path.
func swapIn(t *testing.T, served, fresh string) {
	t.Helper()
	if err := os.Rename(fresh, served); err != nil {
		t.Fatalf("swap index into place: %v", err)
	}
}

// callTargets returns how many targets the session currently sees — a
// cheap probe that the handle behind the tools still works.
func callTargets(t *testing.T, client *mcpclient.Client) int {
	t.Helper()
	var resp struct {
		Targets []json.RawMessage `json:"targets"`
	}
	callJSON(t, client, "list_targets", nil, &resp)
	return len(resp.Targets)
}

func callJSON(t *testing.T, client *mcpclient.Client, name string, args map[string]any, into any) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned error: %+v", name, res.Content)
	}
	body := textOf(t, res)
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("unmarshal %s: %v\n%s", name, err, body)
	}
}

// TestReloadIndexUnderConcurrentQueries is the -race guard for the lock
// discipline: the swap closes the handle other tools are querying, so
// anything less than "the write lock waits every in-flight call out"
// shows up here as a race or a use-after-close.
func TestReloadIndexUnderConcurrentQueries(t *testing.T) {
	hbr := fixtureHBR(t)
	client := startClient(t, hbr)
	fresh := rebuiltIndex(t, nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req := mcp.CallToolRequest{}
				req.Params.Name = "list_targets"
				res, err := client.CallTool(context.Background(), req)
				if err != nil {
					t.Errorf("CallTool(list_targets): %v", err)
					return
				}
				if res.IsError {
					t.Errorf("list_targets returned error: %+v", res.Content)
					return
				}
			}
		}()
	}

	swapIn(t, hbr, fresh)
	var resp herbmcp.ReloadResponse
	callJSON(t, client, "reload_index", nil, &resp)

	close(stop)
	wg.Wait()

	if callTargets(t, client) == 0 {
		t.Error("session lost its targets after a reload under load")
	}
}

// TestReloadIndexMissingFileExplainsItself is the regression for a real
// session: an agent re-collected to the wrong path and got SQLite's
// bare "unable to open database file (14)", which reads as corruption.
// The error has to name the path serve is pinned to and the command
// that writes it, since the caller cannot see either.
func TestReloadIndexMissingFileExplainsItself(t *testing.T) {
	hbr := fixtureHBR(t)
	client := startClient(t, hbr)
	before := callTargets(t, client)

	if err := os.Remove(hbr); err != nil {
		t.Fatalf("remove served index: %v", err)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = "reload_index"
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("reload_index succeeded against a deleted index")
	}
	msg := textOf(t, res)
	for _, want := range []string{hbr, "--replace", "still serving"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message does not mention %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "(14)") {
		t.Errorf("error message leaks the raw SQLite errno instead of diagnosing:\n%s", msg)
	}

	if after := callTargets(t, client); after != before {
		t.Errorf("targets after failed reload = %d, want %d", after, before)
	}
}
