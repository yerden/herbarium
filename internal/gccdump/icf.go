package gccdump

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
)

// Grammar (from GCC 16 -fdump-ipa-icf):
//
//   Dump after hash based groups
//   Congruence classes: N with total: M items (in a non-singular class: K)
//   ...
//   Introduced new external node (WINNER.localalias/id).
//   ...
//   Analyzing function: LOSER/id
//   ...
//     scanning: retval.2_4 = WINNER.localalias (x_2(D)); [tail call]
//   ...
//   int LOSER (int x)
//   {
//     ...
//     retval.2_4 = WINNER.localalias (x_2(D)); [tail call]
//     ...
//   }
//
// A fold produces two independent signals: (1) `Introduced new external
// node (WINNER.localalias/id)` tells us the winning symbol; (2) every
// loser's rewritten body invokes `WINNER.localalias(...)`. We correlate
// the invocation with the most recent `Analyzing function: LOSER/id` to
// pin down which function was folded. GCC's earlier per-class member
// listing was never emitted in this dump kind — the alias + rewritten
// bodies are the load-bearing artifacts.

var (
	icfIntroducedRe = regexp.MustCompile(`Introduced new external node \(([A-Za-z_][\w.$]*)\.localalias/\d+\)`)
	icfAnalyzingRe  = regexp.MustCompile(`^Analyzing function:\s+([A-Za-z_][\w.$]*)/\d+`)
	icfAliasCallRe  = regexp.MustCompile(`\b([A-Za-z_][\w.$]*)\.localalias\s*\(`)
)

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

	winners := map[string]bool{}
	// winner → set of losers
	losersOf := map[string]map[string]bool{}
	var currentFn string

	for sc.Scan() {
		line := sc.Text()
		if m := icfIntroducedRe.FindStringSubmatch(line); m != nil {
			winners[m[1]] = true
			continue
		}
		if m := icfAnalyzingRe.FindStringSubmatch(line); m != nil {
			currentFn = m[1]
			continue
		}
		// A `.localalias` call inside a function body attributes that
		// function to the winner's group. Self-calls (winner referring to
		// its own alias) are impossible in IPA-ICF's wrapper scheme but
		// guarded anyway.
		for _, m := range icfAliasCallRe.FindAllStringSubmatch(line, -1) {
			winner := m[1]
			if currentFn == "" || currentFn == winner {
				continue
			}
			if losersOf[winner] == nil {
				losersOf[winner] = map[string]bool{}
			}
			losersOf[winner][currentFn] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	// Emit only groups where we have both a confirmed winner and at
	// least one loser rewritten against its alias — partial signals
	// are dropped so downstream code never records a bare winner.
	for w := range winners {
		losers := losersOf[w]
		if len(losers) == 0 {
			continue
		}
		names := make([]string, 0, len(losers))
		for l := range losers {
			names = append(names, l)
		}
		sort.Strings(names)
		dump.Groups = append(dump.Groups, ICFGroup{
			WinnerName: w,
			LoserNames: names,
		})
	}
	sort.Slice(dump.Groups, func(i, j int) bool {
		return dump.Groups[i].WinnerName < dump.Groups[j].WinnerName
	})
	return dump, nil
}
