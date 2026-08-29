package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
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
		targets = fs.String("target", "", "comma-separated list of Meson target names to include; empty means all. Skips nm/objdump/map work for other targets — compiler-plane ingest (symbols, cgraph edges) still covers every TU.")
	)
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
	if err := filterTargets(intro, *targets); err != nil {
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
	// Phase 7 and uses a distinct code path).
	if _, err := os.Stat(*out); err == nil {
		fmt.Fprintf(os.Stderr, "collect: %s already exists; remove it or pass --out to a new path\n", *out)
		return 1
	}

	db, err := store.Open(*out)
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
		{"indexed_at", time.Now().UTC().Format(time.RFC3339)},
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
	linkSum, err := ingest.Link(db, bd, intro, pr, targetIDs)
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
	fmt.Printf("  signatures:          %d\n", dwarfSum.Signatures)
	fmt.Printf("  decl locations:      %d\n", dwarfSum.DeclLocations)
	fmt.Printf("  indirect sites:      %d\n", dwarfSum.IndirectSites)
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

// stringSliceFlag lets flag.Var collect multiple --include-external
// occurrences into an ordered list. Preserves duplicates so the user's
// spelling shows up verbatim in error messages.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

// filterTargets restricts intro.Targets in place to the comma-separated
// set in spec. Empty spec keeps every target. Unknown names are a hard
// error (loud rather than silently indexing nothing) — the message lists
// available target names so the user can correct their typo.
func filterTargets(intro *mesonintrospect.Introspection, spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	wanted := map[string]bool{}
	for name := range strings.SplitSeq(spec, ",") {
		if n := strings.TrimSpace(name); n != "" {
			wanted[n] = true
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
