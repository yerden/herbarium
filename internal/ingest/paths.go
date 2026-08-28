// Package ingest orchestrates per-TU dump parsing, USR synthesis, and
// database population for `herbarium collect`. Phase 2 populates the
// compiler-plane tables (symbols, call_edges, indirect_call_sites,
// inline_decisions, devirt_hints). Phases 3–5 add DWARF, link plane,
// and source blobs — this package is the join point.
package ingest

import (
	"fmt"
	"path/filepath"
	"strings"
)

// PathResolver converts paths from dumps (compile-CWD-relative or
// absolute) into project-relative paths for USR synthesis and blob
// keys. The plan requires that every stored source path exist under
// project-root — this resolver refuses paths that escape it (line 550
// of the appendix).
type PathResolver struct {
	BuildDir    string // absolute
	ProjectRoot string // absolute
}

// NewPathResolver returns a PathResolver with both fields absolutized.
func NewPathResolver(buildDir, projectRoot string) (*PathResolver, error) {
	bAbs, err := filepath.Abs(buildDir)
	if err != nil {
		return nil, fmt.Errorf("ingest: abs builddir %q: %w", buildDir, err)
	}
	pAbs, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("ingest: abs project-root %q: %w", projectRoot, err)
	}
	return &PathResolver{BuildDir: bAbs, ProjectRoot: pAbs}, nil
}

// ToProjectRelative resolves p — which came from a dump and is either
// absolute or relative to BuildDir — to a project-relative path with
// forward slashes and no leading "./". Returns InProject=false when the
// path lives outside project-root (a library header, /usr/include/*).
// Ingest tolerates those but doesn't create USRs anchored to them; they
// stay as bare declarations with an empty path.
type ResolvedPath struct {
	Rel       string
	InProject bool
}

// ToProjectRelative resolves p per the rules above.
func (pr *PathResolver) ToProjectRelative(p string) ResolvedPath {
	if p == "" {
		return ResolvedPath{}
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(pr.BuildDir, abs)
	}
	abs = filepath.Clean(abs)

	rel, err := filepath.Rel(pr.ProjectRoot, abs)
	if err != nil {
		return ResolvedPath{Rel: p, InProject: false}
	}
	// filepath.Rel yields "../..." when abs is outside ProjectRoot.
	if strings.HasPrefix(rel, "..") {
		return ResolvedPath{Rel: filepath.ToSlash(abs), InProject: false}
	}
	return ResolvedPath{Rel: filepath.ToSlash(rel), InProject: true}
}
