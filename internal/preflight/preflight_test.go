package preflight_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yerden/herbarium/internal/builddir"
	"github.com/yerden/herbarium/internal/mesonintrospect"
	"github.com/yerden/herbarium/internal/preflight"
)

func fixtureBuilddir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(repo, "testdata", "fixture", "builddir")
}

func TestCheckHappy(t *testing.T) {
	intro, err := mesonintrospect.Load(fixtureBuilddir(t))
	if err != nil {
		t.Fatalf("mesonintrospect.Load: %v", err)
	}
	bd, err := builddir.Crawl(fixtureBuilddir(t))
	if err != nil {
		t.Fatalf("builddir.Crawl: %v", err)
	}
	r := preflight.Check(intro, bd)
	if !r.Ok {
		t.Errorf("expected Ok, got findings:\n%s", r.FormatUserMessage(bd.Root))
	}
	if r.GCCVersion == "" {
		t.Error("GCCVersion empty in report")
	}
}

// TestCheckMissingCI copies the fixture builddir crawl and then blanks the
// CI slot on every object to simulate a build that forgot
// -fcallgraph-info. The preflight report must name the flag by name.
func TestCheckMissingCI(t *testing.T) {
	intro, err := mesonintrospect.Load(fixtureBuilddir(t))
	if err != nil {
		t.Fatalf("mesonintrospect.Load: %v", err)
	}
	bd, err := builddir.Crawl(fixtureBuilddir(t))
	if err != nil {
		t.Fatalf("builddir.Crawl: %v", err)
	}
	for i := range bd.Objects {
		bd.Objects[i].CI = ""
	}
	r := preflight.Check(intro, bd)
	if r.Ok {
		t.Fatal("expected Ok=false with missing CI, got true")
	}
	var found bool
	for _, f := range r.Findings {
		if f.Kind == preflight.KindMissingCI {
			found = true
			if !strings.Contains(f.FixHint, "-fcallgraph-info") {
				t.Errorf("fix hint does not name -fcallgraph-info: %q", f.FixHint)
			}
		}
	}
	if !found {
		t.Errorf("expected KindMissingCI finding; got %+v", r.Findings)
	}
	msg := r.FormatUserMessage(bd.Root)
	if !strings.Contains(msg, "meson setup") {
		t.Errorf("user message missing meson setup line:\n%s", msg)
	}
	if !strings.Contains(msg, preflight.RecommendedCArgs) {
		t.Errorf("user message missing recommended c_args:\n%s", msg)
	}
}

func TestCheckNoObjects(t *testing.T) {
	intro := &mesonintrospect.Introspection{
		MesonVersion: "1.12.0",
		CCompiler:    mesonintrospect.CompilerInfo{ID: "gcc", Version: "16.2.1"},
	}
	bd := &builddir.BuildDir{Root: t.TempDir()}
	r := preflight.Check(intro, bd)
	if r.Ok {
		t.Fatal("empty builddir should not be Ok")
	}
	if r.Findings[0].Kind != preflight.KindNoTargets {
		t.Errorf("expected first finding NoTargets, got %s", r.Findings[0].Kind)
	}
}

func TestCheckGCCTooOld(t *testing.T) {
	intro, err := mesonintrospect.Load(fixtureBuilddir(t))
	if err != nil {
		t.Fatalf("mesonintrospect.Load: %v", err)
	}
	bd, err := builddir.Crawl(fixtureBuilddir(t))
	if err != nil {
		t.Fatalf("builddir.Crawl: %v", err)
	}
	// Pretend the compiler is GCC 9.
	intro.CCompiler.Version = "9.4.0"
	r := preflight.Check(intro, bd)
	if r.Ok {
		t.Fatal("GCC 9 should fail preflight")
	}
	var found bool
	for _, f := range r.Findings {
		if f.Kind == preflight.KindGCCTooOld {
			found = true
		}
	}
	if !found {
		t.Errorf("expected KindGCCTooOld; got %+v", r.Findings)
	}
}

func TestCheckNoDebugInfo(t *testing.T) {
	// Point preflight at a file that isn't a valid ELF at all (plain
	// text) and confirm the KindNoDebugInfo finding fires. Exercises the
	// error branch where debug/elf can't open the file.
	tmpDir := t.TempDir()
	badObj := filepath.Join(tmpDir, "not-an-elf.o")
	if err := os.WriteFile(badObj, []byte("hello, this is not ELF"), 0o644); err != nil {
		t.Fatalf("write bogus .o: %v", err)
	}
	// Also drop a valid-looking CI/Cgraph so those checks pass and
	// isolate the debug-info branch.
	for _, ext := range []string{".ci", ".000i.cgraph", ".095i.inline", ".090i.devirt", ".089i.icf"} {
		if err := os.WriteFile(filepath.Join(tmpDir, "not-an-elf"+ext), []byte{}, 0o644); err != nil {
			t.Fatalf("touch %s: %v", ext, err)
		}
	}

	// Reuse the crawler so we exercise the same wiring the real command uses.
	bd, err := builddir.Crawl(tmpDir)
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if len(bd.Objects) != 1 {
		t.Fatalf("crawl found %d objects, want 1", len(bd.Objects))
	}
	intro := &mesonintrospect.Introspection{
		CCompiler: mesonintrospect.CompilerInfo{ID: "gcc", Version: "16.2.1"},
	}
	r := preflight.Check(intro, bd)
	if r.Ok {
		t.Fatal("bogus ELF should not pass preflight")
	}
	var found bool
	for _, f := range r.Findings {
		if f.Kind == preflight.KindNoDebugInfo {
			found = true
		}
	}
	if !found {
		t.Errorf("expected KindNoDebugInfo; got %+v", r.Findings)
	}
}
