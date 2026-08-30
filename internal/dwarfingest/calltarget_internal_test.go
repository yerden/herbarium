package dwarfingest

import "testing"

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
