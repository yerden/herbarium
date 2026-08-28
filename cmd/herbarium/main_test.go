package main

import (
	"os"
	"testing"

	"github.com/yerden/herbarium/internal/linkplane"
)

// TestMain silences the linkplane tool-invocation log so TestCollectSmoke
// (which runs collect end-to-end) doesn't flood stderr.
func TestMain(m *testing.M) {
	linkplane.SetLogWriter(nil)
	os.Exit(m.Run())
}
