package builddir_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yerden/herbarium/internal/builddir"
)

func fixtureBuilddir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(repo, "testdata", "fixture", "builddir")
}

func TestCrawlFixture(t *testing.T) {
	bd, err := builddir.Crawl(fixtureBuilddir(t))
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}

	// The fixture builds 7 source files across 3 targets. meson-private
	// contains a sanity-check .o that must be skipped.
	if len(bd.Objects) != 7 {
		names := make([]string, len(bd.Objects))
		for i, o := range bd.Objects {
			names[i] = o.Object
		}
		t.Fatalf("Objects count = %d, want 7\nfound: %v", len(bd.Objects), names)
	}

	// meson-private skip: no object under that directory may appear.
	for _, o := range bd.Objects {
		if strings.Contains(o.Object, "/meson-private/") {
			t.Errorf("meson-private not skipped: %s", o.Object)
		}
	}

	// Every .o must have all four IPA dumps + the .ci file.
	for _, o := range bd.Objects {
		if o.CI == "" {
			t.Errorf("%s: missing .ci", o.Object)
		}
		if o.Cgraph == "" {
			t.Errorf("%s: missing .cgraph", o.Object)
		}
		if o.Inline == "" {
			t.Errorf("%s: missing .inline", o.Object)
		}
		if o.Devirt == "" {
			t.Errorf("%s: missing .devirt", o.Object)
		}
		if o.ICF == "" {
			t.Errorf("%s: missing .icf", o.Object)
		}
		// .Preprocessed is only present with -save-temps; assert nothing.
	}

	// Two executables → two linker maps at the top of the builddir.
	if len(bd.LinkerMaps) != 2 {
		t.Errorf("LinkerMaps = %v, want 2 entries", bd.LinkerMaps)
	}
	for _, m := range bd.LinkerMaps {
		if filepath.Dir(m) != bd.Root {
			t.Errorf("linker map %s not at builddir root %s", m, bd.Root)
		}
	}
}

func TestCrawlEmptyDir(t *testing.T) {
	bd, err := builddir.Crawl(t.TempDir())
	if err != nil {
		t.Fatalf("Crawl empty: %v", err)
	}
	if len(bd.Objects) != 0 || len(bd.LinkerMaps) != 0 {
		t.Errorf("empty dir returned %d objects and %d maps", len(bd.Objects), len(bd.LinkerMaps))
	}
}
