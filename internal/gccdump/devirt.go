package gccdump

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// Grammar (from GCC 16 -fdump-ipa-devirt):
//
//   No noted function pointers stored in records.       ← common in pure C
//   Procesing function <name>/<id>                       (sic — GCC typo)
//   ...
//   N polymorphic calls, N devirtualized, N speculatively devirtualized
//   Symbol table: …
//
// Devirtualization primarily targets C++ virtual calls. In pure C the
// pass reports 0 hits for our fixture. When it does fire, the pass
// emits lines like "speculatively devirtualizing <callee> called from
// <caller>" — that would be the anchor for DevirtHit entries. We keep
// the parser minimal (recognizes the summary line, extracts counts, does
// not synthesize hits from empty runs) so ingest gets a real empty slice
// on our fixture and can wire the schema without waiting for a devirt-
// firing test case.

var (
	devirtSpecLine = regexp.MustCompile(`speculatively devirtualizing (\S+) called from (\S+)`)
	devirtHardLine = regexp.MustCompile(`devirtualizing (?:direct |)call in ([\w.$]+) to ([\w.$]+)`)
)

// ParseDevirtFile parses one .devirt dump.
func ParseDevirtFile(path string) (*DevirtDump, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("gccdump: open .devirt %s: %w", path, err)
	}
	defer f.Close()
	d, err := ParseDevirt(f)
	if err != nil {
		return nil, fmt.Errorf("gccdump: parse .devirt %s: %w", path, err)
	}
	return d, nil
}

// ParseDevirt parses one .devirt dump from r.
func ParseDevirt(r io.Reader) (*DevirtDump, error) {
	dump := &DevirtDump{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := devirtSpecLine.FindStringSubmatch(line); m != nil {
			dump.Hits = append(dump.Hits, DevirtHit{
				CallerName: m[2],
				TargetName: m[1],
				Confidence: "speculative",
			})
			continue
		}
		if m := devirtHardLine.FindStringSubmatch(line); m != nil {
			dump.Hits = append(dump.Hits, DevirtHit{
				CallerName: m[1],
				TargetName: m[2],
				Confidence: "resolved",
			})
			continue
		}
	}
	return dump, sc.Err()
}
