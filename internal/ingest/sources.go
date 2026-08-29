package ingest

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yerden/herbarium/internal/blobstore"
	"github.com/yerden/herbarium/internal/builddir"
	"github.com/yerden/herbarium/internal/mesonintrospect"
	"github.com/yerden/herbarium/internal/ninjadeps"
)

// SourcesSummary counts what Sources wrote so the collect summary can
// display it.
type SourcesSummary struct {
	Files          int // number of `sources` rows inserted/updated
	Blobs          int // number of new blobs written (post-dedup)
	Duplicates     int // number of Put calls whose content already existed
	Generated      int // count of files classified as generated (in-tree, sources.is_generated=1)
	ExternalFiles  int // rows written to external_sources (0 when no globs)
	ExternalBlobs  int // subset of Blobs contributed by external headers
	GeneratedFiles int // rows written to generated_sources (out-of-tree build outputs)
	GeneratedBlobs int // subset of Blobs contributed by generated_sources
	GlobMatchCount map[string]int
}

// SourcesOptions tunes packing. Strict enforces the risk-mitigation rule
// (herbarium-plan.md line 539): refuse if any source's mtime is newer
// than the .o that consumed it. Off by default because most invocations
// happen right after a build and mtime skew is normal for editors that
// touch-save.
type SourcesOptions struct {
	Strict          bool
	ExternalGlobs   []ExternalGlob
}

