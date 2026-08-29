package ingest

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ExternalGlob matches an absolute path against a user-supplied pattern.
// Supported shapes (deliberately narrow — real callers write these by hand):
//
//	/prefix/**       recursive: any path under /prefix (inclusive: /prefix itself matches)
//	/prefix          exact match, or literal filepath.Match glob for a single segment
//
// Double-star is only recognized as a trailing /** — mid-pattern ** is not
// supported. Users who want fancier matching can pass multiple globs.
type ExternalGlob struct {
	raw    string
	prefix string // set when the pattern ends in /**; otherwise empty
	// If prefix is empty, use filepath.Match against raw for exact-ish matches.
}

// NewExternalGlob parses one --include-external argument. Empty or
// whitespace-only patterns are rejected up front so the collect CLI
// surfaces the mistake rather than silently disabling matching.
func NewExternalGlob(raw string) (ExternalGlob, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ExternalGlob{}, fmt.Errorf("empty --include-external pattern")
	}
	if !filepath.IsAbs(trimmed) {
		return ExternalGlob{}, fmt.Errorf("--include-external must be an absolute path: %q", raw)
	}
	if prefix, ok := strings.CutSuffix(trimmed, "/**"); ok {
		return ExternalGlob{raw: raw, prefix: prefix}, nil
	}
	if strings.Contains(trimmed, "**") {
		return ExternalGlob{}, fmt.Errorf("--include-external %q: '**' is only supported as a trailing /**", raw)
	}
	return ExternalGlob{raw: raw}, nil
}

// Match reports whether absPath satisfies the pattern.
func (g ExternalGlob) Match(absPath string) bool {
	if g.prefix != "" {
		if absPath == g.prefix {
			return true
		}
		return strings.HasPrefix(absPath, g.prefix+"/")
	}
	ok, _ := filepath.Match(g.raw, absPath)
	return ok
}

// Raw returns the original user-supplied pattern (for error messages).
func (g ExternalGlob) Raw() string { return g.raw }

// MatchesAnyExternalGlob returns true if any glob in gs matches absPath.
// Convenience for the ingest hot loop.
func MatchesAnyExternalGlob(absPath string, gs []ExternalGlob) bool {
	for _, g := range gs {
		if g.Match(absPath) {
			return true
		}
	}
	return false
}
