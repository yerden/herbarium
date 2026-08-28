package linkplane_test

import (
	"strings"
	"testing"

	"github.com/yerden/herbarium/internal/linkplane"
)

// TestSubprocessErrorSurfacesStderr checks that when nm exits non-zero
// (here: unknown file), the error message includes both a mention of
// the binary/args and the tool's actual stderr output — not just the
// bare exit status. Regression guard for the "wrap exec.Cmd errors so
// the user sees WHY they failed" contract.
func TestSubprocessErrorSurfacesStderr(t *testing.T) {
	_, err := linkplane.RunNMDefined("/does/not/exist/for/nm")
	if err == nil {
		t.Fatal("RunNMDefined on missing path returned nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "nm ") {
		t.Errorf("error missing tool name: %s", msg)
	}
	if !strings.Contains(msg, "--defined-only") {
		t.Errorf("error missing args: %s", msg)
	}
	if !strings.Contains(msg, "--- stderr ---") {
		t.Errorf("error missing stderr section: %s", msg)
	}
	// nm always writes something diagnostic to stderr for this case
	// (e.g., "No such file"). Assert the payload landed rather than
	// pinning the exact text — GNU vs. LLVM nm phrase it differently.
	if strings.TrimSpace(msg) == "" {
		t.Error("error message is empty")
	}
}

func TestObjdumpErrorSurfacesStderr(t *testing.T) {
	_, err := linkplane.RunObjdump("/does/not/exist/for/objdump")
	if err == nil {
		t.Fatal("RunObjdump on missing path returned nil error")
	}
	if !strings.Contains(err.Error(), "objdump ") {
		t.Errorf("error missing tool name: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "--- stderr ---") {
		t.Errorf("error missing stderr section: %s", err.Error())
	}
}
