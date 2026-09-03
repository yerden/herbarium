package ingest

import "testing"

// TestPruneUnreferencedKeepRule pins the four ways a symbol earns its
// row against the one case that does not. The pruned shape — internal
// linkage, no def, not address-taken, referenced by nothing — is the
// unused `static inline` from a widely-included header, which the USR
// scheme replicates once per including TU and which dominated the
// `symbols` table before this filter existed.
func TestPruneUnreferencedKeepRule(t *testing.T) {
	rec := func(usr, linkage string, defs int, addr bool) *symbolRec {
		r := &symbolRec{usr: usr, linkage: linkage, addressTaken: addr}
		for i := 0; i < defs; i++ {
			r.defs = append(r.defs, defRec{file: "a.c", line: i + 1})
		}
		return r
	}

	symbols := map[string]*symbolRec{
		"keep:def":      rec("keep:def", "internal", 1, false),
		"keep:addr":     rec("keep:addr", "internal", 0, true),
		"keep:external": rec("keep:external", "external", 0, false),
		"keep:weak":     rec("keep:weak", "weak", 0, false),
		"keep:edge":     rec("keep:edge", "internal", 0, false),
		"drop:me":       rec("drop:me", "internal", 0, false),
	}
	referenced := map[string]struct{}{"keep:edge": {}}

	pruneUnreferenced(symbols, referenced)

	if _, ok := symbols["drop:me"]; ok {
		t.Error("unreferenced internal decl-only symbol survived the prune")
	}
	for _, usr := range []string{"keep:def", "keep:addr", "keep:external", "keep:weak", "keep:edge"} {
		if _, ok := symbols[usr]; !ok {
			t.Errorf("%s was pruned but should have been kept", usr)
		}
	}
	if len(symbols) != 5 {
		t.Errorf("symbols left = %d, want 5", len(symbols))
	}
}
