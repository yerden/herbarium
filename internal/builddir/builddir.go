// Package builddir walks a Meson/ninja build directory and locates the
// per-object GCC dumps and per-target linker maps that herbarium ingests.
//
// The plan (§ Phase 1) requires that meson-private/ is skipped so
// sanity-check compile artifacts are never ingested as real TUs. Dump
// filenames on GCC 16 land as `<obj-basename>.c.NNNi.<pass>`; we glob
// on the suffix so pass numbers (which drift across GCC majors) do not
// leak into the crawler.
package builddir

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// ObjectArtifacts groups a .o with the sidecar files GCC/Meson dropped
// alongside it. Any missing sidecar is an empty string; ingest decides
// whether that is an error (preflight owns the required-set check).
type ObjectArtifacts struct {
	Object       string // absolute path to the .o
	CI           string // -fcallgraph-info output
	Cgraph       string // -fdump-ipa-cgraph
	Inline       string // -fdump-ipa-inline
	Devirt       string // -fdump-ipa-devirt
	ICF          string // -fdump-ipa-icf
	OptRecord    string // -fsave-optimization-record (gzipped JSON)
	Preprocessed string // .i (only when -save-temps is in effect)
}

// BuildDir is the crawler's result: every .o with its sidecars, plus any
// top-level .map file the user configured via `-Wl,-Map=` per the plan.
type BuildDir struct {
	Root       string
	Objects    []ObjectArtifacts
	LinkerMaps []string // absolute paths to .map files at the top of the builddir
}

// dirsToSkip lists Meson-internal directories the crawler must not enter.
// meson-private/ contains sanity-check compile output (plan touch-up from
// Phase 0). meson-info/ and meson-logs/ contain no .o files, but skipping
// them cuts walk time on real projects.
var dirsToSkip = map[string]bool{
	"meson-private": true,
	"meson-info":    true,
	"meson-logs":    true,
}

// Crawl walks root and returns every .o with its sidecar dumps, plus any
// top-level .map files. Returns an error only on I/O failure — a builddir
// that produced zero .o files is not itself an error (preflight will
// report that with a clearer message).
func Crawl(root string) (*BuildDir, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("builddir: abs %q: %w", root, err)
	}

	bd := &BuildDir{Root: absRoot}

	// Two passes: one for top-level .map files (single Glob), one deep
	// walk for .o files and their sidecars.
	maps, err := filepath.Glob(filepath.Join(absRoot, "*.map"))
	if err != nil {
		return nil, fmt.Errorf("builddir: glob maps: %w", err)
	}
	bd.LinkerMaps = maps

	// Collect .o paths first, then resolve sidecars in a second sweep so
	// the loop body stays a single-purpose visitor.
	var objects []string
	err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if dirsToSkip[d.Name()] && filepath.Dir(path) == absRoot {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".o") {
			objects = append(objects, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("builddir: walk %q: %w", absRoot, err)
	}

	for _, obj := range objects {
		bd.Objects = append(bd.Objects, resolveSidecars(obj))
	}
	return bd, nil
}

// resolveSidecars finds the GCC dump files sitting next to obj. Meson names
// its objects `<source>.<ext>.o` (e.g., `main.c.o`); GCC names its dumps
// `<source>.<ext>.NNNi.<pass>` and `<source>.<ext>.ci` (no NNNi suffix on
// the callgraph-info dump). We derive both bases from the .o and glob.
func resolveSidecars(obj string) ObjectArtifacts {
	art := ObjectArtifacts{Object: obj}

	dir := filepath.Dir(obj)
	// base = "main.c" for "main.c.o"
	base := strings.TrimSuffix(filepath.Base(obj), ".o")

	// .ci: single fixed filename, no pass number
	if p := filepath.Join(dir, base+".ci"); fileExists(p) {
		art.CI = p
	}
	// .i: preprocessed source, single fixed filename (with -save-temps)
	if p := filepath.Join(dir, base+".i"); fileExists(p) {
		art.Preprocessed = p
	}
	// The optimization record is gzipped JSON, not a numbered dump, so
	// it needs its own glob: <base>.c.opt-record.json.gz on GCC 16.
	if hits, err := filepath.Glob(filepath.Join(dir, base+"*opt-record.json.gz")); err == nil && len(hits) > 0 {
		art.OptRecord = hits[0]
	}
	// IPA dumps: <base>.NNNi.<pass> — but on GCC 16 we observe an extra
	// language suffix, so files land as <base>.c.NNNi.<pass>. Glob on the
	// pass suffix and accept whatever pass number the compiler assigned.
	for _, pass := range []struct {
		suffix string
		out    *string
	}{
		{".cgraph", &art.Cgraph},
		{".inline", &art.Inline},
		{".devirt", &art.Devirt},
		{".icf", &art.ICF},
	} {
		// Try both conventions: <base>.NNNi.pass and <base>.c.NNNi.pass.
		// The double-.c. form is what GCC 16 actually produces for us.
		hits, err := filepath.Glob(filepath.Join(dir, base+".*i"+pass.suffix))
		if err == nil && len(hits) > 0 {
			*pass.out = hits[0]
		}
	}
	return art
}

func fileExists(p string) bool {
	_, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	return true
}
