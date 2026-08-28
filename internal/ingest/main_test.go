package ingest_test

import (
	"os"
	"testing"

	"github.com/yerden/herbarium/internal/linkplane"
)

// TestMain silences the linkplane tool-invocation log so link_test.go's
// runFullIngest helper doesn't flood stderr with `$ nm ...` lines.
func TestMain(m *testing.M) {
	linkplane.SetLogWriter(nil)
	os.Exit(m.Run())
}
