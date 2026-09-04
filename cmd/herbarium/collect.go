package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yerden/herbarium/internal/builddir"
	"github.com/yerden/herbarium/internal/ingest"
	"github.com/yerden/herbarium/internal/mesonintrospect"
	"github.com/yerden/herbarium/internal/preflight"
	"github.com/yerden/herbarium/internal/store"
)

func runCollect(args []string) int {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	var (
		bdir    = fs.String("builddir", "", "Meson build directory (required)")
		proot   = fs.String("project-root", "", "project source root (required)")
		out     = fs.String("out", "herbarium.hbr", "output .hbr file")
		strict  = fs.Bool("strict", false, "refuse to pack sources whose mtime is newer than their .o (per herbarium-plan.md Risks)")
		replace = fs.Bool("replace", false,
			"overwrite an existing --out. The new index is built beside it and renamed into place, "+
				"so a `herbarium serve` already holding the old file keeps answering from it until "+
				"its reload_index tool reopens the path.")
	)
	var targets stringSliceFlag
	fs.Var(&targets, "target",
		"Meson target name to include; empty means all. Repeatable, and each occurrence may itself be a comma-separated list "+
			"(e.g. --target app1 --target app2 or --target app1,app2). THE MAIN LEVER ON COLLECT TIME: every linked binary "+
			"is disassembled in full, so N executables sharing a static library pay for that library N times. This skips "+
			"nm/objdump/map work for other targets, while compiler-plane ingest (symbols, cgraph edges, DWARF) and source "+
			"packing still cover every TU — a fast slice, not a partial index. Only the post-link view narrows.")
	var externalGlobs stringSliceFlag
	fs.Var(&externalGlobs, "include-external",
		"Absolute-path glob pointing at headers outside --project-root to pack into external_sources. "+
			"Repeatable. Supports a trailing /** for recursive matches (e.g. /usr/include/**). "+
			"Zero-match globs are a hard error.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	parsedGlobs := make([]ingest.ExternalGlob, 0, len(externalGlobs))
	for _, raw := range externalGlobs {
		g, err := ingest.NewExternalGlob(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "collect:", err)
			return 2
		}
		parsedGlobs = append(parsedGlobs, g)
	}
	if *bdir == "" || *proot == "" {
		fmt.Fprintln(os.Stderr, "collect: --builddir and --project-root are required")
		fs.Usage()
		return 2
	}

	intro, err := mesonintrospect.Load(*bdir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := filterTargets(intro, targets); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	bd, err := builddir.Crawl(*bdir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	report := preflight.Check(intro, bd)
	if !report.Ok {
		fmt.Fprint(os.Stderr, report.FormatUserMessage(*bdir))
		return 1
	}

	// Refuse to clobber an existing .hbr silently — the plan treats each
	// index run as producing a fresh artifact (incremental re-ingest is
	// Phase 7 and uses a distinct code path). --replace is the opt-in for
	// the agent loop, where re-collecting over the served path is the
	// point.
	if _, err := os.Stat(*out); err == nil && !*replace {
		fmt.Fprintf(os.Stderr, "collect: %s already exists; pass --replace to overwrite it, remove it, or pass --out to a new path\n", *out)
		return 1
	}

	// Build beside the destination and rename in at the end. Two
	// properties come out of that: a failed collect leaves no .hbr at all
	// rather than a stub, and --replace never exposes a half-written file
	// to a serve process — rename is atomic, and a reader holding the old
	// path keeps its inode until it reopens.
	tmpPath, err := reserveTempIndex(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer removeIndexFiles(tmpPath)

	db, err := store.Open(tmpPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer db.Close()
	if err := store.Init(db); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// Stamp meta with what we know now. The rest (build_config_hash,
	// project_root_hint) lands as ingest phases populate their tables.
	stamps := [][2]string{
		{"herbarium_version", Version},
		{"gcc_version", intro.CCompiler.Version},
		{"meson_version", intro.MesonVersion},
		// Sub-second precision so reload_index can tell "the agent
		// re-collected" from "the agent forgot to": two collects of a
		// small project can land in the same wall-clock second.
		{"indexed_at", time.Now().UTC().Format(time.RFC3339Nano)},
		{"project_root_hint", *proot},
	}
	for _, kv := range stamps {
		if _, err := db.Exec(
			`INSERT INTO meta(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv[0], kv[1],
		); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	pr, err := ingest.NewPathResolver(*bdir, *proot)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sum, err := ingest.Compiler(db, bd, pr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dwarfSum, err := ingest.DWARF(db, bd, pr, sum.IDByUSR)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	targetIDs, err := ingest.Targets(db, intro, pr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	linkSum, err := ingest.Link(db, bd, intro, pr, targetIDs, sum.ObjectToSource)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	srcSum, err := ingest.Sources(db, bd, intro, pr, ingest.SourcesOptions{
		Strict:        *strict,
		ExternalGlobs: parsedGlobs,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// Close before the rename: the -wal/-shm sidecars are only folded
	// into the main file and unlinked when the last connection drops, so
	// renaming a still-open database would move an incomplete artifact.
	if err := db.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "collect: closing index:", err)
		return 1
	}
	// A shipped .hbr is read-only in the truest sense: sealing it out of
	// WAL is what lets serve open it from a directory it cannot write.
	if err := store.SealForReading(tmpPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.Rename(tmpPath, *out); err != nil {
		fmt.Fprintln(os.Stderr, "collect:", err)
		return 1
	}

	fmt.Printf("herbarium collect: %s written\n", *out)
	fmt.Printf("  builddir:            %s\n", bd.Root)
	fmt.Printf("  targets:             %d\n", len(intro.Targets))
	fmt.Printf("  objects:             %d\n", len(bd.Objects))
	fmt.Printf("  maps:                %d\n", len(bd.LinkerMaps))
	fmt.Printf("  gcc:                 %s\n", intro.CCompiler.Version)
	fmt.Printf("  meson:               %s\n", intro.MesonVersion)
	fmt.Printf("  symbols:             %d\n", sum.Symbols)
	fmt.Printf("  cgraph call edges:   %d\n", sum.CallEdges)
	fmt.Printf("  inline decisions:    %d\n", sum.InlineDecisions)
	fmt.Printf("  inline records:      %d\n", sum.InlineRecords)
	fmt.Printf("  icf groups:          %d\n", sum.ICFGroups)
	fmt.Printf("  signatures:          %d\n", dwarfSum.Signatures)
	fmt.Printf("  decl locations:      %d\n", dwarfSum.DeclLocations)
	fmt.Printf("  def locations:       %d\n", dwarfSum.DefLocations)
	fmt.Printf("  indirect sites:      %d\n", dwarfSum.IndirectSites)
	fmt.Printf("  inline instances:    %d\n", dwarfSum.InlineInstances)
	fmt.Printf("  link resolutions:    %d\n", linkSum.LinkResolutions)
	fmt.Printf("  objdump call edges:  %d\n", linkSum.ObjdumpEdges)
	fmt.Printf("  source files packed: %d (%d new blobs, %d deduped, %d generated)\n",
		srcSum.Files, srcSum.Blobs, srcSum.Duplicates, srcSum.Generated)
	if srcSum.GeneratedFiles > 0 {
		fmt.Printf("  generated files:     %d (%d new blobs)\n", srcSum.GeneratedFiles, srcSum.GeneratedBlobs)
	}
	if srcSum.ExternalFiles > 0 {
		fmt.Printf("  external headers:    %d (%d new blobs)\n", srcSum.ExternalFiles, srcSum.ExternalBlobs)
	}
	return 0
}

// reserveTempIndex creates the scratch file collect builds into. It
// lives in the destination's directory so the closing rename stays
// within one filesystem — os.Rename is only atomic there, and atomicity
// is the whole point of the detour.
//
// This open-and-retry loop is os.CreateTemp's, inlined for one reason:
// CreateTemp hardcodes 0600, and os.Rename carries the scratch file's
// mode through to the finished .hbr. Every index therefore came out
// readable only by the user who collected it — not a decision anyone
// made, just the rename detour leaking its temp-file convention, and
// the 0600 half of the sudo-collect failure described in CLAUDE.md
// under Reloading a live session. Mode has to reach open(2) for the
// kernel to apply the caller's umask (chmod would not consult it, and
// 0666 via chmod would be world-writable), so 0666 is passed here and
// masked down to the same 0644 SQLite produced before this detour
// existed.
func reserveTempIndex(out string) (string, error) {
	dir := filepath.Dir(out)
	prefix := filepath.Join(dir, "."+filepath.Base(out)+".tmp-")

	for attempt := 0; ; attempt++ {
		name := prefix + strconv.FormatUint(rand.Uint64(), 36)
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
		if errors.Is(err, fs.ErrExist) {
			// Same bound os.CreateTemp uses: past this many collisions the
			// cause is a wedged directory, not bad luck, and looping is worse
			// than reporting it.
			if attempt < 10000 {
				continue
			}
			return "", fmt.Errorf("collect: creating scratch index in %s: %w", dir, fs.ErrExist)
		}
		if err != nil {
			return "", fmt.Errorf("collect: creating scratch index in %s: %w", dir, err)
		}
		// SQLite wants to open the path itself; a zero-length file is a valid
		// empty database, so handing over the name is enough.
		if err := f.Close(); err != nil {
			os.Remove(name)
			return "", fmt.Errorf("collect: %w", err)
		}
		return name, nil
	}
}

// removeIndexFiles cleans up a scratch index and its journal sidecars.
// All three are expected to be gone on the success path (renamed away,
// and the sidecars unlinked by the final Close), so errors are ignored.
func removeIndexFiles(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}
}

// stringSliceFlag lets flag.Var collect multiple --include-external
// occurrences into an ordered list. Preserves duplicates so the user's
// spelling shows up verbatim in error messages.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

// filterTargets restricts intro.Targets in place to the union of every
// --target occurrence. Each entry may itself be a comma-separated list.
// Empty specs keep every target. Unknown names are a hard error (loud
// rather than silently indexing nothing) — the message lists available
// target names so the user can correct their typo.
func filterTargets(intro *mesonintrospect.Introspection, specs []string) error {
	wanted := map[string]bool{}
	for _, spec := range specs {
		for name := range strings.SplitSeq(spec, ",") {
			if n := strings.TrimSpace(name); n != "" {
				wanted[n] = true
			}
		}
	}
	if len(wanted) == 0 {
		return nil
	}

	kept := intro.Targets[:0]
	seen := map[string]bool{}
	for _, t := range intro.Targets {
		if wanted[t.Name] {
			kept = append(kept, t)
			seen[t.Name] = true
		}
	}
	var missing []string
	for name := range wanted {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		known := make([]string, 0, len(intro.Targets))
		for _, t := range intro.Targets {
			known = append(known, t.Name)
		}
		sort.Strings(known)
		return fmt.Errorf("collect: --target names not found in this builddir: %s\n  known targets: %s",
			strings.Join(missing, ", "), strings.Join(known, ", "))
	}
	intro.Targets = kept
	return nil
}
