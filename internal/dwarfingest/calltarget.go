package dwarfingest

import (
	"debug/dwarf"
	"debug/elf"
	"encoding/binary"
	"strings"
)

// Resolving what an indirect call dispatches through needs two sources,
// because GCC only ever emits one of them for a given site:
//
//   - DW_AT_call_target, a DWARF expression naming the location the
//     callee address came from. GCC emits it when that location is a
//     register it can still describe at the return PC — in practice,
//     calls through a function-pointer *parameter*.
//
//   - the call instruction's relocation. For the dispatch-table shape
//     (`g_ops.add(...)` → `call *g_ops+0x0(%rip)`) GCC emits no
//     call_target at all, because the loaded value is dead by the
//     return PC. But the instruction carries an R_X86_64_PC32
//     relocation against the table symbol, and its addend pins the
//     exact byte offset — i.e. the exact struct member.
//
// Both routes end at a type DIE, which is then rendered in the same
// form as Subprogram.Signature so callers can join the two directly.

// DWARF expression opcodes decoded out of DW_AT_call_target.
const (
	opReg0       = 0x50 // DW_OP_reg0 .. DW_OP_reg31 (0x50..0x6f)
	opReg31      = 0x6f
	opRegx       = 0x90 // DW_OP_regx <ULEB reg>
	opEntryValue = 0xa3 // DW_OP_entry_value <ULEB len> <expr>
)

// x86-64 SysV passes the first six integer/pointer arguments in
// rdi, rsi, rdx, rcx, r8, r9 — DWARF register numbers 5, 4, 1, 2, 8, 9.
// DW_AT_call_target's DW_OP_entry_value(DW_OP_regN) therefore names the
// Nth argument of the enclosing function.
var argRegOrder = []uint64{5, 4, 1, 2, 8, 9}

// callTargetReloc is the relocation that supplies an indirect call's
// target address.
type callTargetReloc struct {
	sym    string
	addend int64
}

// indirectResolver holds the per-object indexes the second pass needs.
// Building them is O(object size) once, versus a linear scan per call
// site, and a translation unit can hold hundreds of indirect calls.
type indirectResolver struct {
	tc *typeCache
	dw *dwarf.Data

	// x86Reloc is false on architectures where the return_pc-4 rule
	// below does not hold; the relocation route is then skipped
	// entirely rather than matching something unrelated.
	x86Reloc bool

	funcSyms map[string]elf.Symbol                 // FUNC symbol by name
	relocs   map[uint32]map[uint64]callTargetReloc // section index → offset → reloc
	varTypes map[string]dwarf.Offset               // CU-scope variable name → DW_AT_type
}

func newIndirectResolver(f *elf.File, dw *dwarf.Data, tc *typeCache, varTypes map[string]dwarf.Offset) *indirectResolver {
	ir := &indirectResolver{
		tc:       tc,
		dw:       dw,
		x86Reloc: f.Machine == elf.EM_X86_64 && f.Class == elf.ELFCLASS64,
		funcSyms: map[string]elf.Symbol{},
		relocs:   map[uint32]map[uint64]callTargetReloc{},
		varTypes: varTypes,
	}

	syms, err := f.Symbols()
	if err != nil {
		// A stripped .o has no symtab; the relocation route is then
		// unavailable but DW_AT_call_target still works.
		return ir
	}
	for _, s := range syms {
		if elf.ST_TYPE(s.Info) == elf.STT_FUNC && s.Name != "" {
			if _, dup := ir.funcSyms[s.Name]; !dup {
				ir.funcSyms[s.Name] = s
			}
		}
	}
	if ir.x86Reloc {
		ir.loadRelocs(f, syms)
	}
	return ir
}

