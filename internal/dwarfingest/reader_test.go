package dwarfingest_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yerden/herbarium/internal/dwarfingest"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func TestReadApp1Main(t *testing.T) {
	info, err := dwarfingest.Read(filepath.Join(
		repoRoot(t), "testdata", "fixture", "builddir",
		"app1", "app1.p", "main.c.o",
	))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.HasSuffix(info.CUFile, "main.c") {
		t.Errorf("CUFile = %q, want *main.c", info.CUFile)
	}
	if info.CompDir == "" {
		t.Error("CompDir empty")
	}

	byName := map[string][]dwarfingest.Subprogram{}
	for _, sp := range info.Subprograms {
		byName[sp.Name] = append(byName[sp.Name], sp)
	}

	// main is defined here (has DW_AT_low_pc)
	mainDefs := findDefinitions(byName["main"])
	if len(mainDefs) != 1 {
		t.Errorf("main defs = %d, want 1", len(mainDefs))
	}
	if len(mainDefs) > 0 {
		m := mainDefs[0]
		if m.Signature != "int (int, char **)" {
			t.Errorf("main.Signature = %q, want %q", m.Signature, "int (int, char **)")
		}
		if !strings.HasSuffix(m.DeclFile, "app1/main.c") {
			t.Errorf("main.DeclFile = %q, want *app1/main.c", m.DeclFile)
		}
		if m.DeclLine != 11 {
			t.Errorf("main.DeclLine = %d, want 11", m.DeclLine)
		}
	}

	// use_dispatch is static-defined here
	udDefs := findDefinitions(byName["use_dispatch"])
	if len(udDefs) != 1 {
		t.Errorf("use_dispatch defs = %d, want 1", len(udDefs))
	}
	if len(udDefs) > 0 {
		ud := udDefs[0]
		if ud.Signature != "int (int, int)" {
			t.Errorf("use_dispatch.Signature = %q, want %q", ud.Signature, "int (int, int)")
		}
	}

	// External declarations: printf, hook, compute (with their signatures)
	for _, name := range []string{"printf", "hook", "compute"} {
		decls := findDeclarations(byName[name])
		if len(decls) == 0 {
			t.Errorf("no declaration entry for %s; entries: %+v", name, byName[name])
			continue
		}
	}

	// Signature check for a non-main function.
	if hookDecls := findDeclarations(byName["hook"]); len(hookDecls) > 0 {
		if hookDecls[0].Signature != "int (int)" {
			t.Errorf("hook.Signature = %q, want %q", hookDecls[0].Signature, "int (int)")
		}
	}

	// Struct ops with fn-pointer members.
	var opsStruct *dwarfingest.StructInfo
	for i := range info.Structs {
		if info.Structs[i].Name == "ops" {
			opsStruct = &info.Structs[i]
		}
	}
	if opsStruct == nil {
		t.Fatal("missing struct ops")
	}
	// Fields: add, mul, name.
	fieldTypes := map[string]string{}
	for _, f := range opsStruct.Fields {
		fieldTypes[f.Name] = f.Type
	}
	if fieldTypes["add"] != "int (*)(int, int)" {
		t.Errorf("ops.add type = %q, want %q", fieldTypes["add"], "int (*)(int, int)")
	}
	if fieldTypes["mul"] != "int (*)(int, int)" {
		t.Errorf("ops.mul type = %q, want %q", fieldTypes["mul"], "int (*)(int, int)")
	}
	if !strings.Contains(fieldTypes["name"], "char") {
		t.Errorf("ops.name type = %q, want to contain char", fieldTypes["name"])
	}

	// Call sites: at least one indirect site inside use_dispatch's
	// inlined instance (source caller = use_dispatch, enclosing = main).
	var indirectSites int
	for _, cs := range info.CallSites {
		if cs.Indirect && cs.SourceCallerName == "use_dispatch" {
			indirectSites++
		}
	}
	if indirectSites < 2 {
		t.Errorf("indirect call sites attributed to use_dispatch = %d, want ≥2\nall sites: %+v",
			indirectSites, info.CallSites)
	}

	// Both sites dispatch through g_ops. GCC emits no DW_AT_call_target
	// for them (the loaded pointer is dead by the return PC), so the
	// member comes from the call instruction's R_X86_64_PC32 relocation
	// against g_ops plus the struct's member offsets.
	targets := map[string]string{} // field_hint → callee_type
	for _, cs := range info.CallSites {
		if cs.Indirect && cs.FieldHint != "" {
			targets[cs.FieldHint] = cs.CalleeType
		}
	}
	for _, want := range []string{"ops.add", "ops.mul"} {
		if targets[want] != "int (int, int)" {
			t.Errorf("field_hint %q → callee_type %q, want %q\nall: %v",
				want, targets[want], "int (int, int)", targets)
		}
	}
}