// Sources packs every source and header the build touched into the
// blob store and populates the `sources` table. See herbarium-plan.md
// § Phase 5 for the contract:
//
//   - Target sources + generated sources come from Meson introspection.
//   - Per-TU header sets come from ninja's binary .ninja_deps log,
//     keyed by each .o file's builddir-relative path.
//   - Any source referenced by the build but missing under
//     --project-root is a hard error (blob coverage and USR coverage
//     must stay in lockstep — see Appendix line 584).
//   - System headers (paths outside --project-root that are not
//     generated files under the builddir) are silently skipped: they
//     have no USRs anchored to them, so they need no blob either.
//   - is_generated=1 when the file came from Meson's generated_sources
//     array OR lives under the builddir at ingest time.
func Sources(db *sql.DB, bd *builddir.BuildDir, intro *mesonintrospect.Introspection, pr *PathResolver, opts SourcesOptions) (SourcesSummary, error) {
	// entry describes one file we intend to store.
	//
	//   relPath      — project-relative, forward slashes; the primary key
	//                  of the sources table
	//   absPath      — absolute path on disk (rooted at ProjectRoot)
	//   isGenerated  — 1 if from generated_sources or under builddir
	//   consumers    — absolute .o paths that referenced this file
	//                  (used for --strict mtime comparison)
	//   required     — true if this file MUST exist under ProjectRoot
	//                  (target-source list). When false, we skip missing
	//                  files rather than failing (deps log entries may
	//                  reference transient files that have since moved).
	type entry struct {
		relPath     string
		absPath     string
		isGenerated bool
		consumers   []string
		required    bool
	}
	entries := map[string]*entry{}

	// mark records that relPath needs to be packed. Later calls merge
	// generated/required flags monotonically (once-set stays set) and
	// append consumers.
	mark := func(relPath, absPath string, isGenerated, required bool, consumer string) {
		e, ok := entries[relPath]
		if !ok {
			e = &entry{relPath: relPath, absPath: absPath}
			entries[relPath] = e
		}
		if isGenerated {
			e.isGenerated = true
		}
		if required {
			e.required = true
		}
		if consumer != "" {
			e.consumers = append(e.consumers, consumer)
		}
	}

	// externalEntries is the parallel bucket for headers matched by
	// --include-external globs. Keyed by absolute path so duplicates from
	// many TUs collapse before we touch the filesystem.
	externalEntries := map[string]struct{}{}
	globHits := make(map[string]int, len(opts.ExternalGlobs))
	for _, g := range opts.ExternalGlobs {
		globHits[g.Raw()] = 0
	}

	// generatedEntries is the parallel bucket for build-tree files (files
	// under bd.Root that aren't in --project-root). Keyed by builddir-
	// relative path so the value is portable across machines.
	generatedEntries := map[string]string{} // builddirRel → absPath

	markGenerated := func(abs string) {
		rel, err := filepath.Rel(bd.Root, abs)
		if err != nil || strings.HasPrefix(rel, "..") {
			return
		}
		key := filepath.ToSlash(rel)
		if _, seen := generatedEntries[key]; !seen {
			generatedEntries[key] = abs
		}
	}

	// Pass 1: target sources + generated sources from Meson.
	for _, t := range intro.Targets {
		for _, abs := range t.Sources {
			r := pr.ToProjectRelative(abs)
			if !r.InProject {
				return SourcesSummary{}, fmt.Errorf(
					"ingest/sources: target %q source %q is outside --project-root %q; "+
						"herbarium refuses to index a builddir whose sources moved after configure",
					t.Name, abs, pr.ProjectRoot)
			}
			mark(r.Rel, filepath.Join(pr.ProjectRoot, filepath.FromSlash(r.Rel)), false, true, "")
		}
		for _, abs := range t.Generated {
			r := pr.ToProjectRelative(abs)
			if r.InProject {
				// In-tree builddir: keep the historical shape (goes to
				// sources with is_generated=1) so agents that grew up
				// on that layout keep working.
				mark(r.Rel, filepath.Join(pr.ProjectRoot, filepath.FromSlash(r.Rel)), true, true, "")
				continue
			}
			// Out-of-tree: pack via generated_sources if the file lives
			// under builddir, which it must for anything Meson labels
			// as generated. Truly-outside paths remain a hard error —
			// that's a genuinely broken introspection.
			absNative, err := filepath.Abs(abs)
			if err != nil {
				return SourcesSummary{}, fmt.Errorf(
					"ingest/sources: target %q generated source %q: abs: %w", t.Name, abs, err)
			}
			if !isUnderBuildDir(absNative, bd.Root) {
				return SourcesSummary{}, fmt.Errorf(
					"ingest/sources: target %q generated source %q is outside both --project-root %q and --builddir %q",
					t.Name, abs, pr.ProjectRoot, bd.Root)
			}
			markGenerated(absNative)
		}
	}

	// Pass 2: per-TU headers via .ninja_deps. Missing log is not fatal —
	// the source-only view (no headers) is still a valid index, and
	// preflight has already flagged the compile-side dumps that matter.
	log, err := ninjadeps.ReadForBuildDir(bd.Root)
	if err != nil {
		return SourcesSummary{}, err
	}
	if log != nil {
		for _, obj := range bd.Objects {
			// ninja key = obj path relative to builddir, forward slashes.
			relObj, err := filepath.Rel(bd.Root, obj.Object)
			if err != nil {
				continue
			}
			deps, ok := log.DepsFor(filepath.ToSlash(relObj))
			if !ok {
				continue
			}
			for _, dep := range deps {
				// deps are typically builddir-relative (like
				// "../lib/x.c") or absolute (system headers).
				r := pr.ToProjectRelative(dep)
				if !r.InProject {
					abs := dep
					if !filepath.IsAbs(abs) {
						abs = filepath.Join(bd.Root, abs)
					}
					abs = filepath.Clean(abs)
					// Under builddir → generated_sources unconditionally.
					// The build produced these files; agents inspecting
					// the artifact need them regardless of --include-external.
					if isUnderBuildDir(abs, bd.Root) {
						markGenerated(abs)
						continue
					}
					// Outside both project-root and builddir → external
					// glob or skip.
					for _, g := range opts.ExternalGlobs {
						if g.Match(abs) {
							externalEntries[abs] = struct{}{}
							globHits[g.Raw()]++
							break
						}
					}
					continue
				}
				absPath := filepath.Join(pr.ProjectRoot, filepath.FromSlash(r.Rel))
				isGen := isUnderBuildDir(absPath, bd.Root)
				mark(r.Rel, absPath, isGen, false, obj.Object)
			}
		}
	}

	// Determinism: sort keys before iterating so the blob insertion order
	// (and thus SQLite rowids) is stable across runs.
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	w, err := blobstore.New(db)
	if err != nil {
		return SourcesSummary{}, err
	}
	// Any error past this point → Rollback; blobstore is transactional.
	commit := false
	defer func() {
		if !commit {
			w.Rollback()
		}
	}()

	var sum SourcesSummary
	for _, k := range keys {
		e := entries[k]
		content, err := os.ReadFile(e.absPath)
		if err != nil {
			if os.IsNotExist(err) {
				if e.required {
					return SourcesSummary{}, fmt.Errorf(
						"ingest/sources: %s referenced by Meson introspection but missing under --project-root %q",
						e.relPath, pr.ProjectRoot)
				}
				// Header referenced by .ninja_deps but not present on
				// disk — likely a race with a cleanup step. Skip.
				continue
			}
			return SourcesSummary{}, fmt.Errorf("ingest/sources: read %s: %w", e.absPath, err)
		}

		if opts.Strict {
			if err := checkStrictMtime(e.absPath, e.consumers); err != nil {
				return SourcesSummary{}, err
			}
		}

		res, err := w.Put(e.relPath, content, e.isGenerated)
		if err != nil {
			return SourcesSummary{}, err
		}
		sum.Files++
		if res.Deduplicated {
			sum.Duplicates++
		} else {
			sum.Blobs++
		}
		if e.isGenerated {
			sum.Generated++
		}
	}

	// Generated (build-tree) files, sorted for determinism. Missing files
	// are silently skipped (a custom_target output may have been cleaned
	// between build and ingest).
	if len(generatedEntries) > 0 {
		keys := make([]string, 0, len(generatedEntries))
		for k := range generatedEntries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			abs := generatedEntries[k]
			content, err := os.ReadFile(abs)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return SourcesSummary{}, fmt.Errorf("ingest/sources: read generated %s: %w", abs, err)
			}
			res, err := w.PutGenerated(k, content)
			if err != nil {
				return SourcesSummary{}, err
			}
			sum.GeneratedFiles++
			if !res.Deduplicated {
				sum.GeneratedBlobs++
				sum.Blobs++
			} else {
				sum.Duplicates++
			}
		}
	}

	// External headers, in sorted order for determinism. Missing files
	// are silently skipped (same rationale as .ninja_deps entries above —
	// a header may have been renamed since the last build).
	if len(externalEntries) > 0 {
		absPaths := make([]string, 0, len(externalEntries))
		for p := range externalEntries {
			absPaths = append(absPaths, p)
		}
		sort.Strings(absPaths)
		for _, abs := range absPaths {
			content, err := os.ReadFile(abs)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return SourcesSummary{}, fmt.Errorf("ingest/sources: read external %s: %w", abs, err)
			}
			res, err := w.PutExternal(abs, content)
			if err != nil {
				return SourcesSummary{}, err
			}
			sum.ExternalFiles++
			if !res.Deduplicated {
				sum.ExternalBlobs++
				sum.Blobs++
			} else {
				sum.Duplicates++
			}
		}
	}

	// Zero-match glob is a hard error, on the same rationale as the
	// --target unknown-name check: silent no-op hides typos.
	var zero []string
	for pat, n := range globHits {
		if n == 0 {
			zero = append(zero, pat)
		}
	}
	if len(zero) > 0 {
		sort.Strings(zero)
		return SourcesSummary{}, fmt.Errorf(
			"ingest/sources: --include-external glob(s) matched zero headers: %s",
			strings.Join(zero, ", "))
	}
	sum.GlobMatchCount = globHits

	if err := w.Commit(); err != nil {
		return SourcesSummary{}, err
	}
	commit = true
	return sum, nil
}

