package linkplane

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// MapFile is the parsed subset of a GNU ld map file herbarium consumes.
// The full format has many fields we don't need; we focus on what
// answers "which object supplied this symbol" and "was it pulled from
// an archive".
type MapFile struct {
	// SymbolOrigin maps a symbol name to the object file that supplied
	// its definition. Extracted from section-table entries like:
	//   .text 0x11a0 0x6 app1/app1.p/strong_override.c.o
	//                    0x11a0                hook
	SymbolOrigin map[string]string

	// ArchivePulls lists objects the linker pulled from archives to
	// satisfy references, with the symbol that triggered each pull.
	// Extracted from the "Archive member included to satisfy..." block.
	ArchivePulls []ArchivePull
}

// ArchivePull describes one entry in the map's load list.
type ArchivePull struct {
	Object   string // pulled object path (e.g., "lib/libshared.a.p/weak_impl.c.o")
	Referrer string // the object that referenced the trigger symbol
	Symbol   string // the symbol name that triggered the pull
}

// ArchiveFor infers the .a a pulled object came from. Meson's static-
// library convention: an object at "<dir>/<name>.a.p/<obj>.o" was
// bundled into "<dir>/<name>.a". Returns "" when path doesn't match
// (e.g., objects compiled directly into an executable).
func ArchiveFor(objectPath string) string {
	// Look for "/<name>.a.p/" as a directory segment.
	idx := strings.Index(objectPath, ".a.p/")
	if idx < 0 {
		return ""
	}
	// Everything up to and including ".a" is the archive path.
	return objectPath[:idx+2]
}

var (
	// Section contribution start: " .text  0x0000...  0xN  path/to/.o"
	//                              " .text.startup  0x...  0x... path/to/.o"
	// Section names may contain dots (.text.startup, .text.unlikely,
	// and — under -ffunction-sections — .text.<symbol>).
	// Object path may end in .o (regular object) or .o) (archive member notation).
	mfContribRe = regexp.MustCompile(`^\s+\.[\w.]+\s+0x[0-9a-fA-F]+\s+0x[0-9a-fA-F]+\s+(\S+\.o)$`)
	// Section header line with no address on it. Under long section
	// names (-ffunction-sections often emits ".text.<symbol>" long
	// enough to overflow ld's alignment), ld splits the header from
	// the address/size/object triple onto two lines:
	//   .text.add_ints
	//                0x11c0        0x4 lib/…/shared_utils.c.o
	mfContribHeadRe = regexp.MustCompile(`^\s+\.[\w.]+\s*$`)
	// Continuation line for the split form above: address, size, object.
	mfContribTailRe = regexp.MustCompile(`^\s+0x[0-9a-fA-F]+\s+0x[0-9a-fA-F]+\s+(\S+\.o)$`)
	// Symbol assignment: "                0x11a0                hook"
	// Deep indent, address, symbol name (no path — a bare identifier).
	mfSymRe = regexp.MustCompile(`^\s+0x[0-9a-fA-F]+\s+([A-Za-z_][\w.$@]*)$`)
	// Load list entries — two-line pattern; state machine handles both.
	mfLoadObject = regexp.MustCompile(`^(\S+\.o)$`)
	mfLoadRef    = regexp.MustCompile(`^\s+(\S+\.o)\s+\(([^)]+)\)$`)
)

// ReadMap parses a map file at path.
func ReadMap(path string) (*MapFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("linkplane: open map %s: %w", path, err)
	}
	defer f.Close()

	mf := &MapFile{
		SymbolOrigin: map[string]string{},
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	// State machine phases:
	//   0 — pre-load-list header
	//   1 — inside the archive-pull load list
	//   2 — past load list; scanning section contributions
	phase := 0
	var pendingLoadObject string
	var curContribObject string
	var pendingSplitHeader bool

	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)

		switch phase {
		case 0:
			if strings.HasPrefix(trimmed, "Archive member included") {
				phase = 1
			}
		case 1:
			// End of load list: blank line then a new section header.
			if trimmed == "" {
				// Blank line inside the list is OK; skip.
				continue
			}
			if strings.HasPrefix(trimmed, "Merging program properties") ||
				strings.HasPrefix(trimmed, "Discarded input sections") ||
				strings.HasPrefix(trimmed, "As-needed library") ||
				strings.HasPrefix(trimmed, "Memory Configuration") {
				phase = 2
				continue
			}
			// Two-line pattern: object path, then indented "(referrer (symbol))".
			if m := mfLoadObject.FindStringSubmatch(trimmed); m != nil {
				pendingLoadObject = m[1]
				continue
			}
			if m := mfLoadRef.FindStringSubmatch(line); m != nil && pendingLoadObject != "" {
				mf.ArchivePulls = append(mf.ArchivePulls, ArchivePull{
					Object:   pendingLoadObject,
					Referrer: m[1],
					Symbol:   m[2],
				})
				pendingLoadObject = ""
			}
		case 2:
			// Split-header form (line 1 = section name only, next line
			// carries the address/size/object triple). Peek by leaving
			// pendingSplitHeader set; the next iteration resolves it.
			if pendingSplitHeader {
				pendingSplitHeader = false
				if m := mfContribTailRe.FindStringSubmatch(line); m != nil {
					curContribObject = m[1]
					continue
				}
				// If the tail didn't match, fall through and treat this
				// line normally (rare — probably a header for an empty
				// section like ".text.never_called" that was gc-stripped).
			}
			// Single-line contribution: begins with " .text …" or " .data …".
			if m := mfContribRe.FindStringSubmatch(line); m != nil {
				curContribObject = m[1]
				continue
			}
			if mfContribHeadRe.MatchString(line) {
				pendingSplitHeader = true
				continue
			}
			// Symbol under current contribution.
			if curContribObject != "" {
				if m := mfSymRe.FindStringSubmatch(line); m != nil {
					name := m[1]
					if _, seen := mf.SymbolOrigin[name]; !seen {
						mf.SymbolOrigin[name] = curContribObject
					}
					continue
				}
			}
			// Any line that isn't an indented symbol assignment closes
			// the current contribution scope.
			if !strings.HasPrefix(line, "  ") {
				curContribObject = ""
			}
		}
	}
	return mf, sc.Err()
}
