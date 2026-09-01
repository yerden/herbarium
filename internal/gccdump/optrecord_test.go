package gccdump_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yerden/herbarium/internal/gccdump"
)

func sampleOptRecord(t *testing.T, parts ...string) *gccdump.OptRecordDump {
	t.Helper()
	args := append([]string{repoRoot(t), "testdata", "samples", "gcc-16"}, parts...)
	d, err := gccdump.ParseOptRecordFile(filepath.Join(args...))
	if err != nil {
		t.Fatalf("ParseOptRecordFile: %v", err)
	}
	return d
}

// find returns the single record for a caller→callee pair in a pass, or
// fails. Pass "" matches any pass.
func find(t *testing.T, d *gccdump.OptRecordDump, pass, caller, callee string) gccdump.InlineRecord {
	t.Helper()
	var hits []gccdump.InlineRecord
	for _, r := range d.InlineRecords {
		if (pass == "" || r.Pass == pass) && r.CallerName == caller && r.CalleeName == callee {
			hits = append(hits, r)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("records for %s: %s -> %s = %d, want 1 (all: %+v)", pass, caller, callee, len(hits), d.InlineRecords)
	}
	return hits[0]
}

// The early inliner's always_inline fold is the record no IPA-stage dump
// carries: scale_by_two never reaches the ipa-inline pass, so .inline and
// .cgraph show nothing for it.
func TestParseOptRecordEarlyInline(t *testing.T) {
	d := sampleOptRecord(t, "lib", "shared_utils.c.c.opt-record.json.gz")

	r := find(t, d, "einline", "scaled_compute", "scale_by_two")
	if !r.Inlined {
		t.Errorf("scale_by_two -> scaled_compute: Inlined = false, want true")
	}
	if r.Reason != "" {
		t.Errorf("Reason on a success = %q, want empty", r.Reason)
	}
	if r.CalleeLocalID != "5" || r.CallerLocalID != "6" {
		t.Errorf("local ids = callee %q caller %q, want callee 5 caller 6", r.CalleeLocalID, r.CallerLocalID)
	}
}

// A failure record's caller/callee order is the reverse of a success
// record's; the parser reads the message's separator, not the position.
func TestParseOptRecordFailureReason(t *testing.T) {
	d := sampleOptRecord(t, "lib", "shared_utils.c.c.opt-record.json.gz")

	r := find(t, d, "einline", "compute", "add_ints")
	if r.Inlined {
		t.Errorf("compute -> add_ints: Inlined = true, want false")
	}
	if r.Reason != "function body can be overwritten at link time" {
		t.Errorf("Reason = %q", r.Reason)
	}
	if r.File == "" || r.Line == 0 {
		t.Errorf("call site not located: %q:%d:%d", r.File, r.Line, r.Column)
	}

	// The same rejection is re-made by the IPA inliner, and the two are
	// distinct facts: one pass looked at the call before IPA analysis and
	// one after.
	if ipa := find(t, d, "inline", "compute", "add_ints"); ipa.Inlined {
		t.Errorf("ipa inline pass: Inlined = true, want false")
	}
}

// The IPA inliner reports clones under their clone name; ingest maps the
// name/order back to the parent symbol through the cgraph resolve map.
func TestParseOptRecordIPAClone(t *testing.T) {
	d := sampleOptRecord(t, "app1", "main.c.c.opt-record.json.gz")

	r := find(t, d, "inline", "main", "use_dispatch.constprop")
	if !r.Inlined {
		t.Errorf("use_dispatch.constprop -> main: Inlined = false, want true")
	}
	if r.CalleeLocalID == "" || r.CallerLocalID == "" {
		t.Errorf("missing local ids: %+v", r)
	}

	// printf has no body in this TU — a rejection with GCC's own words.
	if p := find(t, d, "inline", "main", "printf"); p.Reason != "function body not available" {
		t.Errorf("printf Reason = %q", p.Reason)
	}
}

// Only inline-pass records survive; the same file carries vectorizer
// records, and the notes that restate a decision are dropped so a
// single fold is not counted twice.
func TestParseOptRecordDropsNonInlineRecords(t *testing.T) {
	d := sampleOptRecord(t, "app1", "main.c.c.opt-record.json.gz")
	if len(d.InlineRecords) == 0 {
		t.Fatal("no inline records parsed")
	}
	seen := map[[3]string]bool{}
	for _, r := range d.InlineRecords {
		if r.CallerName == "" || r.CalleeName == "" {
			t.Errorf("record with an unresolved end: %+v", r)
		}
		k := [3]string{r.Pass, r.CallerName, r.CalleeName}
		if seen[k] {
			t.Errorf("duplicate record for %v", k)
		}
		seen[k] = true
	}
}

// A future GCC that bumps the record format must fail loudly. Parsing a
// changed layout leniently would yield zero inline records and no signal
// that the plane went empty.
func TestParseOptRecordRefusesUnknownFormat(t *testing.T) {
	body := `[{"format":"2","generator":{"name":"GNU C"}},[],[]]`
	_, err := gccdump.ParseOptRecord(strings.NewReader(body))
	if err == nil {
		t.Fatal("format 2 parsed without error")
	}
	if !strings.Contains(err.Error(), `"2"`) || !strings.Contains(err.Error(), gccdump.SupportedOptRecordFormat) {
		t.Errorf("error should name both versions: %v", err)
	}
}
