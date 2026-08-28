package gccdump_test

import (
	"path/filepath"
	"testing"

	"github.com/yerden/herbarium/internal/gccdump"
)

func TestParseInlineFixture(t *testing.T) {
	d, err := gccdump.ParseInlineFile(filepath.Join(
		repoRoot(t), "testdata", "samples", "gcc-16",
		"app1", "main.c.c.095i.inline",
	))
	if err != nil {
		t.Fatalf("ParseInlineFile: %v", err)
	}
	if len(d.Summaries) == 0 {
		t.Fatal("no summaries parsed")
	}

	// main and use_dispatch.constprop should both be flagged inlinable
	// in the fixture's IPA summaries.
	byID := map[string]gccdump.InlineSummary{}
	for _, s := range d.Summaries {
		byID[s.FunctionLocalID] = s
	}
	main, ok := byID["5"]
	if !ok {
		t.Fatalf("no summary for main/5; ids present: %v", ids(d.Summaries))
	}
	if !main.Inlinable {
		t.Errorf("main not marked inlinable")
	}
	// main.InlinedInto should include local id 10 (use_dispatch.constprop)
	// from the second-wave decision block.
	var hitClone bool
	for _, id := range main.InlinedInto {
		if id == "10" {
			hitClone = true
		}
	}
	if !hitClone {
		t.Errorf("main.InlinedInto missing use_dispatch.constprop/10; got %v", main.InlinedInto)
	}
}

func TestParseICFEmpty(t *testing.T) {
	// app1/main.c has nothing foldable; parser must return zero groups
	// without error — guards the no-op path.
	d, err := gccdump.ParseICFFile(filepath.Join(
		repoRoot(t), "testdata", "samples", "gcc-16",
		"app1", "main.c.c.089i.icf",
	))
	if err != nil {
		t.Fatalf("ParseICFFile: %v", err)
	}
	if len(d.Groups) != 0 {
		t.Errorf("expected zero groups on main.c, got %d", len(d.Groups))
	}
}

func TestParseICFFolded(t *testing.T) {
	// icf_pair.c defines two semantically-equivalent helpers that GCC's
	// ICF pass folds into one class. The parser currently reports N
	// placeholder groups keyed off the "in a non-singular class: N"
	// line — the exact member-name extraction is future work, but the
	// count must be non-zero so downstream tooling knows folding fired.
	d, err := gccdump.ParseICFFile(filepath.Join(
		repoRoot(t), "testdata", "samples", "gcc-16",
		"lib", "icf_pair.c.c.089i.icf",
	))
	if err != nil {
		t.Fatalf("ParseICFFile: %v", err)
	}
	if len(d.Groups) == 0 {
		t.Errorf("icf_pair.c produced 0 groups; expected the ICF-forced pair to fire")
	}
}

func TestParseDevirtEmpty(t *testing.T) {
	// Pure C dumps rarely produce devirt hits — the fixture has none.
	d, err := gccdump.ParseDevirtFile(filepath.Join(
		repoRoot(t), "testdata", "samples", "gcc-16",
		"app1", "main.c.c.090i.devirt",
	))
	if err != nil {
		t.Fatalf("ParseDevirtFile: %v", err)
	}
	if len(d.Hits) != 0 {
		t.Errorf("expected zero devirt hits on fixture, got %d", len(d.Hits))
	}
}

func ids(ss []gccdump.InlineSummary) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.FunctionLocalID
	}
	return out
}