func TestReadLibDispatchImpls(t *testing.T) {
	info, err := dwarfingest.Read(filepath.Join(
		repoRoot(t), "testdata", "fixture", "builddir",
		"lib", "libshared.a.p", "dispatch_impls.c.o",
	))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// Struct ops declared in include/dispatch.h; find it.
	var ops *dwarfingest.StructInfo
	for i := range info.Structs {
		if info.Structs[i].Name == "ops" {
			ops = &info.Structs[i]
		}
	}
	if ops == nil {
		t.Fatal("missing struct ops")
	}
	if !strings.HasSuffix(ops.DeclFile, "include/dispatch.h") {
		t.Errorf("ops.DeclFile = %q, want *include/dispatch.h", ops.DeclFile)
	}
	if ops.DeclLine != 15 {
		t.Errorf("ops.DeclLine = %d, want 15", ops.DeclLine)
	}
	if ops.Kind != "struct" {
		t.Errorf("ops.Kind = %q, want struct", ops.Kind)
	}
	// Field offsets are what identify which slot of a dispatch table a
	// call goes through — the fact GCC's .devirt dump carried before v9.
	wantFields := []struct {
		name   string
		offset int
	}{{"add", 0}, {"mul", 8}, {"name", 16}, {"last_status", 24}}
	if len(ops.Fields) != len(wantFields) {
		t.Fatalf("ops.Fields = %d, want %d: %+v", len(ops.Fields), len(wantFields), ops.Fields)
	}
	for i, w := range wantFields {
		if ops.Fields[i].Name != w.name || ops.Fields[i].ByteOffset != w.offset {
			t.Errorf("field %d = %s@%d, want %s@%d",
				i, ops.Fields[i].Name, ops.Fields[i].ByteOffset, w.name, w.offset)
		}
	}

	// The enum plane: DW_TAG_enumeration_type with its enumerators, and
	// the typedef over it. Both reach DWARF purely because struct ops has
	// a member of that type — nothing in this TU names them otherwise.
	var st *dwarfingest.EnumInfo
	for i := range info.Enums {
		if info.Enums[i].Name == "op_status" {
			st = &info.Enums[i]
		}
	}
	if st == nil {
		t.Fatalf("missing enum op_status; got %+v", info.Enums)
	}
	wantConsts := map[string]int64{"OP_STATUS_OK": 0, "OP_STATUS_OVERFLOW": 7, "OP_STATUS_UNSUPPORTED": 9}
	if len(st.Constants) != len(wantConsts) {
		t.Errorf("op_status constants = %d, want %d: %+v", len(st.Constants), len(wantConsts), st.Constants)
	}
	for _, c := range st.Constants {
		want, ok := wantConsts[c.Name]
		if !ok {
			t.Errorf("unexpected enumerator %q", c.Name)
			continue
		}
		if c.Value != want {
			t.Errorf("%s = %d, want %d", c.Name, c.Value, want)
		}
	}

	var td *dwarfingest.TypedefInfo
	for i := range info.Typedefs {
		if info.Typedefs[i].Name == "op_status_t" {
			td = &info.Typedefs[i]
		}
	}
	if td == nil {
		t.Fatalf("missing typedef op_status_t; got %+v", info.Typedefs)
	}
	if td.Target != "enum op_status" {
		t.Errorf("op_status_t target = %q, want \"enum op_status\"", td.Target)
	}

	// g_ops variable at CU scope.
	var gOps *dwarfingest.VariableInfo
	for i := range info.Variables {
		if info.Variables[i].Name == "g_ops" {
			gOps = &info.Variables[i]
		}
	}
	if gOps == nil {
		t.Fatal("missing variable g_ops")
	}
	if gOps.Type != "const struct ops" {
		t.Errorf("g_ops.Type = %q, want %q", gOps.Type, "const struct ops")
	}
}

func findDefinitions(sps []dwarfingest.Subprogram) []dwarfingest.Subprogram {
	var out []dwarfingest.Subprogram
	for _, sp := range sps {
		if sp.Definition {
			out = append(out, sp)
		}
	}
	return out
}

func findDeclarations(sps []dwarfingest.Subprogram) []dwarfingest.Subprogram {
	var out []dwarfingest.Subprogram
	for _, sp := range sps {
		if sp.Declaration && !sp.Definition {
			out = append(out, sp)
		}
	}
	return out
}

// TestReadInlineInstances: the fixture holds one surviving inlined body
// per plane — use_dispatch folded into main by the IPA inliner, and
// scale_by_two folded into scaled_compute by the early inliner, which is
// the case no IPA dump can report.
func TestReadInlineInstances(t *testing.T) {
	for _, tc := range []struct {
		object string
		parts  []string
		callee string
		caller string
		line   int
	}{
		{"app1 main.c.o", []string{"app1", "app1.p", "main.c.o"}, "use_dispatch", "main", 14},
		{"shared_utils.c.o", []string{"lib", "libshared.a.p", "shared_utils.c.o"}, "scale_by_two", "scaled_compute", 28},
	} {
		t.Run(tc.object, func(t *testing.T) {
			args := append([]string{repoRoot(t), "testdata", "fixture", "builddir"}, tc.parts...)
			info, err := dwarfingest.Read(filepath.Join(args...))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			var hit *dwarfingest.InlineInstance
			for i, ii := range info.InlineInstances {
				if ii.CalleeName == tc.callee {
					hit = &info.InlineInstances[i]
					break
				}
			}
			if hit == nil {
				t.Fatalf("no inline instance for %s; got %+v", tc.callee, info.InlineInstances)
			}
			if hit.CallerName != tc.caller {
				t.Errorf("CallerName = %q, want %q", hit.CallerName, tc.caller)
			}
			if hit.Depth != 1 {
				t.Errorf("Depth = %d, want 1", hit.Depth)
			}
			if hit.ParentCalleeName != "" {
				t.Errorf("ParentCalleeName = %q, want empty at depth 1", hit.ParentCalleeName)
			}
			if hit.Line != tc.line {
				t.Errorf("Line = %d, want %d", hit.Line, tc.line)
			}
			if !strings.HasSuffix(hit.File, ".c") {
				t.Errorf("File = %q, want a .c path", hit.File)
			}
		})
	}
}
