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

// Grammar (from GCC's callgraph-info output, verified on GCC 16):
//
//   graph: { title: "<compile-cwd-relative source path>"
//   node: { title: "<name>" label: "<multi-line>" [shape : ellipse] }
//   edge: { sourcename: "<X>" targetname: "<Y>" label: "<path:line:col>" }
//   }
//
// Labels embed literal `\n` (two chars) between fields:
//   name \n decl-path:line:col \n <stack> bytes (static) \n <n> dynamic objects
//
// Nodes with `shape : ellipse` are external references (the compile TU
// only saw a declaration). Nodes without shape are defined functions.
// A synthetic node named "__indirect_call" represents every indirect
// call site in the source.

var (
	ciFieldRe = regexp.MustCompile(`(\w+)\s*:\s*"((?:[^"\\]|\\.)*)"`)
	ciFileLoc = regexp.MustCompile(`^(.*?):(\d+):(\d+)$`)
	ciStackRe = regexp.MustCompile(`^(\d+)\s+bytes\s+\((\w+)\)$`)
	ciDynRe   = regexp.MustCompile(`^(\d+)\s+dynamic\s+objects?$`)
)

// ParseCIFile parses one .ci file. Errors carry the path so ingest can
// surface them without re-wrapping.
func ParseCIFile(path string) (*CI, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("gccdump: open .ci %s: %w", path, err)
	}
	defer f.Close()
	ci, err := ParseCI(f)
	if err != nil {
		return nil, fmt.Errorf("gccdump: parse .ci %s: %w", path, err)
	}
	return ci, nil
}

// ParseCI parses one .ci dump from an io.Reader.
func ParseCI(r io.Reader) (*CI, error) {
	ci := &CI{Nodes: map[string]CINode{}}
	sc := bufio.NewScanner(r)
	// GCC labels are compact; the default 64 KiB buffer is fine but bump
	// once for safety on real projects with long paths.
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line == "}" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "graph:"):
			if fs := ciFields(line); fs["title"] != "" {
				ci.Title = fs["title"]
			}
		case strings.HasPrefix(line, "node:"):
			fs := ciFields(line)
			n := parseCINode(fs, line)
			ci.Nodes[n.Name] = n
		case strings.HasPrefix(line, "edge:"):
			fs := ciFields(line)
			e := parseCIEdge(fs)
			ci.Edges = append(ci.Edges, e)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ci, nil
}

// ciFields extracts every `key: "value"` pair from a line into a map.
// GCC never emits duplicate keys per line, so a plain map suffices.
func ciFields(line string) map[string]string {
	out := map[string]string{}
	for _, m := range ciFieldRe.FindAllStringSubmatch(line, -1) {
		// Unescape the two known escapes inside label strings.
		v := strings.ReplaceAll(m[2], `\n`, "\n")
		v = strings.ReplaceAll(v, `\"`, `"`)
		out[m[1]] = v
	}
	return out
}

func parseCINode(fs map[string]string, raw string) CINode {
	n := CINode{
		Name:                  fs["title"],
		IsExternal:            strings.Contains(raw, "shape") && strings.Contains(raw, "ellipse"),
		IsIndirectPlaceholder: fs["title"] == "__indirect_call",
	}
	// Label is multi-line; the second line (when present) is the decl
	// location. The third and fourth lines carry stack usage and dynamic
	// object count for locally-defined functions.
	for i, part := range strings.Split(fs["label"], "\n") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		switch i {
		case 0:
			// name — already captured via `title`; ignore.
		default:
			if m := ciFileLoc.FindStringSubmatch(part); m != nil && n.DeclFile == "" {
				n.DeclFile = m[1]
				n.DeclLine, _ = strconv.Atoi(m[2])
				n.DeclColumn, _ = strconv.Atoi(m[3])
				continue
			}
			if m := ciStackRe.FindStringSubmatch(part); m != nil {
				n.StackBytes, _ = strconv.Atoi(m[1])
				n.StackKind = m[2]
				continue
			}
			if m := ciDynRe.FindStringSubmatch(part); m != nil {
				n.DynamicObjs, _ = strconv.Atoi(m[1])
				continue
			}
		}
	}
	return n
}

func parseCIEdge(fs map[string]string) CIEdge {
	e := CIEdge{
		Source:   fs["sourcename"],
		Target:   fs["targetname"],
		Indirect: fs["targetname"] == "__indirect_call",
	}
	if m := ciFileLoc.FindStringSubmatch(fs["label"]); m != nil {
		e.SiteFile = m[1]
		e.SiteLine, _ = strconv.Atoi(m[2])
		e.SiteColumn, _ = strconv.Atoi(m[3])
	}
	return e
}
