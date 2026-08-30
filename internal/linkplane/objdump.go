package linkplane

import (
	"bufio"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// ObjdumpEdge is one direct call/jump extracted from objdump -d output.
// Indirect calls (call *…) are recorded separately by DWARF Phase 3;
// this parser skips them.
//
// CallerAddr / CalleeAddr are the operand addresses in the linked binary
// — the caller's function-label address and the callee's branch target.
// They are the primary key for edge disambiguation when the callee's
// linkage name collides across TUs (same-named statics); the name-based
// fallback is only correct when the name is unambiguous. CalleeAddr for
// PLT stubs points at the trampoline, not the callee proper — the
// stripped name is the disambiguator there instead.
type ObjdumpEdge struct {
	Caller     string // linkage name of the enclosing function
	CallerAddr uint64 // address of the function label in the linked binary
	Callee     string // raw linkage name from the disassembly (may include @plt)
	CalleeAddr uint64 // branch-target address; PLT stubs point at the trampoline
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
	// "0000000000001040 <main>:" — capture addr + name.
	odFuncRe = regexp.MustCompile(`^([0-9a-fA-F]+)\s+<([^>]+)>:$`)
	// "    104b:	call   11d0 <compute>" — capture callee addr + name.
	// "    108b:	call   1030 <printf@plt>"
	// (Direct — the operand is a hex address, no leading '*'.)
	odDirectCallRe = regexp.MustCompile(`^\s+[0-9a-fA-F]+:\s+(?:call|jmp)\s+([0-9a-fA-F]+)\s+<([^>]+)>`)
	// "    105c:	call   *0x2d5e(%rip)        # 3dc0 <g_ops>"
	// Indirect calls start the operand with '*'. We don't record these
	// as edges — DWARF Phase 3 already captured them with source
	// locations.
)

// RunObjdump runs `objdump -d --demangle --no-show-raw-insn` on path
// and returns every direct call/jump edge. Streams stdout line-by-line
// — a large binary's disassembly can exceed 100 MB and we drop >99% of
// bytes during scan, so buffering the full payload would spike RSS for
// no benefit.
func RunObjdump(path string) ([]ObjdumpEdge, error) {
	var edges []ObjdumpEdge
	err := runToolStreaming("objdump", []string{"-d", "--demangle", "--no-show-raw-insn", path}, func(r io.Reader) error {
		out, perr := parseObjdumpStream(r)
		edges = out
		return perr
	})
	if err != nil {
		return nil, err
	}
	return edges, nil
}

func parseObjdumpStream(r io.Reader) ([]ObjdumpEdge, error) {
	var edges []ObjdumpEdge
	var curFunc string
	var curFuncAddr uint64
	sc := bufio.NewScanner(r)
	// Long .text sections can produce lines wider than the default 64K.
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := sc.Text()
		if m := odFuncRe.FindStringSubmatch(line); m != nil {
			curFunc = m[2]
			curFuncAddr, _ = strconv.ParseUint(m[1], 16, 64)
			continue
		}
		if curFunc == "" {
			continue
		}
		if m := odDirectCallRe.FindStringSubmatch(line); m != nil {
			addr, _ := strconv.ParseUint(m[1], 16, 64)
			edges = append(edges, ObjdumpEdge{
				Caller:     curFunc,
				CallerAddr: curFuncAddr,
				Callee:     m[2],
				CalleeAddr: addr,
			})
		}
	}
	return edges, sc.Err()
}
