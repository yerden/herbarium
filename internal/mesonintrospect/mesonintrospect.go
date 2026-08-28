// Package mesonintrospect reads the JSON files Meson persists under
// builddir/meson-info/ during `meson setup`. Herbarium never invokes
// `meson introspect` — Meson writes these files eagerly and they are the
// canonical build-system view.
package mesonintrospect

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Target is the herbarium-facing shape of one Meson target. It normalizes
// Meson's `type` strings ("static library" → "static_library") to match the
// enum documented in the plan's schema.
type Target struct {
	Name          string
	ID            string
	Kind          string // 'executable' | 'static_library' | 'shared_library' | other
	DefinedIn     string
	Filenames     []string
	Sources       []string // union of all c-language source groups' `sources` (absolute paths as Meson emits them)
	Generated     []string // union of `generated_sources`
	CompileParams []string // first c-language source group's `parameters`
	CompilerCmd   []string // first c-language source group's `compiler` (e.g., ["/usr/bin/ccache","cc"])
	LinkCmd       []string // linker exelist + parameters, joined display-only
	Installed     bool
	Subproject    string
}

// CompilerInfo is the host-machine compiler entry from intro-compilers.json.
type CompilerInfo struct {
	ID          string // 'gcc', 'clang', ...
	Version     string // '16.2.1'
	FullVersion string // 'cc (GCC) 16.2.1 20260810'
	Exelist     []string
	LinkerID    string
}

// Introspection bundles everything herbarium reads from meson-info/.
type Introspection struct {
	MesonVersion string
	SourceDir    string
	BuildDir     string
	Targets      []Target
	CCompiler    CompilerInfo // host-machine C compiler; zero-value if the project doesn't use C
	Dependencies []Dependency
}

// Dependency mirrors an entry in intro-dependencies.json — external libs.
// Kept minimal for Phase 1; add fields as later phases need them.
type Dependency struct {
	Name    string
	Version string
	Type    string
}

// Load reads all required intro-*.json files from the given builddir.
// Returns an error naming the missing file if Meson has not been run.
func Load(builddir string) (*Introspection, error) {
	infoDir := filepath.Join(builddir, "meson-info")

	if _, err := os.Stat(infoDir); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("mesonintrospect: %s not found — run `meson setup` first", infoDir)
	}

	// meson-info.json — anchor for version + directory paths.
	var mi struct {
		MesonVersion struct {
			Full string `json:"full"`
		} `json:"meson_version"`
		Directories struct {
			Source string `json:"source"`
			Build  string `json:"build"`
		} `json:"directories"`
	}
	if err := readJSON(filepath.Join(infoDir, "meson-info.json"), &mi); err != nil {
		return nil, err
	}

	// intro-targets.json — the interesting one.
	var rawTargets []rawTarget
	if err := readJSON(filepath.Join(infoDir, "intro-targets.json"), &rawTargets); err != nil {
		return nil, err
	}

	// intro-compilers.json — used by preflight for GCC version gating.
	var rawComp struct {
		Host map[string]rawCompiler `json:"host"`
	}
	if err := readJSON(filepath.Join(infoDir, "intro-compilers.json"), &rawComp); err != nil {
		return nil, err
	}

	// intro-dependencies.json — may be an empty array.
	var rawDeps []Dependency
	if err := readJSON(filepath.Join(infoDir, "intro-dependencies.json"), &rawDeps); err != nil {
		return nil, err
	}

	out := &Introspection{
		MesonVersion: mi.MesonVersion.Full,
		SourceDir:    mi.Directories.Source,
		BuildDir:     mi.Directories.Build,
		Dependencies: rawDeps,
	}
	if c, ok := rawComp.Host["c"]; ok {
		out.CCompiler = CompilerInfo{
			ID:          c.ID,
			Version:     c.Version,
			FullVersion: c.FullVersion,
			Exelist:     c.Exelist,
			LinkerID:    c.LinkerID,
		}
	}
	for _, rt := range rawTargets {
		out.Targets = append(out.Targets, rt.toTarget())
	}
	return out, nil
}

type rawTarget struct {
	Name       string      `json:"name"`
	ID         string      `json:"id"`
	Type       string      `json:"type"`
	DefinedIn  string      `json:"defined_in"`
	Filename   []string    `json:"filename"`
	Sources    []rawSrcGrp `json:"target_sources"`
	Installed  bool        `json:"installed"`
	Subproject *string     `json:"subproject"`
}

type rawSrcGrp struct {
	Language         string   `json:"language"`
	Compiler         []string `json:"compiler"`
	Linker           []string `json:"linker"`
	Parameters       []string `json:"parameters"`
	Sources          []string `json:"sources"`
	GeneratedSources []string `json:"generated_sources"`
}

type rawCompiler struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	FullVersion string   `json:"full_version"`
	Exelist     []string `json:"exelist"`
	LinkerID    string   `json:"linker_id"`
}

func (rt rawTarget) toTarget() Target {
	t := Target{
		Name:      rt.Name,
		ID:        rt.ID,
		Kind:      normalizeKind(rt.Type),
		DefinedIn: rt.DefinedIn,
		Filenames: rt.Filename,
		Installed: rt.Installed,
	}
	if rt.Subproject != nil {
		t.Subproject = *rt.Subproject
	}
	for _, g := range rt.Sources {
		if g.Language == "c" {
			t.Sources = append(t.Sources, g.Sources...)
			t.Generated = append(t.Generated, g.GeneratedSources...)
			if t.CompilerCmd == nil {
				t.CompilerCmd = g.Compiler
				t.CompileParams = g.Parameters
			}
		}
		if len(g.Linker) > 0 && t.LinkCmd == nil {
			t.LinkCmd = append(append([]string{}, g.Linker...), g.Parameters...)
		}
	}
	return t
}

func normalizeKind(mesonType string) string {
	switch mesonType {
	case "executable":
		return "executable"
	case "static library":
		return "static_library"
	case "shared library":
		return "shared_library"
	default:
		return mesonType
	}
}

func readJSON(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("mesonintrospect: open %s: %w", path, err)
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(dst); err != nil {
		return fmt.Errorf("mesonintrospect: decode %s: %w", path, err)
	}
	return nil
}
