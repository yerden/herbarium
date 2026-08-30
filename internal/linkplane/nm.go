// Package linkplane invokes read-only binutils inspectors (nm, objdump,
// readelf) against already-built objects and linked artifacts, plus
// parses GNU ld map files, to derive the link-plane facts herbarium's
// schema requires. Every invocation is read-only; nothing modifies or
// rebuilds any file in the builddir.
package linkplane

import (
	"bufio"
	"strconv"
	"strings"
)

// NMSymbol is one line from `nm --defined-only --format=posix` or `nm -u`.
type NMSymbol struct {
	Name    string
	Kind    string // 'T', 't', 'W', 'w', 'V', 'v', 'D', 'd', 'B', 'b', 'R', 'r', 'C', 'U', ...
	Address string // hex string; empty for undefined
	Size    int    // bytes; 0 when nm didn't report
}

// Strong reports whether the symbol has strong external linkage per nm's
// POSIX type codes (T for text, D for data, B for bss, R for rodata).
func (s NMSymbol) Strong() bool {
	switch s.Kind {
	case "T", "D", "B", "R":
		return true
	}
	return false
}

// Weak reports whether the symbol has weak external linkage.
func (s NMSymbol) Weak() bool {
	switch s.Kind {
	case "W", "V":
		return true
	}
	return false
}

// Local reports whether the symbol is file-scoped (static in C). Local
// symbols don't participate in link resolution the same way — they
// belong to exactly the object that defined them.
func (s NMSymbol) Local() bool {
	if len(s.Kind) != 1 {
		return false
	}
	c := s.Kind[0]
	return c >= 'a' && c <= 'z' && c != 'w' && c != 'v'
}

// LinkageKind maps to the schema's link_resolutions.linkage_kind enum.
func (s NMSymbol) LinkageKind() string {
	switch {
	case s.Weak():
		return "weak"
	case s.Kind == "C":
		return "common"
	case s.Kind == "u":
		return "unique_global"
	case s.Strong():
		return "strong"
	}
	return ""
}

// IsFunction reports whether the symbol lives in a text section — i.e.,
// is (probably) a function. Used to filter link_resolutions writes to
// symbols herbarium's source view actually tracks.
func (s NMSymbol) IsFunction() bool {
	return s.Kind == "T" || s.Kind == "t" || s.Kind == "W" || s.Kind == "w"
}

// RunNMDefined runs `nm --defined-only --format=posix` on path and
// returns the parsed rows. Undefined symbols are excluded — call
// RunNMUndefined separately when needed.
func RunNMDefined(path string) ([]NMSymbol, error) {
	stdout, err := runTool("nm", "--defined-only", "--format=posix", path)
	if err != nil {
		return nil, err
	}
	return parseNMPosix(string(stdout))
}

// RunNMUndefined runs `nm -u --format=posix` for undefined-symbol
// discovery — used by dependency mapping (Phase 4's list_undefined_symbols).
func RunNMUndefined(path string) ([]NMSymbol, error) {
	stdout, err := runTool("nm", "-u", "--format=posix", path)
	if err != nil {
		return nil, err
	}
	return parseNMPosix(string(stdout))
}

// ObjectDef pairs a .o path with the nm kind code of one defined symbol
// observed in it. ScanObjectDefs produces these so ingest.Link can
// enumerate every candidate object that defines a given symbol name —
// used for winning_object attribution when no linker map is available
// and for losing_objects enumeration in every case.
type ObjectDef struct {
	Object string
	Kind   string
}

// ScanObjectDefs runs `nm --defined-only` on every path in objects and
// returns a symbol-name → observation-list index. Each observation
// preserves the caller's object path verbatim (caller decides absolute
// vs builddir-relative) and the raw nm kind code so callers can apply
// their own strong/weak/local classification. A failure on any single
// path aborts the whole scan.
func ScanObjectDefs(objects []string) (map[string][]ObjectDef, error) {
	out := map[string][]ObjectDef{}
	for _, obj := range objects {
		syms, err := RunNMDefined(obj)
		if err != nil {
			return nil, err
		}
		for _, s := range syms {
			if s.Kind == "" || s.Name == "" {
				continue
			}
			out[s.Name] = append(out[s.Name], ObjectDef{Object: obj, Kind: s.Kind})
		}
	}
	return out, nil
}

// parseNMPosix parses lines like:
//
//	compute T 11d0 29                     ← defined, 3-4 fields
//	__bss_start B 4018                    ← defined without size
//	printf@GLIBC_2.2.5 U                  ← undefined, 2 fields (name kind)
func parseNMPosix(out string) ([]NMSymbol, error) {
	var syms []NMSymbol
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var sym NMSymbol
		sym.Name = fields[0]
		sym.Kind = fields[1]
		if len(fields) >= 3 {
			sym.Address = fields[2]
		}
		if len(fields) >= 4 {
			if n, err := strconv.ParseInt(fields[3], 16, 64); err == nil {
				sym.Size = int(n)
			}
		}
		syms = append(syms, sym)
	}
	return syms, sc.Err()
}
