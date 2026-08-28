package gccdump

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Grammar (from GCC 16 post-IPA cgraph dumps):
//
//   Trivially needed symbols: main/5 …
//
//   Initial Symbol table:                        ← section marker
//   ...
//   Reclaimed Symbol table:
//   ...
//   Optimized Symbol table:                      ← richest post-IPA view
//   ...
//   Final Symbol table:                          ← often stripped
//
//   <name>/<id> (<linkage-name>)                 ← symbol header, col 0
//     Type: (variable|function) [definition analyzed]
//     Body removed by symtab_remove_unreachable_nodes
//     Visibility: <space-separated tokens>
//     Address is taken.
//     Function flags: [count:N (…)] <tokens>
//     Function <linkage_name>/<id> is inline copy in <name>/<id>
//     Clone of <name>/<id>
//     Aux: @0x…                                  ← ignored
//     Availability: <one word>
//     Varpool flags: <tokens>                    ← ignored
//     References: <name>/<id> (<kind>) …
//     Referring: <name>/<id> (<kind>) …
//     Called by: <name>/<id> [(inlined)] [(count,freq)] …
//     Calls: <name>/<id> [(inlined)] [(count,freq)] …
//        indirect simple callsite, … num speculative call targets: N   ← 7-space indent
//
// The parser reads each symbol block into a throwaway Function, then
// merges into the persistent per-id record with a "richer wins" rule
// so that early tables (which carry pre-IPA data like all external
// calls) and the Optimized table (which carries clones and inline tags)
// both contribute. The Final table's stripped rows never lose us data.

var (
	cgSymbolHeader = regexp.MustCompile(`^([A-Za-z_][\w.$]*)/(\d+)\s+\(([^)]*)\)\s*$`)
	cgTargetTok    = regexp.MustCompile(`([A-Za-z_][\w.$]*)/(\d+)`)
	cgIndirect     = regexp.MustCompile(`indirect .+ callsite,.*num speculative call targets:\s*(\d+)`)
	cgCloneOf      = regexp.MustCompile(`^Clone of ([A-Za-z_][\w.$]*)/(\d+)$`)
	cgCountTok     = regexp.MustCompile(`^count:\d+`)
)

// ParseCgraphFile parses one .cgraph dump.
func ParseCgraphFile(path string) (*Cgraph, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("gccdump: open .cgraph %s: %w", path, err)
	}
	defer f.Close()
	c, err := ParseCgraph(f)
	if err != nil {
		return nil, fmt.Errorf("gccdump: parse .cgraph %s: %w", path, err)
	}
	return c, nil
}