// loadRelocs indexes SHT_RELA entries by (target section, offset).
// Only relocations against executable sections are kept — .rela.debug_*
// dwarfs .rela.text in most objects and can never hold a call site.
func (ir *indirectResolver) loadRelocs(f *elf.File, syms []elf.Symbol) {
	for _, sec := range f.Sections {
		if sec.Type != elf.SHT_RELA || int(sec.Info) >= len(f.Sections) {
			continue
		}
		if f.Sections[sec.Info].Flags&elf.SHF_EXECINSTR == 0 {
			continue
		}
		data, err := sec.Data()
		if err != nil {
			continue
		}
		byOff := ir.relocs[sec.Info]
		if byOff == nil {
			byOff = map[uint64]callTargetReloc{}
			ir.relocs[sec.Info] = byOff
		}
		for off := 0; off+24 <= len(data); off += 24 {
			rOff := binary.LittleEndian.Uint64(data[off:])
			rInfo := binary.LittleEndian.Uint64(data[off+8:])
			rAddend := int64(binary.LittleEndian.Uint64(data[off+16:]))
			// f.Symbols() drops the null entry at index 0, so the ELF
			// symbol index is one ahead of the slice index.
			symIdx := int(rInfo>>32) - 1
			if symIdx < 0 || symIdx >= len(syms) {
				continue
			}
			byOff[rOff] = callTargetReloc{sym: syms[symIdx].Name, addend: rAddend}
		}
	}
}

// resolve fills cs.CalleeType and cs.FieldHint, leaving them empty when
// neither route yields a typed answer.
func (ir *indirectResolver) resolve(cs *CallSite) {
	if len(cs.callTarget) > 0 && ir.resolveViaCallTarget(cs) {
		return
	}
	ir.resolveViaReloc(cs)
}

// resolveViaCallTarget handles the register forms GCC emits: a call
// through a function-pointer parameter, described as the value that
// argument register held on entry.
func (ir *indirectResolver) resolveViaCallTarget(cs *CallSite) bool {
	reg, ok := decodeRegExpr(cs.callTarget)
	if !ok || cs.enclosingOff == 0 {
		return false
	}
	argIdx := -1
	for i, r := range argRegOrder {
		if r == reg {
			argIdx = i
			break
		}
	}
	if argIdx < 0 {
		return false
	}

	params := ir.formalParams(cs.enclosingOff)
	if argIdx >= len(params) {
		return false
	}
	p := params[argIdx]
	sig, ok := ir.fnPtrSignature(p.typeOff)
	if !ok {
		// The register-order mapping only holds when every preceding
		// argument is register-passed. A non-fn-pointer here means the
		// guess was wrong, so report nothing rather than something false.
		return false
	}
	cs.CalleeType = sig
	cs.FieldHint = p.name
	return true
}

// resolveViaReloc reads the relocation on the call instruction. On
// x86-64 `call *disp(%rip)` ends with the 4-byte displacement, so the
// relocation sits at return_pc-4 and the addressed byte is
// symbol + addend + 4 (the displacement is relative to the *end* of the
// instruction, which is return_pc).
func (ir *indirectResolver) resolveViaReloc(cs *CallSite) bool {
	if !ir.x86Reloc || cs.returnPC < 4 || cs.EnclosingName == "" {
		return false
	}
	sym, ok := ir.funcSyms[cs.EnclosingName]
	if !ok || cs.returnPC < sym.Value || cs.returnPC > sym.Value+sym.Size {
		return false
	}
	rel, ok := ir.relocs[uint32(sym.Section)][cs.returnPC-4]
	if !ok {
		return false
	}
	typeOff, ok := ir.varTypes[rel.sym]
	if !ok {
		return false
	}
	return ir.resolveMember(cs, rel.sym, typeOff, rel.addend+4)
}

// resolveMember walks from a variable's type to the function pointer
// living at byteOff inside it.
func (ir *indirectResolver) resolveMember(cs *CallSite, varName string, typeOff dwarf.Offset, byteOff int64) bool {
	base, e := ir.stripQualifiers(typeOff)
	if e == nil {
		return false
	}

	// A bare function-pointer variable: `int (*g_hook)(int)`.
	if byteOff == 0 {
		if sig, ok := ir.fnPtrSignature(base); ok {
			cs.CalleeType = sig
			cs.FieldHint = varName
			return true
		}
	}

	if e.Tag != dwarf.TagStructType && e.Tag != dwarf.TagUnionType {
		return false
	}
	r := ir.dw.Reader()
	r.Seek(base)
	if _, err := r.Next(); err != nil {
		return false
	}
	for {
		c, err := r.Next()
		if err != nil || c == nil || c.Tag == 0 {
			return false
		}
		if c.Children {
			r.SkipChildren()
		}
		if c.Tag != dwarf.TagMember {
			continue
		}
		loc, _ := c.Val(dwarf.AttrDataMemberLoc).(int64)
		if loc != byteOff {
			continue
		}
		mt, ok := c.Val(dwarf.AttrType).(dwarf.Offset)
		if !ok {
			return false
		}
		sig, ok := ir.fnPtrSignature(mt)
		if !ok {
			return false
		}
		name, _ := c.Val(dwarf.AttrName).(string)
		if name == "" {
			return false
		}
		cs.CalleeType = sig
		// An anonymous container has no tag to qualify with, so fall
		// back to the variable the call actually dispatched through.
		qualifier := varName
		if tag, _ := e.Val(dwarf.AttrName).(string); tag != "" {
			qualifier = tag
		}
		cs.FieldHint = qualifier + "." + name
		return true
	}
}

