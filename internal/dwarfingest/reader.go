package dwarfingest

import (
	"debug/dwarf"
	"debug/elf"
	"fmt"
	"path/filepath"
	"slices"
)

// Read opens an ELF object and returns the DWARF facts herbarium needs.
// Preflight has already ensured the build was compiled with -g.
func Read(path string) (*Info, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("dwarfingest: open elf %s: %w", path, err)
	}
	defer f.Close()

	dw, err := f.DWARF()
	if err != nil {
		return nil, fmt.Errorf("dwarfingest: extract DWARF from %s: %w", path, err)
	}

	info := &Info{ObjectPath: path}
	tc := newTypeCache(dw)

	r := dw.Reader()
	var (
		curCUFiles []*dwarf.LineFile
		curLR      *dwarf.LineReader
		stack      []frame
	)
	// Type offsets of every CU-scope variable, keyed by name — the
	// relocation route in calltarget.go joins a table symbol back to
	// its DWARF type through this.
	varTypes := map[string]dwarf.Offset{}

	for {
		e, err := r.Next()
		if err != nil {
			return nil, fmt.Errorf("dwarfingest: read %s: %w", path, err)
		}
		if e == nil {
			break
		}
		if e.Tag == 0 {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}

		switch e.Tag {
		case dwarf.TagCompileUnit:
			info.CompDir, _ = e.Val(dwarf.AttrCompDir).(string)
			info.CUFile, _ = e.Val(dwarf.AttrName).(string)
			lr, err := dw.LineReader(e)
			if err == nil && lr != nil {
				curLR = lr
				curCUFiles = lr.Files()
			}

		case dwarf.TagSubprogram:
			sp := parseSubprogram(e, curCUFiles, tc)
			info.Subprograms = append(info.Subprograms, sp)

		case dwarf.TagInlinedSubroutine:
			if ii, ok := parseInlineInstance(e, curCUFiles, stack, dw); ok {
				info.InlineInstances = append(info.InlineInstances, ii)
			}

		case dwarf.TagCallSite:
			cs := parseCallSite(e, curCUFiles, curLR, stack, dw)
			if cs.File != "" || cs.Line != 0 {
				info.CallSites = append(info.CallSites, cs)
			}

		case dwarf.TagStructType, dwarf.TagUnionType:
			s := parseStruct(e, curCUFiles, r, tc)
			info.Structs = append(info.Structs, s)
			// parseStruct consumed the struct's children including the
			// terminating Tag=0 — do NOT push a frame.
			continue

		case dwarf.TagTypedef:
			t := parseTypedef(e, curCUFiles, tc)
			if t.Name != "" {
				info.Typedefs = append(info.Typedefs, t)
			}

		case dwarf.TagVariable:
			if len(stack) <= 1 {
				if n, ok := e.Val(dwarf.AttrName).(string); ok && n != "" {
					if off, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
						varTypes[n] = off
					}
				}
			}
			v := parseVariable(e, curCUFiles, tc)
			// Only CU-scope variables — nested locals would flood the DB.
			if v.Name != "" && v.DeclFile != "" && len(stack) <= 1 {
				info.Variables = append(info.Variables, v)
			}
		}

		if e.Children {
			stack = append(stack, newFrame(e, dw))
		}
	}

	// Building the resolver's indexes costs a pass over the object's
	// symbol and relocation tables, so skip it when nothing needs them.
	if slices.ContainsFunc(info.CallSites, func(cs CallSite) bool { return cs.Indirect }) {
		ir := newIndirectResolver(f, dw, tc, varTypes)
		for i := range info.CallSites {
			if info.CallSites[i].Indirect {
				ir.resolve(&info.CallSites[i])
			}
		}
	}

	return info, nil
}

type frame struct {
	tag             dwarf.Tag
	subprogramName  string
	inlinedFromName string
	offset          dwarf.Offset
}

func newFrame(e *dwarf.Entry, dw *dwarf.Data) frame {
	f := frame{tag: e.Tag, offset: e.Offset}
	switch e.Tag {
	case dwarf.TagSubprogram:
		if n, ok := e.Val(dwarf.AttrName).(string); ok {
			f.subprogramName = n
		}
		// A concrete out-of-line instance (emitted when a callee is both
		// inlined somewhere and kept as a standalone body) carries no
		// name of its own — only a pointer at the abstract instance.
		if f.subprogramName == "" {
			if off, ok := e.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset); ok {
				f.subprogramName = lookupName(dw, off)
			}
		}
	case dwarf.TagInlinedSubroutine:
		if off, ok := e.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset); ok {
			f.inlinedFromName = lookupName(dw, off)
		}
	}
	return f
}