// ParseCgraph parses one .cgraph dump from r.
func ParseCgraph(r io.Reader) (*Cgraph, error) {
	cg := &Cgraph{Symbols: map[string]*Function{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	// Per-observation staging. flushed into cg.Symbols on end-of-block.
	var cur *Function

	flush := func() {
		if cur == nil {
			return
		}
		mergeSymbol(cg.Symbols, cur)
		cur = nil
	}

	for sc.Scan() {
		raw := sc.Text()
		trimmed := strings.TrimSpace(raw)

		if strings.HasPrefix(trimmed, "Trivially needed") {
			flush()
			for _, m := range cgTargetTok.FindAllStringSubmatch(trimmed, -1) {
				cg.TrivialNeeded = append(cg.TrivialNeeded, m[2])
			}
			continue
		}
		if strings.HasSuffix(trimmed, "Symbol table:") {
			flush()
			continue
		}
		if trimmed == "" ||
			strings.HasPrefix(trimmed, "Removing ") ||
			strings.HasPrefix(trimmed, "Reclaiming ") ||
			strings.HasPrefix(trimmed, "Clearing ") {
			flush()
			continue
		}

		// Column-0 header line starts a new symbol observation.
		if !strings.HasPrefix(raw, " ") {
			if m := cgSymbolHeader.FindStringSubmatch(raw); m != nil {
				flush()
				cur = &Function{
					LocalID:     m[2],
					Name:        m[1],
					LinkageName: m[3],
				}
				continue
			}
			// Unrecognized column-0 line — ends any in-flight block.
			flush()
			continue
		}

		if cur == nil {
			continue
		}

		switch {
		case strings.HasPrefix(trimmed, "Type:"):
			t := strings.TrimSpace(strings.TrimPrefix(trimmed, "Type:"))
			toks := strings.Fields(t)
			if len(toks) > 0 {
				cur.Kind = toks[0]
			}
			cur.Analyzed = strings.Contains(t, "definition analyzed")

		case strings.HasPrefix(trimmed, "Body removed"):
			cur.BodyRemoved = true

		case strings.HasPrefix(trimmed, "Visibility:"):
			v := strings.TrimSpace(strings.TrimPrefix(trimmed, "Visibility:"))
			cur.VisibilityFlags = strings.Fields(v)

		case trimmed == "Address is taken.":
			cur.AddressTaken = true

		case strings.HasPrefix(trimmed, "Function flags:"):
			v := strings.TrimSpace(strings.TrimPrefix(trimmed, "Function flags:"))
			for _, tok := range strings.Fields(v) {
				if cgCountTok.MatchString(tok) {
					continue
				}
				if strings.HasPrefix(tok, "(") || strings.HasSuffix(tok, ")") {
					continue
				}
				cur.FunctionFlags = append(cur.FunctionFlags, tok)
			}

		case cgCloneOf.MatchString(trimmed):
			m := cgCloneOf.FindStringSubmatch(trimmed)
			cur.CloneOfID = m[2]

		// "Function X/N is inline copy in Y/M" is metadata — the outgoing
		// (inlined) tag on Y's Calls: line already carries the same fact.
		// We deliberately do NOT synthesize an edge here (an earlier
		// version did, which put an inverse edge in the graph).

		case strings.HasPrefix(trimmed, "References:"):
			cur.Refs = append(cur.Refs, parseRefList(strings.TrimPrefix(trimmed, "References:"))...)

		case strings.HasPrefix(trimmed, "Referring:"):
			for _, r := range parseRefList(strings.TrimPrefix(trimmed, "Referring:")) {
				if r.Kind == "addr" {
					cur.AddressTaken = true
				}
			}

		case strings.HasPrefix(trimmed, "Calls:"):
			cur.Called = append(cur.Called, parseCallList(strings.TrimPrefix(trimmed, "Calls:"))...)

		case strings.HasPrefix(trimmed, "Called by:"):
			// Outgoing side is canonical; skip.

		case cgIndirect.MatchString(trimmed):
			m := cgIndirect.FindStringSubmatch(trimmed)
			n, _ := strconv.Atoi(m[1])
			cur.IndirectSites = append(cur.IndirectSites, IndirectSite{SpeculativeCount: n})

		default:
			// Aux, Availability, Varpool flags, GIMPLE pretty-print, and
			// diagnostic lines like "updating call of X -> Y" are ignored.
		}
	}
	flush()
	return cg, sc.Err()
}

// mergeSymbol combines a newly-parsed observation into the persistent
// record. Rule: for each field, keep whichever side has more information.
// The .cgraph writes symbols multiple times across pass boundaries; the
// Initial table has pre-IPA structure, the Optimized table has clones and
// (inlined) tags, and the Final table often has stripped rows we don't
// want to inherit from.
func mergeSymbol(dst map[string]*Function, obs *Function) {
	prev, ok := dst[obs.LocalID]
	if !ok {
		dst[obs.LocalID] = obs
		return
	}
	// Name / LinkageName: keep the more specific one (a clone header may
	// change the name in later tables — that's when we want the new value).
	if obs.Name != "" {
		prev.Name = obs.Name
	}
	if obs.LinkageName != "" {
		prev.LinkageName = obs.LinkageName
	}
	if obs.Kind != "" {
		prev.Kind = obs.Kind
	}
	// Booleans: OR — a flag observed once stays.
	prev.Analyzed = prev.Analyzed || obs.Analyzed
	prev.BodyRemoved = prev.BodyRemoved || obs.BodyRemoved
	prev.AddressTaken = prev.AddressTaken || obs.AddressTaken
	// CloneOfID: only overwrite if the new one is set.
	if obs.CloneOfID != "" {
		prev.CloneOfID = obs.CloneOfID
	}
	// Flag arrays: >= (later wins on ties) so the Optimized/Final view
	// with additions like 'asm_written' or 'externally_visible' displaces
	// the Initial view even at equal length.
	if len(obs.VisibilityFlags) >= len(prev.VisibilityFlags) && len(obs.VisibilityFlags) > 0 {
		prev.VisibilityFlags = obs.VisibilityFlags
	}
	if len(obs.FunctionFlags) >= len(prev.FunctionFlags) && len(obs.FunctionFlags) > 0 {
		prev.FunctionFlags = obs.FunctionFlags
	}
	// Refs / Called / IndirectSites: >= on non-empty, never replace a
	// non-empty list with an empty one — Final tables often strip these.
	// The >= is load-bearing: Initial and Optimized may both have 4-edge
	// call lists, but Optimized references clone IDs post-IPA. We want
	// the later one.
	if len(obs.Refs) >= len(prev.Refs) && len(obs.Refs) > 0 {
		prev.Refs = obs.Refs
	}
	if len(obs.Called) >= len(prev.Called) && len(obs.Called) > 0 {
		prev.Called = obs.Called
	}
	if len(obs.IndirectSites) >= len(prev.IndirectSites) && len(obs.IndirectSites) > 0 {
		prev.IndirectSites = obs.IndirectSites
	}
}

// parseRefList tokenizes lines like "g_ops/9 (read) g_ops/9 (read)".
func parseRefList(s string) []Ref {
	idxs := cgTargetTok.FindAllStringSubmatchIndex(s, -1)
	if idxs == nil {
		return nil
	}
	var out []Ref
	for i, idx := range idxs {
		id := s[idx[4]:idx[5]]
		end := len(s)
		if i+1 < len(idxs) {
			end = idxs[i+1][0]
		}
		tail := s[idx[1]:end]
		kind := ""
		if lp := strings.Index(tail, "("); lp >= 0 {
			if rp := strings.Index(tail[lp:], ")"); rp > 0 {
				kind = tail[lp+1 : lp+rp]
			}
		}
		out = append(out, Ref{TargetLocalID: id, Kind: kind})
	}
	return out
}

// parseCallList tokenizes lines like:
//   printf/8 (1073741824 (estimated locally),1.00 per call)
//   use_dispatch.constprop.0/10 (inlined) (…)
//
// Returns id + whether "(inlined)" appears in the trailing paren blob
// before the next id.
func parseCallList(s string) []Call {
	idxs := cgTargetTok.FindAllStringSubmatchIndex(s, -1)
	if idxs == nil {
		return nil
	}
	var out []Call
	for i, idx := range idxs {
		id := s[idx[4]:idx[5]]
		end := len(s)
		if i+1 < len(idxs) {
			end = idxs[i+1][0]
		}
		tail := s[idx[1]:end]
		out = append(out, Call{
			TargetLocalID: id,
			Inlined:       strings.Contains(tail, "(inlined)"),
		})
	}
	return out
}