// fnPtrSignature renders the pointee of a pointer-to-function type in
// Subprogram.Signature form. Reports false for anything else, which is
// how both routes above self-check their guesses.
func (ir *indirectResolver) fnPtrSignature(off dwarf.Offset) (string, bool) {
	_, e := ir.stripQualifiers(off)
	if e == nil || e.Tag != dwarf.TagPointerType {
		return "", false
	}
	to, ok := e.Val(dwarf.AttrType).(dwarf.Offset)
	if !ok {
		return "", false
	}
	sub, se := ir.stripQualifiers(to)
	if se == nil || se.Tag != dwarf.TagSubroutineType {
		return "", false
	}
	sig := ir.tc.renderSubroutineType(sub, se)
	if sig == "" || !strings.Contains(sig, "(") {
		return "", false
	}
	return sig, true
}

// stripQualifiers follows const/volatile/typedef chains to the type DIE
// that actually carries structure.
func (ir *indirectResolver) stripQualifiers(off dwarf.Offset) (dwarf.Offset, *dwarf.Entry) {
	// Bounded so a malformed (self-referential) chain can't spin.
	for range 32 {
		r := ir.dw.Reader()
		r.Seek(off)
		e, err := r.Next()
		if err != nil || e == nil {
			return off, nil
		}
		switch e.Tag {
		case dwarf.TagConstType, dwarf.TagVolatileType, dwarf.TagTypedef, dwarf.TagRestrictType:
			next, ok := e.Val(dwarf.AttrType).(dwarf.Offset)
			if !ok {
				return off, e
			}
			off = next
		default:
			return off, e
		}
	}
	return off, nil
}

type formalParam struct {
	name    string
	typeOff dwarf.Offset
}

func (ir *indirectResolver) formalParams(spOff dwarf.Offset) []formalParam {
	r := ir.dw.Reader()
	r.Seek(spOff)
	if _, err := r.Next(); err != nil {
		return nil
	}
	var out []formalParam
	for {
		c, err := r.Next()
		if err != nil || c == nil || c.Tag == 0 {
			return out
		}
		if c.Children {
			r.SkipChildren()
		}
		if c.Tag != dwarf.TagFormalParameter {
			continue
		}
		p := formalParam{}
		p.name, _ = c.Val(dwarf.AttrName).(string)
		p.typeOff, _ = c.Val(dwarf.AttrType).(dwarf.Offset)
		out = append(out, p)
	}
}

// decodeRegExpr recognizes the register forms of a DW_AT_call_target
// expression — plain DW_OP_regN/DW_OP_regx and the DW_OP_entry_value
// wrapper GCC uses once the register has been clobbered — and returns
// the DWARF register number.
func decodeRegExpr(expr []byte) (uint64, bool) {
	if len(expr) == 0 {
		return 0, false
	}
	if expr[0] == opEntryValue {
		n, w := uleb(expr[1:])
		if w == 0 || int(n) > len(expr)-1-w {
			return 0, false
		}
		return decodeRegExpr(expr[1+w : 1+w+int(n)])
	}
	if expr[0] >= opReg0 && expr[0] <= opReg31 && len(expr) == 1 {
		return uint64(expr[0] - opReg0), true
	}
	if expr[0] == opRegx {
		n, w := uleb(expr[1:])
		if w == 0 || w != len(expr)-1 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// uleb decodes a ULEB128 and returns the value plus bytes consumed
// (0 when the encoding is truncated).
func uleb(b []byte) (uint64, int) {
	var v uint64
	var shift uint
	for i, c := range b {
		if shift >= 64 {
			return 0, 0
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, i + 1
		}
		shift += 7
	}
	return 0, 0
}
