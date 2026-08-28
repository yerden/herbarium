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

// Grammar (from GCC 16 -fdump-ipa-icf):
//
//   Dump after hash based groups
//   Congruence classes: N with total: M items (in a non-singular class: K)
//   ...
//   Item count: M
//   Congruent classes before: X, after: Y
//   ...
//
// Only "in a non-singular class: K" with K > 0 indicates actual folding.
// When ICF fires, GCC also emits a summary listing the members of each
// non-singular class — the exact format has drifted across GCC versions,
// so we currently report only that folding occurred (Groups is empty in
// that case) and leave the per-group breakdown to Phase 8's richer
// fixture, which forces ICF and pins down the exact format.

var icfNonSingular = regexp.MustCompile(`in a non-singular class:\s*(\d+)`)

// ParseICFFile parses one .icf dump.
func ParseICFFile(path string) (*ICFDump, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("gccdump: open .icf %s: %w", path, err)
	}
	defer f.Close()
	d, err := ParseICF(f)
	if err != nil {
		return nil, fmt.Errorf("gccdump: parse .icf %s: %w", path, err)
	}
	return d, nil
}

// ParseICF parses one .icf dump from r.
func ParseICF(r io.Reader) (*ICFDump, error) {
	dump := &ICFDump{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var maxFolded int
	for sc.Scan() {
		trimmed := strings.TrimSpace(sc.Text())
		if m := icfNonSingular.FindStringSubmatch(trimmed); m != nil {
			n, _ := strconv.Atoi(m[1])
			if n > maxFolded {
				maxFolded = n
			}
		}
	}
	// Placeholder: when we see folding, record N empty groups so
	// downstream code sees the signal. Phase 8's fixture will replace
	// this with member-name extraction.
	for i := 0; i < maxFolded; i++ {
		dump.Groups = append(dump.Groups, ICFGroup{})
	}
	return dump, sc.Err()
}