func parseSubprogram(e *dwarf.Entry, files []*dwarf.LineFile, tc *typeCache) Subprogram {
	sp := Subprogram{}
	if n, ok := e.Val(dwarf.AttrName).(string); ok {
		sp.Name = n
	}
	if ln, ok := e.Val(dwarf.AttrLinkageName).(string); ok {
		sp.LinkageName = ln
	}
	if idx, ok := e.Val(dwarf.AttrDeclFile).(int64); ok {
		sp.DeclFile = fileFromIdx(files, int(idx))
	}
	if ln, ok := e.Val(dwarf.AttrDeclLine).(int64); ok {
		sp.DeclLine = int(ln)
	}
	if col, ok := e.Val(dwarf.AttrDeclColumn).(int64); ok {
		sp.DeclColumn = int(col)
	}
	if _, hasLow := e.Val(dwarf.AttrLowpc).(uint64); hasLow {
		sp.Definition = true
	}
	if decl, ok := e.Val(dwarf.AttrDeclaration).(bool); ok && decl {
		sp.Declaration = true
	}
	// Abstract instances of inlined functions have DW_AT_inline set and
	// no low_pc; they still represent the source definition.
	if _, hasInline := e.Val(dwarf.AttrInline).(int64); hasInline && !sp.Declaration {
		sp.Definition = true
	}
	if ext, ok := e.Val(dwarf.AttrExternal).(bool); ok && ext {
		sp.External = true
	}
	sp.Signature = buildSignature(tc, e)
	return sp
}

// parseInlineInstance reads one DW_TAG_inlined_subroutine. The DIE says
// only which body was copied in (DW_AT_abstract_origin) and where the
// call was written (DW_AT_call_file/line/column); who it was copied
// into comes from the frame stack, which is also where nesting depth
// comes from. Returns false when the origin does not resolve to a name
// — an unnamed inline event is not a fact worth storing.
func parseInlineInstance(e *dwarf.Entry, files []*dwarf.LineFile, stack []frame, dw *dwarf.Data) (InlineInstance, bool) {
	ii := InlineInstance{Depth: 1}
	off, ok := e.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset)
	if !ok {
		return InlineInstance{}, false
	}
	if ii.CalleeName = lookupName(dw, off); ii.CalleeName == "" {
		return InlineInstance{}, false
	}

	if idx, ok := e.Val(dwarf.AttrCallFile).(int64); ok {
		ii.File = fileFromIdx(files, int(idx))
	}
	if ln, ok := e.Val(dwarf.AttrCallLine).(int64); ok {
		ii.Line = int(ln)
	}
	if col, ok := e.Val(dwarf.AttrCallColumn).(int64); ok {
		ii.Column = int(col)
	}

	// Walk outward: every enclosing inlined_subroutine is one more level
	// of nesting, and the first real subprogram is the physical frame.
	for i := len(stack) - 1; i >= 0; i-- {
		f := stack[i]
		if f.tag == dwarf.TagInlinedSubroutine {
			if ii.ParentCalleeName == "" {
				ii.ParentCalleeName = f.inlinedFromName
			}
			ii.Depth++
			continue
		}
		if f.tag == dwarf.TagSubprogram {
			ii.CallerName = f.subprogramName
			break
		}
	}
	if ii.CallerName == "" {
		return InlineInstance{}, false
	}
	return ii, true
}

// parseCallSite recovers the source location of a call. GCC 16 does NOT
// put DW_AT_call_file/line/column on the DW_TAG_call_site DIE itself —
// those attrs live on the *enclosing* DW_TAG_inlined_subroutine. For
// the actual call site we resolve DW_AT_call_return_pc through the CU's
// line table.
func parseCallSite(e *dwarf.Entry, files []*dwarf.LineFile, lr *dwarf.LineReader, stack []frame, dw *dwarf.Data) CallSite {
	cs := CallSite{}

	// Some GCC configurations DO put call_file/line/column directly on
	// the call_site — respect them when present.
	if idx, ok := e.Val(dwarf.AttrCallFile).(int64); ok {
		cs.File = fileFromIdx(files, int(idx))
	}
	if ln, ok := e.Val(dwarf.AttrCallLine).(int64); ok {
		cs.Line = int(ln)
	}
	if col, ok := e.Val(dwarf.AttrCallColumn).(int64); ok {
		cs.Column = int(col)
	}

	if pc, ok := e.Val(dwarf.AttrCallReturnPC).(uint64); ok {
		cs.returnPC = pc
	}
	cs.callTarget, _ = e.Val(dwarf.AttrCallTarget).([]byte)

	// Fallback: resolve via line table using CallReturnPC.
	if (cs.File == "" || cs.Line == 0) && lr != nil {
		if pc := cs.returnPC; pc > 0 {
			// Use pc-1: return_pc is the byte AFTER the call, whose
			// line entry is "the statement after the call". pc-1 is
			// inside the call instruction and maps to its source line.
			resolveLine(lr, pc-1, &cs, files)
		}
	}

	// Attribution: enclosing = innermost subprogram; source caller =
	// innermost inlined-subroutine (falls back to enclosing when no
	// inline in the chain).
	for i := len(stack) - 1; i >= 0; i-- {
		f := stack[i]
		if f.tag == dwarf.TagInlinedSubroutine && cs.SourceCallerName == "" {
			cs.SourceCallerName = f.inlinedFromName
		}
		if f.tag == dwarf.TagSubprogram {
			if cs.EnclosingName == "" {
				cs.EnclosingName = f.subprogramName
				cs.enclosingOff = f.offset
			}
			if cs.SourceCallerName == "" {
				cs.SourceCallerName = f.subprogramName
			}
			break
		}
	}

	if origin, ok := e.Val(dwarf.AttrCallOrigin).(dwarf.Offset); ok {
		cs.CalleeName = lookupName(dw, origin)
	} else {
		cs.Indirect = true
	}
	return cs
}

