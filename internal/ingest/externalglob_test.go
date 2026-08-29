package ingest_test

import (
	"testing"

	"github.com/yerden/herbarium/internal/ingest"
)

func TestExternalGlob(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		// Trailing /** matches any descendant, plus the prefix root itself.
		{"/usr/include/**", "/usr/include/stdio.h", true},
		{"/usr/include/**", "/usr/include/bits/types.h", true},
		{"/usr/include/**", "/usr/include", true},
		{"/usr/include/**", "/usr/local/include/stdio.h", false},
		{"/usr/include/**", "/usr/includes/stdio.h", false}, // no partial prefix match

		// No wildcards → exact literal match.
		{"/usr/include/stdio.h", "/usr/include/stdio.h", true},
		{"/usr/include/stdio.h", "/usr/include/other.h", false},

		// Vendored deps under a sibling path.
		{"/home/x/vendored/**", "/home/x/vendored/foo/bar.h", true},
		{"/home/x/vendored/**", "/home/x/other/foo.h", false},
	}
	for _, tc := range cases {
		g, err := ingest.NewExternalGlob(tc.pattern)
		if err != nil {
			t.Errorf("NewExternalGlob(%q): %v", tc.pattern, err)
			continue
		}
		if got := g.Match(tc.path); got != tc.want {
			t.Errorf("%q.Match(%q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestExternalGlobRejects(t *testing.T) {
	cases := []string{
		"",                        // empty
		"   ",                     // whitespace
		"usr/include/**",          // not absolute
		"/usr/**/stdio.h",         // mid-pattern **
		"/usr/**/*.h",             // mid-pattern **
	}
	for _, pat := range cases {
		if _, err := ingest.NewExternalGlob(pat); err == nil {
			t.Errorf("NewExternalGlob(%q) accepted; want error", pat)
		}
	}
}
