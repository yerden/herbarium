package mcp

import "testing"

// The 'mixed' collapse is unreachable from the fixture — it needs one USR
// whose TUs decided differently — so the fold is tested directly.
func TestOverallVerdictCollapse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		per        []ObjectInlineVerdict
		wantVerdit string
		wantReason string
	}{
		{"empty", nil, VerdictNoDecisionLogged, ""},
		{
			"unanimous declined keeps the reason",
			[]ObjectInlineVerdict{
				{Object: "a.o", Verdict: VerdictDeclined, Reason: "function body not available"},
				{Object: "b.o", Verdict: VerdictDeclined, Reason: "function body not available"},
			},
			VerdictDeclined, "function body not available",
		},
		{
			"disagreement is reported, not resolved",
			[]ObjectInlineVerdict{
				{Object: "a.o", Verdict: VerdictInlinedAndPresent},
				{Object: "b.o", Verdict: VerdictDeclined, Reason: "call is cold"},
			},
			VerdictMixed, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, reason := overallVerdict(tc.per)
			if verdict != tc.wantVerdit || reason != tc.wantReason {
				t.Errorf("= %q/%q, want %q/%q", verdict, reason, tc.wantVerdit, tc.wantReason)
			}
		})
	}
}

// A surviving body outranks a record: presence is stronger evidence than
// any pass's remark, and the later pass's reason is the one that stuck.
func TestPerObjectVerdictPrecedence(t *testing.T) {
	per := perObjectVerdicts(
		[]InlineRecordRow{
			{Pass: "einline", Inlined: false, Reason: "call is cold", Object: "a.o"},
			{Pass: "inline", Inlined: true, Object: "a.o"},
			{Pass: "einline", Inlined: false, Reason: "too big", Object: "b.o"},
			{Pass: "inline", Inlined: false, Reason: "body not available", Object: "b.o"},
			{Pass: "inline", Inlined: true, Object: "c.o"},
		},
		[]InlineInstanceRow{{Object: "a.o"}},
	)
	got := map[string]ObjectInlineVerdict{}
	for _, p := range per {
		got[p.Object] = p
	}
	if got["a.o"].Verdict != VerdictInlinedAndPresent {
		t.Errorf("a.o = %q, want %q", got["a.o"].Verdict, VerdictInlinedAndPresent)
	}
	if got["b.o"].Verdict != VerdictDeclined || got["b.o"].Reason != "body not available" {
		t.Errorf("b.o = %+v, want declined with the IPA pass's reason", got["b.o"])
	}
	if got["c.o"].Verdict != VerdictInlinedThenFolded {
		t.Errorf("c.o = %q, want %q", got["c.o"].Verdict, VerdictInlinedThenFolded)
	}
}
