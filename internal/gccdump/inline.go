package gccdump

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// Grammar (from GCC 16 -fdump-ipa-inline):
//
//   IPA function summary for <name>/<id> [inlinable|not inlinable]
//     ...
//     calls:
//       <name>/<id> inlined                                ← inline decision
//         freq:1.00
//       <name>/<id> function body not available
//         freq:1.00 loop depth:0 size:3 time:12
//       <name>/<id> function not considered for inlining
//         ...
//
// Two waves of summaries appear per TU: a first pass with the "size/time"
// estimates and no decisions, then a second pass after the ipa-inline
// decision block that carries the actual `inlined` markers. The .cgraph
// dump's `(inlined)` tags on the Called: line record the same decisions
// so this parser is confirmatory rather than authoritative — it exists
// so ingest can cross-check and so future MCP tools can surface size/time
// numbers if needed.

var (
	inlSummaryHead = regexp.MustCompile(`^IPA function summary for ([A-Za-z_][\w.$]*)/(\d+)\s+(not inlinable|inlinable)`)
	// A call entry inside `calls:` — indent varies across versions.
	inlCallEntry = regexp.MustCompile(`^\s+([A-Za-z_][\w.$]*)/(\d+)\s+(inlined|function body not available|function not considered for inlining)`)
)

// ParseInlineFile parses one .inline dump.
func ParseInlineFile(path string) (*InlineDump, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("gccdump: open .inline %s: %w", path, err)
	}
	defer f.Close()
	d, err := ParseInline(f)
	if err != nil {
		return nil, fmt.Errorf("gccdump: parse .inline %s: %w", path, err)
	}
	return d, nil
}

// ParseInline parses one .inline dump from r.
func ParseInline(r io.Reader) (*InlineDump, error) {
	dump := &InlineDump{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	// The dump's later summaries overwrite earlier observations for the
	// same caller (the second-wave summary contains the decision).
	byID := map[string]*InlineSummary{}

	var cur *InlineSummary
	var inCalls bool
	for sc.Scan() {
		line := sc.Text()

		if m := inlSummaryHead.FindStringSubmatch(line); m != nil {
			// Flush prior summary observation.
			if cur != nil {
				byID[cur.FunctionLocalID] = cur
			}
			cur = &InlineSummary{
				FunctionLocalID: m[2],
				Name:            m[1],
				Inlinable:       m[3] == "inlinable",
			}
			inCalls = false
			continue
		}

		if strings.TrimSpace(line) == "calls:" {
			inCalls = true
			continue
		}

		// End of a summary block: a blank line or an un-indented line
		// that isn't a call entry.
		if cur != nil && inCalls {
			if m := inlCallEntry.FindStringSubmatch(line); m != nil {
				if m[3] == "inlined" {
					cur.InlinedInto = append(cur.InlinedInto, m[2])
				}
				continue
			}
			if strings.TrimSpace(line) == "" {
				inCalls = false
			}
		}

		// A non-summary column-0 line ends the current block.
		if cur != nil && line != "" && !strings.HasPrefix(line, " ") {
			byID[cur.FunctionLocalID] = cur
			cur = nil
			inCalls = false
		}
	}
	if cur != nil {
		byID[cur.FunctionLocalID] = cur
	}

	for _, s := range byID {
		dump.Summaries = append(dump.Summaries, *s)
	}
	return dump, sc.Err()
}
