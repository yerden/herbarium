package linkplane

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// stdoutHeadLimit caps how much of a failed tool's stdout we splice
// into the error message. 2 KiB is enough to name the offending symbol
// or offset without dumping the whole disassembly.
const stdoutHeadLimit = 2 * 1024

// logWriter is the destination for the per-invocation progress lines
// runTool prints. Defaults to os.Stderr; tests swap it via SetLogWriter
// to keep test output clean. Held atomically because collect may
// eventually fan out subprocess calls across goroutines.
var logWriter atomic.Pointer[io.Writer]

func init() {
	var w io.Writer = os.Stderr
	logWriter.Store(&w)
}

// SetLogWriter redirects the per-tool progress log to w. Pass nil to
// silence it entirely (useful for tests that assert error text and
// don't want spurious stderr noise).
func SetLogWriter(w io.Writer) {
	if w == nil {
		var discard io.Writer = io.Discard
		logWriter.Store(&discard)
		return
	}
	logWriter.Store(&w)
}

func logf(format string, args ...any) {
	wp := logWriter.Load()
	if wp == nil {
		return
	}
	fmt.Fprintf(*wp, format, args...)
}

// runTool runs an external binutils inspector, returning its stdout
// on success. Prints "$ name args" before the invocation and " (elapsed,
// N stdout bytes)" after — long-running collects otherwise look hung
// while objdump chews on a big binary. On failure the returned error
// includes exit status, stderr, and a bounded stdout head so herbarium's
// caller can diagnose what the tool actually complained about — the
// default *exec.ExitError only surfaces the exit code, which is useless
// in isolation.
func runTool(name string, args ...string) ([]byte, error) {
	logf("$ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	// Capturing stderr explicitly means we own the buffer; the default
	// Output() path only surfaces stderr on ExitError, not on other
	// error kinds (e.g., binary not on PATH).
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	stdout, err := cmd.Output()
	elapsed := time.Since(start)
	if err == nil {
		logf("  (%s, %s)\n", elapsed.Round(time.Millisecond), byteCount(len(stdout)))
		return stdout, nil
	}
	logf("  (%s, FAILED: %v)\n", elapsed.Round(time.Millisecond), err)
	return nil, wrapToolErr(name, args, stdout, stderr.Bytes(), err)
}

// byteCount formats n as a compact human-readable size (e.g., "4.2 MB").
func byteCount(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := int64(n) / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func wrapToolErr(name string, args []string, stdout, stderr []byte, err error) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %v", name, strings.Join(args, " "), err)
	// exec.ExitError may also carry its own Stderr snippet when Output
	// was used with cmd.Stderr==nil — try both sources so we don't miss
	// diagnostics no matter which code path landed us here.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 && len(stderr) == 0 {
		stderr = exitErr.Stderr
	}
	if s := strings.TrimRight(string(stderr), "\n"); s != "" {
		b.WriteString("\n--- stderr ---\n")
		b.WriteString(s)
	}
	if len(stdout) > 0 {
		head := stdout
		if len(head) > stdoutHeadLimit {
			head = head[:stdoutHeadLimit]
		}
		b.WriteString("\n--- stdout (first ")
		fmt.Fprintf(&b, "%d bytes)", len(head))
		if len(head) < len(stdout) {
			fmt.Fprintf(&b, " of %d", len(stdout))
		}
		b.WriteString(" ---\n")
		b.Write(head)
	}
	return errors.New(b.String())
}
