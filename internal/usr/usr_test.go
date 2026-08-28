package usr_test

import (
	"testing"

	"github.com/yerden/herbarium/internal/usr"
)

func TestFunctionUSR(t *testing.T) {
	cases := []struct {
		name string
		path string
		sym  string
		want string
	}{
		{"external", "", "main", "c:@F@main"},
		{"static in .c", "src/net/conn.c", "helper", "c:src/net/conn.c@F@helper"},
		{"static-inline in .h", "include/utils.h", "inline_swap", "c:include/utils.h@F@inline_swap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usr.Function(tc.path, tc.sym); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVariableUSR(t *testing.T) {
	if usr.Variable("", "errno") != "c:@V@errno" {
		t.Fail()
	}
	if usr.Variable("lib/dispatch_impls.c", "g_ops") != "c:lib/dispatch_impls.c@V@g_ops" {
		t.Fail()
	}
}

func TestTypedefStructUnionEnum(t *testing.T) {
	if usr.Typedef("include/types.h", "size32_t") != "c:include/types.h@T@size32_t" {
		t.Fail()
	}
	if usr.Struct("include/dispatch.h", "ops", 4, 8) != "c:include/dispatch.h@S@ops" {
		t.Fail()
	}
	// Anonymous struct/union/enum use __anon_line_col.
	if got := usr.Struct("src/x.c", "", 12, 3); got != "c:src/x.c@S@__anon_12_3" {
		t.Errorf("anon struct = %q", got)
	}
	if got := usr.Union("src/x.c", "", 1, 2); got != "c:src/x.c@U@__anon_1_2" {
		t.Errorf("anon union = %q", got)
	}
	if got := usr.Enum("src/x.c", "", 5, 6); got != "c:src/x.c@E@__anon_5_6" {
		t.Errorf("anon enum = %q", got)
	}
}

func TestEnumMember(t *testing.T) {
	if got := usr.EnumMember("src/x.c", "color", 1, 1, "RED"); got != "c:src/x.c@E@color@RED" {
		t.Errorf("member = %q", got)
	}
	if got := usr.EnumMember("src/x.c", "", 3, 7, "RED"); got != "c:src/x.c@E@__anon_3_7@RED" {
		t.Errorf("anon-enum member = %q", got)
	}
}

func TestField(t *testing.T) {
	s := usr.Struct("include/dispatch.h", "ops", 4, 8)
	if got := usr.Field(s, "add"); got != "c:include/dispatch.h@S@ops@F@add" {
		t.Errorf("field = %q", got)
	}
}

func TestClone(t *testing.T) {
	cases := []struct {
		in            string
		wantBase      string
		wantSuffix    string
		wantIsClone   bool
	}{
		{"use_dispatch.constprop", "use_dispatch", ".constprop", true},
		{"use_dispatch.constprop.0", "use_dispatch", ".constprop.0", true},
		{"helper.isra.1", "helper", ".isra.1", true},
		{"foo.part.2", "foo", ".part.2", true},
		{"main", "main", "", false},
		{"add_ints", "add_ints", "", false},
		{"my.function.with.dots", "my.function.with.dots", "", false}, // no known suffix
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			base, suf, ok := usr.Clone(tc.in)
			if ok != tc.wantIsClone {
				t.Errorf("ok = %v, want %v", ok, tc.wantIsClone)
			}
			if base != tc.wantBase {
				t.Errorf("base = %q, want %q", base, tc.wantBase)
			}
			if suf != tc.wantSuffix {
				t.Errorf("suffix = %q, want %q", suf, tc.wantSuffix)
			}
		})
	}
}

func TestNormalizeProjectPath(t *testing.T) {
	cases := map[string]string{
		"src/x.c":       "src/x.c",
		"./src/x.c":     "src/x.c",
		"src/x.c/":      "src/x.c",
		`src\x.c`:       "src/x.c",
		"include/foo.h": "include/foo.h",
	}
	for in, want := range cases {
		if got := usr.NormalizeProjectPath(in); got != want {
			t.Errorf("NormalizeProjectPath(%q) = %q, want %q", in, got, want)
		}
	}
}