// isUnderBuildDir reports whether abs is inside bdRoot on the filesystem
// (both absolute). Used to flag generated files even when Meson's
// generated_sources list doesn't mention them (e.g., configure-time
// substitutions that fall through to a regular target-source path).
func isUnderBuildDir(abs, bdRoot string) bool {
	rel, err := filepath.Rel(bdRoot, abs)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..") && rel != "."
}

// checkStrictMtime enforces the --strict rule from herbarium-plan.md
// line 539: the source's mtime must not be newer than any .o that
// consumed it. Fires on the first offender; the error message names both
// files so the user can decide whether to rebuild or drop --strict.
func checkStrictMtime(src string, consumers []string) error {
	if len(consumers) == 0 {
		return nil
	}
	sInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("ingest/sources: strict stat %s: %w", src, err)
	}
	srcMtime := sInfo.ModTime()
	for _, obj := range consumers {
		oInfo, err := os.Stat(obj)
		if err != nil {
			// If the .o vanished between crawl and now, don't second-
			// guess — let the strict check pass and let later stages
			// surface the real error.
			continue
		}
		if srcMtime.After(oInfo.ModTime()) {
			return fmt.Errorf(
				"ingest/sources: --strict: %s (mtime %s) is newer than its consumer %s (mtime %s); rerun `meson compile` before indexing",
				src, srcMtime.Format("2006-01-02T15:04:05"),
				obj, oInfo.ModTime().Format("2006-01-02T15:04:05"),
			)
		}
	}
	return nil
}