func resolveLine(lr *dwarf.LineReader, pc uint64, cs *CallSite, files []*dwarf.LineFile) {
	// LineReader advances forward only; Reset lets us seek back if
	// PCs come out of order across sites.
	lr.Reset()
	var entry dwarf.LineEntry
	if err := lr.SeekPC(pc, &entry); err != nil {
		return
	}
	if entry.File != nil {
		cs.File = entry.File.Name
	}
	cs.Line = entry.Line
	cs.Column = entry.Column
	_ = files
}

func parseStruct(e *dwarf.Entry, files []*dwarf.LineFile, r *dwarf.Reader, tc *typeCache) StructInfo {
	s := StructInfo{}
	if n, ok := e.Val(dwarf.AttrName).(string); ok {
		s.Name = n
	}
	if idx, ok := e.Val(dwarf.AttrDeclFile).(int64); ok {
		s.DeclFile = fileFromIdx(files, int(idx))
	}
	if ln, ok := e.Val(dwarf.AttrDeclLine).(int64); ok {
		s.DeclLine = int(ln)
	}
	if !e.Children {
		return s
	}
	for {
		c, err := r.Next()
		if err != nil || c == nil || c.Tag == 0 {
			break
		}
		if c.Tag == dwarf.TagMember {
			name, _ := c.Val(dwarf.AttrName).(string)
			var typ string
			if off, ok := c.Val(dwarf.AttrType).(dwarf.Offset); ok {
				typ = tc.render(off)
			}
			s.Fields = append(s.Fields, FieldInfo{Name: name, Type: typ})
		}
		if c.Children {
			r.SkipChildren()
		}
	}
	return s
}

func parseTypedef(e *dwarf.Entry, files []*dwarf.LineFile, tc *typeCache) TypedefInfo {
	t := TypedefInfo{}
	if n, ok := e.Val(dwarf.AttrName).(string); ok {
		t.Name = n
	}
	if idx, ok := e.Val(dwarf.AttrDeclFile).(int64); ok {
		t.DeclFile = fileFromIdx(files, int(idx))
	}
	if ln, ok := e.Val(dwarf.AttrDeclLine).(int64); ok {
		t.DeclLine = int(ln)
	}
	if off, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
		t.Target = tc.render(off)
	}
	return t
}

func parseVariable(e *dwarf.Entry, files []*dwarf.LineFile, tc *typeCache) VariableInfo {
	v := VariableInfo{}
	if n, ok := e.Val(dwarf.AttrName).(string); ok {
		v.Name = n
	}
	if idx, ok := e.Val(dwarf.AttrDeclFile).(int64); ok {
		v.DeclFile = fileFromIdx(files, int(idx))
	}
	if ln, ok := e.Val(dwarf.AttrDeclLine).(int64); ok {
		v.DeclLine = int(ln)
	}
	if off, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
		v.Type = tc.render(off)
	}
	return v
}

func fileFromIdx(files []*dwarf.LineFile, idx int) string {
	if idx < 0 || idx >= len(files) {
		return ""
	}
	name := files[idx].Name
	if name == "" {
		return ""
	}
	if filepath.IsAbs(name) {
		return name
	}
	return name
}

func lookupName(dw *dwarf.Data, off dwarf.Offset) string {
	r := dw.Reader()
	r.Seek(off)
	e, err := r.Next()
	if err != nil || e == nil {
		return ""
	}
	if n, ok := e.Val(dwarf.AttrName).(string); ok {
		return n
	}
	return ""
}
