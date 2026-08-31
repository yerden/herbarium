// Package preflight validates that a builddir was prepared with the flags
// herbarium requires. Its user-facing job — per the plan's Risks section
// — is to name the specific missing flag and reprint the corrected
// `meson setup` command line rather than silently ingest a degraded index.
package preflight

import (
	"debug/elf"
	"fmt"
	"strconv"
	"strings"

	"github.com/yerden/herbarium/internal/builddir"
	"github.com/yerden/herbarium/internal/mesonintrospect"
)

// MinGCCMajor is the lowest GCC major we accept. The plan pins GCC ≥ 10
// because `-fcallgraph-info` was introduced in GCC 10.
const MinGCCMajor = 10

// The c_args herbarium requires, kept in one place so the preflight report
// and any doc generator stay in sync. Every flag here is codegen-inert —
// -g emits DWARF, the rest only write dump files beside the object — so a
// builddir configured this way produces byte-identical .text to a stock
// build. That matters: the index describes the binary the user actually
// ships, which is the whole point of indexing compiler output.
const RecommendedCArgs = "-g -gcolumn-info -fcallgraph-info=su,da " +
	"-fdump-ipa-cgraph -fdump-ipa-inline -fdump-ipa-devirt -fdump-ipa-icf"

// OptionalCallGraphCArgs is deliberately NOT part of RecommendedCArgs and is
// not gated by any check below. It keeps single-caller statics out-of-line so
// they survive as distinct .cgraph nodes, which costs a real (if narrow)
// divergence from the shipped binary — the one flag in herbarium's set that
// changes codegen. Whether legibility is worth that divergence is the user's
// call, so the report offers it rather than demanding it.
const OptionalCallGraphCArgs = "-fno-inline-functions-called-once"

// Finding describes one thing wrong with the builddir. Kind is a stable
// enum callers can branch on; Detail and FixHint are human-readable.
type Finding struct {
	Kind    string
	Detail  string
	FixHint string
}

// Kind values.
const (
	KindGCCTooOld     = "gcc_too_old"
	KindNoTargets     = "no_targets"
	KindMissingCI     = "missing_ci"
	KindMissingCgraph = "missing_cgraph"
	KindNoDebugInfo   = "no_debug_info"
)

// Report is the aggregate result. Ok is true iff Findings is empty.
type Report struct {
	Ok           bool
	GCCVersion   string
	MesonVersion string
	Findings     []Finding
}

