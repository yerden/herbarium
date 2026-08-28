package linkplane

import (
	"bufio"
	"regexp"
	"strings"
)

// ObjdumpEdge is one direct call/jump extracted from objdump -d output.
// Indirect calls (call *…) are recorded separately by DWARF Phase 3;
// this parser skips them.
type ObjdumpEdge struct {
	Caller string // linkage name of the enclosing function
	Callee string // raw linkage name from the disassembly (may include @plt)
}

// CalleeStripped returns Callee with PLT and versioned-symbol decorations
// removed, so symbol lookup by name matches the source-view identity.
//
//   "printf@plt"           → "printf"
//   "printf@GLIBC_2.2.5"   → "printf"
func (e ObjdumpEdge) CalleeStripped() string {
	if i := strings.IndexByte(e.Callee, '@'); i >= 0 {
		return e.Callee[:i]
	}
	return e.Callee
}

var (
	// "0000000000001040 <main>:"
	odFuncRe = regexp.MustCompile(`^[0-9a-fA-F]+\s+<([^>]+)>:$`)
	// "    104b:	call   11d0 <compute>"
	// "    108b:	call   1030 <printf@plt>"
	// (Direct — the operand is a hex address, no leading '*'.)
	odDirectCallRe = regexp.MustCompile(`^\s+[0-9a-fA-F]+:\s+(?:call|jmp)\s+[0-9a-fA-F]+\s+<([^>]+)>`)
	// "    105c:	call   *0x2d5e(%rip)        # 3dc0 <g_ops>"
	// Indirect calls start the operand with '*'. We don't record these
	// as edges — DWARF Phase 3 already captured them with source
	// locations.
)

// RunObjdump runs `objdump -d --demangle --no-show-raw-insn` on path
// and returns every direct call/jump edge.
func RunObjdump(path string) ([]ObjdumpEdge, error) {
	stdout, err := runTool("objdump", "-d", "--demangle", "--no-show-raw-insn", path)
	if err != nil {
		return nil, err
	}
	return parseObjdump(string(stdout))
}

func parseObjdump(out string) ([]ObjdumpEdge, error) {
	var edges []ObjdumpEdge
	var curFunc string
	sc := bufio.NewScanner(strings.NewReader(out))
	// Long .text sections can produce lines wider than the default 64K.
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := sc.Text()
		if m := odFuncRe.FindStringSubmatch(line); m != nil {
			curFunc = m[1]
			continue
		}
		if curFunc == "" {
			continue
		}
		if m := odDirectCallRe.FindStringSubmatch(line); m != nil {
			edges = append(edges, ObjdumpEdge{
				Caller: curFunc,
				Callee: m[1],
			})
		}
	}
	return edges, sc.Err()
}
