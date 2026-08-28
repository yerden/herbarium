package mcp_test

import (
	"os"
	"testing"

	"github.com/yerden/herbarium/internal/linkplane"
)

// TestMain silences the linkplane tool-invocation log so building the
// fixture .hbr in-process (via collectForTest) doesn't flood stderr.
func TestMain(m *testing.M) {
	linkplane.SetLogWriter(nil)
	os.Exit(m.Run())
}