// Check runs every gate against the given introspection + builddir crawl.
// It never returns an error — a broken builddir is reported as Findings,
// not as a Go error, so the caller (cmd/herbarium collect) can render
// them all in one message.
func Check(intro *mesonintrospect.Introspection, bd *builddir.BuildDir) *Report {
	r := &Report{
		GCCVersion:   intro.CCompiler.Version,
		MesonVersion: intro.MesonVersion,
	}

	if major, ok := parseMajor(intro.CCompiler.Version); !ok || major < MinGCCMajor {
		r.Findings = append(r.Findings, Finding{
			Kind: KindGCCTooOld,
			Detail: fmt.Sprintf(
				"GCC %q is below the minimum GCC %d required for -fcallgraph-info",
				intro.CCompiler.Version, MinGCCMajor),
			FixHint: fmt.Sprintf("upgrade GCC to %d.x or later, or configure Meson to use a newer compiler", MinGCCMajor),
		})
	}

	if len(bd.Objects) == 0 {
		r.Findings = append(r.Findings, Finding{
			Kind:    KindNoTargets,
			Detail:  "no .o files under builddir — has `meson compile` run?",
			FixHint: "run `meson compile -C " + bd.Root + "` before `herbarium collect`",
		})
		// The remaining checks are pointless without objects.
		r.Ok = len(r.Findings) == 0
		return r
	}

	// A single missing dump kind is enough to conclude the flag was not
	// supplied globally; scan every .o so the report can name each
	// affected TU if the user later wants that detail.
	var missingCI, missingCgraph []string
	for _, o := range bd.Objects {
		if o.CI == "" {
			missingCI = append(missingCI, o.Object)
		}
		if o.Cgraph == "" {
			missingCgraph = append(missingCgraph, o.Object)
		}
	}
	if len(missingCI) > 0 {
		r.Findings = append(r.Findings, Finding{
			Kind:    KindMissingCI,
			Detail:  fmt.Sprintf(".ci dump missing for %d/%d objects (sample: %s)", len(missingCI), len(bd.Objects), missingCI[0]),
			FixHint: "add `-fcallgraph-info=su,da` to c_args and rebuild",
		})
	}
	if len(missingCgraph) > 0 {
		r.Findings = append(r.Findings, Finding{
			Kind:    KindMissingCgraph,
			Detail:  fmt.Sprintf(".cgraph dump missing for %d/%d objects (sample: %s)", len(missingCgraph), len(bd.Objects), missingCgraph[0]),
			FixHint: "add `-fdump-ipa-cgraph` (and the other -fdump-ipa-* flags) to c_args and rebuild",
		})
	}

	// -g check: sample the first .o and look for a .debug_info section.
	sample := bd.Objects[0].Object
	if hasDebug, err := hasDebugInfo(sample); err != nil {
		// Treat unreadable .o as a preflight failure — better to fail loud
		// than to ingest against a mystery builddir.
		r.Findings = append(r.Findings, Finding{
			Kind:    KindNoDebugInfo,
			Detail:  fmt.Sprintf("cannot inspect ELF sections of %s: %v", sample, err),
			FixHint: "verify the file is a regular ELF object",
		})
	} else if !hasDebug {
		r.Findings = append(r.Findings, Finding{
			Kind:    KindNoDebugInfo,
			Detail:  fmt.Sprintf("no .debug_info section in %s — was -g in effect?", sample),
			FixHint: "add `-g -gcolumn-info` to c_args and rebuild",
		})
	}

	r.Ok = len(r.Findings) == 0
	return r
}

// FormatUserMessage renders a report as the multi-line message the collect
// subcommand prints on failure. The recommended `meson setup` line is
// verbatim from the plan's Prerequisites section so a copy-paste fixes
// things in one shot.
func (r *Report) FormatUserMessage(builddir string) string {
	if r.Ok {
		return fmt.Sprintf("preflight ok (GCC %s, Meson %s)", r.GCCVersion, r.MesonVersion)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "herbarium preflight failed for %s\n", builddir)
	fmt.Fprintf(&b, "  detected: GCC=%s Meson=%s\n\n", r.GCCVersion, r.MesonVersion)
	for i, f := range r.Findings {
		fmt.Fprintf(&b, "  %d. [%s] %s\n     fix: %s\n", i+1, f.Kind, f.Detail, f.FixHint)
	}
	fmt.Fprintf(&b, "\nRecommended meson setup command:\n")
	fmt.Fprintf(&b, "  meson setup %s \\\n", builddir)
	fmt.Fprintf(&b, "    --buildtype=debugoptimized \\\n")
	fmt.Fprintf(&b, "    -Dc_args=%q\n", RecommendedCArgs)
	fmt.Fprintf(&b, "\nEvery flag above is codegen-inert: .text stays byte-identical to a stock build.\n")
	fmt.Fprintf(&b, "Optionally append %s to keep single-caller statics\n", OptionalCallGraphCArgs)
	fmt.Fprintf(&b, "out-of-line so they survive as distinct call-graph nodes. It is not required, and it\n")
	fmt.Fprintf(&b, "is the one flag that changes the generated code.\n")
	return b.String()
}

// parseMajor takes a version string like "16.2.1" and returns (16, true).
func parseMajor(v string) (int, bool) {
	if v == "" {
		return 0, false
	}
	first, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(first)
	if err != nil {
		return 0, false
	}
	return n, true
}

// hasDebugInfo returns true iff obj is a valid ELF file with a
// .debug_info section. We use debug/elf from stdlib instead of shelling
// out to readelf — smaller, faster, and no external dep.
func hasDebugInfo(obj string) (bool, error) {
	f, err := elf.Open(obj)
	if err != nil {
		return false, err
	}
	defer f.Close()
	for _, s := range f.Sections {
		if s.Name == ".debug_info" {
			return true, nil
		}
	}
	return false, nil
}
