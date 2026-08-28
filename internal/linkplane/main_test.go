package linkplane_test

import (
	"os"
	"testing"

	"github.com/yerden/herbarium/internal/linkplane"
)

// TestMain silences the per-invocation progress log so linkplane tests
// don't spam stderr with dozens of `$ nm ...` lines during `go test`.
// Individual tests that want the log back can call SetLogWriter with
// their own destination inside the test body.
func TestMain(m *testing.M) {
	linkplane.SetLogWriter(nil)
	os.Exit(m.Run())
}
