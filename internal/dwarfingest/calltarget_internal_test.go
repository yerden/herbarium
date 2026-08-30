package dwarfingest

import "testing"

// paramList carries the C distinction between the three empty-ish
// argument lists. Collapsing any pair of them would let callee_type
// match signatures that are not actually compatible.
func TestParamList(t *testing.T) {
	cases := []struct {
		name     string
		params   []string
		variadic bool
		proto    bool
		want     string
	}{
		{"void", nil, false, true, "(void)"},
		// int f() — non-prototyped. GCC emits no DW_AT_prototyped and a
		// DW_TAG_unspecified_parameters child, but the list is unknown,
		// not variadic, and "(...)" is not valid C.
		{"non-prototyped", nil, true, false, "()"},
		{"non-prototyped no unspec", nil, false, false, "()"},
		{"variadic", []string{"const char *"}, true, true, "(const char *, ...)"},
		{"plain", []string{"int", "int"}, false, true, "(int, int)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := paramList(tc.params, tc.variadic, tc.proto); got != tc.want {
				t.Errorf("paramList(%v, variadic=%v, proto=%v) = %q, want %q",
					tc.params, tc.variadic, tc.proto, got, tc.want)
			}
		})
	}
}

// argIndexForReg must decline rather than mis-map whenever the SysV
// integer-register assignment cannot be replayed from the parameter list
// alone. The classification it consumes is exercised against real DWARF
// in reader_test.go.
func TestArgIndexForReg(t *testing.T) {
	const (
		i = true  // occupies one integer argument register
		o = false // anything else: float, aggregate, __int128, vector
	)
	cases := []struct {
		name    string
		classes []bool
		reg     uint64
		want    int
	}{
		{"first integer arg (rdi)", []bool{i, i}, 5, 0},
		{"second integer arg (rsi)", []bool{i, i}, 4, 1},
		{"third integer arg (rdx)", []bool{i, i, i}, 1, 2},
		// long dispatch(double d, unary first, binary second, long x):
		// d takes an SSE register, so rsi holds `second` (index 2), not
		// `first` (index 1). Indexing argRegOrder directly would name
		// `first` and write its incompatible signature to the site.
		{"leading SSE arg", []bool{o, i, i, i}, 4, -1},
		{"aggregate mid-list", []bool{i, o, i}, 1, -1},
		{"register not in list", []bool{i}, 4, -1},
		{"past the six integer regs", []bool{i, i, i, i, i, i, i}, 99, -1},
		{"no params", nil, 5, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := argIndexForReg(tc.classes, tc.reg); got != tc.want {
				t.Errorf("argIndexForReg(%v, reg=%d) = %d, want %d", tc.classes, tc.reg, got, tc.want)
			}
		})
	}
}

// The relocation route is covered end-to-end against the fixture in
// reader_test.go; the fixture has no site for which GCC emits
// DW_AT_call_target, so the expression decoder is pinned here against
// the byte sequences GCC actually produces.
func TestDecodeRegExpr(t *testing.T) {
	cases := []struct {
		name string
		expr []byte
		want uint64
		ok   bool
	}{
		// DW_OP_entry_value(1) { DW_OP_reg5 (rdi) } — a call through a
		// fn-pointer parameter, after the register was clobbered.
		{"entry_value reg5", []byte{0xa3, 0x01, 0x55}, 5, true},
		{"entry_value reg4", []byte{0xa3, 0x01, 0x54}, 4, true},
		{"bare reg9", []byte{0x59}, 9, true},
		{"regx 17", []byte{0x90, 0x11}, 17, true},
		{"entry_value regx", []byte{0xa3, 0x02, 0x90, 0x11}, 17, true},

		{"empty", nil, 0, false},
		// DW_OP_addr: the operand is relocated, so in an unlinked .o it
		// reads as 0 and names nothing.
		{"addr", []byte{0x03, 0, 0, 0, 0, 0, 0, 0, 0}, 0, false},
		{"entry_value truncated", []byte{0xa3, 0x04, 0x55}, 0, false},
		{"reg with trailing bytes", []byte{0x55, 0x23, 0x08}, 0, false},
		{"regx truncated", []byte{0x90, 0x80}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decodeRegExpr(tc.expr)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("decodeRegExpr(%v) = %d, %v; want %d, %v", tc.expr, got, ok, tc.want, tc.ok)
			}
		})
	}
}
