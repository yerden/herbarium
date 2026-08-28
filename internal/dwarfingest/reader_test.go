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
	if ops.DeclLine != 4 {
		t.Errorf("ops.DeclLine = %d, want 4", ops.DeclLine)
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
