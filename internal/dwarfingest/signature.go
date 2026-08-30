package dwarfingest

import (
	"debug/dwarf"
	"strings"
)

// typeCache renders type DIEs to printable strings, memoized by offset.
// The walker recurses through pointer/const/typedef/base_type chains
// per the C surface syntax GCC records in DWARF.
type typeCache struct {
	dw    *dwarf.Data
	cache map[dwarf.Offset]string
}

func newTypeCache(dw *dwarf.Data) *typeCache {
	return &typeCache{dw: dw, cache: map[dwarf.Offset]string{}}
}

// render returns a printable form of the type DIE at off — "int",
// "const char *", "struct ops", "int (*)(int, int)", etc.
func (tc *typeCache) render(off dwarf.Offset) string {
	if s, ok := tc.cache[off]; ok {
		return s
	}
	// Reserve the slot to break cycles (self-referential types via
	// pointers back to enclosing structs).
	tc.cache[off] = ""
	s := tc.renderNoCache(off)
	tc.cache[off] = s
	return s
}

func (tc *typeCache) renderNoCache(off dwarf.Offset) string {
	r := tc.dw.Reader()
	r.Seek(off)
	e, err := r.Next()
	if err != nil || e == nil {
		return ""
	}

	switch e.Tag {
	case dwarf.TagBaseType, dwarf.TagUnspecifiedType:
		if n, ok := e.Val(dwarf.AttrName).(string); ok {
			return n
		}
		return ""
	case dwarf.TagPointerType:
		inner := ""
		if to, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
			inner = tc.render(to)
		}
		if inner == "" {
			return "void *"
		}
		// Subroutine pointer: emit "return (*)(params)".
		if strings.HasSuffix(inner, ")") && strings.Contains(inner, "(") &&
			!strings.HasSuffix(inner, "*") {
			// inner is already in "return (params)" form. Splice in "(*)".
			if paren := strings.Index(inner, "("); paren > 0 {
				return inner[:paren] + "(*)" + inner[paren:]
			}
		}
		if strings.HasSuffix(inner, "*") {
			return inner + "*"
		}
		return inner + " *"
	case dwarf.TagConstType:
		if to, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
			return "const " + tc.render(to)
		}
		return "const"
	case dwarf.TagVolatileType:
		if to, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
			return "volatile " + tc.render(to)
		}
		return "volatile"
	case dwarf.TagTypedef:
		if n, ok := e.Val(dwarf.AttrName).(string); ok {
			return n
		}
		return ""
	case dwarf.TagStructType:
		if n, ok := e.Val(dwarf.AttrName).(string); ok && n != "" {
			return "struct " + n
		}
		return "struct"
	case dwarf.TagUnionType:
		if n, ok := e.Val(dwarf.AttrName).(string); ok && n != "" {
			return "union " + n
		}
		return "union"
	case dwarf.TagEnumerationType:
		if n, ok := e.Val(dwarf.AttrName).(string); ok && n != "" {
			return "enum " + n
		}
		return "enum"
	case dwarf.TagArrayType:
		if to, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
			return tc.render(to) + "[]"
		}
		return "[]"
	case dwarf.TagSubroutineType:
		return tc.renderSubroutineType(off, e)
	}
	if n, ok := e.Val(dwarf.AttrName).(string); ok {
		return n
	}
	return ""
}

// renderSubroutineType walks a DW_TAG_subroutine_type entry and formats
// it as "return (paramT1, paramT2)". Called for both fn-ptr targets
// (invoked from renderNoCache on TagPointerType → this) and standalone
// subroutine type refs.
func (tc *typeCache) renderSubroutineType(off dwarf.Offset, e *dwarf.Entry) string {
	ret := "void"
	if to, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
		ret = tc.render(to)
	}
	// Walk children for params.
	r := tc.dw.Reader()
	r.Seek(off)
	if _, err := r.Next(); err != nil {
		return ret + " ()"
	}
	var params []string
	for {
		c, err := r.Next()
		if err != nil || c == nil || c.Tag == 0 {
			break
		}
		if c.Tag == dwarf.TagFormalParameter {
			p := ""
			if pt, ok := c.Val(dwarf.AttrType).(dwarf.Offset); ok {
				p = tc.render(pt)
			}
			if p == "" {
				p = "?"
			}
			params = append(params, p)
		}
		if c.Children {
			r.SkipChildren()
		}
	}
	// Rendered exactly as buildSignature renders a subprogram, so a
	// fn-pointer's callee_type joins symbols.signature directly.
	if len(params) == 0 {
		return ret + " (void)"
	}
	return ret + " (" + strings.Join(params, ", ") + ")"
}

// buildSignature walks a DW_TAG_subprogram DIE and formats its
// signature as "returnType (param1, param2, ...)". Uses the same
// rendering rules as fn-pointer types so the two match.
func buildSignature(tc *typeCache, e *dwarf.Entry) string {
	ret := "void"
	if to, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
		ret = tc.render(to)
	}
	if !e.Children {
		return ret + " ()"
	}
	r := tc.dw.Reader()
	r.Seek(e.Offset)
	if _, err := r.Next(); err != nil {
		return ret + " ()"
	}
	var params []string
	for {
		c, err := r.Next()
		if err != nil || c == nil || c.Tag == 0 {
			break
		}
		if c.Tag == dwarf.TagFormalParameter {
			p := ""
			if pt, ok := c.Val(dwarf.AttrType).(dwarf.Offset); ok {
				p = tc.render(pt)
			}
			if p == "" {
				p = "?"
			}
			params = append(params, p)
		} else if c.Children {
			r.SkipChildren()
		}
	}
	if len(params) == 0 {
		return ret + " (void)"
	}
	return ret + " (" + strings.Join(params, ", ") + ")"
}
