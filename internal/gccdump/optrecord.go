package gccdump

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Grammar (from GCC 16 -fsave-optimization-record):
//
// The file is a gzipped JSON array of three elements:
//
//	[0] {"format":"1","generator":{...}}
//	[1] the pass tree — nested {id,type,name,optgroups,children}
//	[2] the records — each {kind,pass,message,location,function,...}
//
// A record's "pass" is an opaque pointer-shaped id into the tree, which
// is the only place the pass name and its optgroups live. We keep the
// records whose pass carries the "inline" optgroup — the same selector
// GCC's own -fopt-info-inline uses — so the vectorizer and unroller
// records in the same file are dropped without hardcoding their names.
//
// The two node references in a message are ordered by the message's
// own wording, not by position:
//
//	success: "Inlined <callee> into <caller> which now has time …"
//	success: "  Inlining <callee> into <caller> (always_inline)."
//	failure: "not inlinable: <caller> -> <callee>, function body not available"
//	failure: "will not early inline: <caller>-><callee>, call is cold and …"
//
// so we read the separator between the two nodes rather than assuming
// an order. A record whose separator matches neither form is dropped: a
// reversed inline edge would be worse than a missing one.

// SupportedOptRecordFormat is the value of element [0]'s "format" key
// this parser understands. The layout below — a 3-element array whose
// second element is the pass tree and third the records — is what that
// version guarantees. A bumped format is refused rather than parsed
// leniently: the failure mode of guessing is an empty inline plane with
// nothing saying it went empty, which is the exact blind spot this
// parser exists to close.
const SupportedOptRecordFormat = "1"

// optMeta is element [0].
type optMeta struct {
	Format string `json:"format"`
}

// optPass is one node of the pass tree in element [1].
type optPass struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Optgroups []string  `json:"optgroups"`
	Children  []optPass `json:"children"`
}

// optLocation is a source position on a record or a message node.
type optLocation struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

// optRecord is one entry of element [2]. Message items are either a
// bare string or an object carrying a symtab_node reference, so they
// stay raw until parseMessage splits them.
type optRecord struct {
	Kind     string            `json:"kind"`
	Pass     string            `json:"pass"`
	Message  []json.RawMessage `json:"message"`
	Location *optLocation      `json:"location"`
}

// ParseOptRecordFile parses one gzipped .opt-record.json.gz.
func ParseOptRecordFile(path string) (*OptRecordDump, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("gccdump: open opt-record %s: %w", path, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gccdump: gunzip opt-record %s: %w", path, err)
	}
	defer zr.Close()
	d, err := ParseOptRecord(zr)
	if err != nil {
		return nil, fmt.Errorf("gccdump: parse opt-record %s: %w", path, err)
	}
	return d, nil
}

// ParseOptRecord parses an optimization record from r, which must yield
// the decompressed JSON.
func ParseOptRecord(r io.Reader) (*OptRecordDump, error) {
	var top []json.RawMessage
	if err := json.NewDecoder(r).Decode(&top); err != nil {
		return nil, err
	}
	if len(top) < 3 {
		return nil, fmt.Errorf("expected 3 top-level elements, got %d", len(top))
	}

	var meta optMeta
	if err := json.Unmarshal(top[0], &meta); err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	if meta.Format != SupportedOptRecordFormat {
		return nil, fmt.Errorf(
			"unsupported optimization-record format %q (this build understands %q); "+
				"the record layout may have changed — update internal/gccdump/optrecord.go",
			meta.Format, SupportedOptRecordFormat)
	}

	var tree []optPass
	if err := json.Unmarshal(top[1], &tree); err != nil {
		return nil, fmt.Errorf("pass tree: %w", err)
	}
	inlinePasses := map[string]string{}
	collectInlinePasses(tree, inlinePasses)

	var records []optRecord
	if err := json.Unmarshal(top[2], &records); err != nil {
		return nil, fmt.Errorf("records: %w", err)
	}

	dump := &OptRecordDump{}
	seen := map[InlineRecord]bool{}
	for _, rec := range records {
		pass, ok := inlinePasses[rec.Pass]
		if !ok {
			continue
		}
		// "note" records restate a decision already logged as success
		// (expand_call_inline) or announce a candidate whose outcome
		// arrives as its own record ("Considering inline candidate").
		// Keeping them would double-count.
		if rec.Kind != "success" && rec.Kind != "failure" {
			continue
		}
		ir, ok := recordFromMessage(rec, pass)
		if !ok {
			continue
		}
		if seen[ir] {
			continue
		}
		seen[ir] = true
		dump.InlineRecords = append(dump.InlineRecords, ir)
	}
	return dump, nil
}

// collectInlinePasses flattens the pass tree into id → name for the
// passes tagged with the "inline" optgroup.
func collectInlinePasses(passes []optPass, out map[string]string) {
	for _, p := range passes {
		for _, g := range p.Optgroups {
			if g == "inline" {
				out[p.ID] = p.Name
				break
			}
		}
		collectInlinePasses(p.Children, out)
	}
}

// msgNode is a message item that references a cgraph node.
type msgNode struct {
	Node string `json:"symtab_node"`
}

// recordFromMessage turns one record into an InlineRecord, returning
// false when the message is not a two-node inline decision (unit-growth
// summaries and single-node candidate notes take that path).
func recordFromMessage(rec optRecord, pass string) (InlineRecord, bool) {
	var (
		nodes  []string
		seps   []string // text runs, one per gap between items
		cur    strings.Builder
		sawOne bool
	)
	for _, raw := range rec.Message {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			cur.WriteString(s)
			continue
		}
		var n msgNode
		if err := json.Unmarshal(raw, &n); err != nil || n.Node == "" {
			continue
		}
		seps = append(seps, cur.String())
		cur.Reset()
		nodes = append(nodes, n.Node)
		sawOne = true
	}
	if !sawOne || len(nodes) < 2 {
		return InlineRecord{}, false
	}
	trailing := cur.String()

	first, firstID, ok := splitNode(nodes[0])
	if !ok {
		return InlineRecord{}, false
	}
	second, secondID, ok := splitNode(nodes[1])
	if !ok {
		return InlineRecord{}, false
	}

	ir := InlineRecord{Pass: pass, Inlined: rec.Kind == "success"}
	switch sep := seps[1]; {
	case strings.Contains(sep, "->"):
		ir.CallerName, ir.CallerLocalID = first, firstID
		ir.CalleeName, ir.CalleeLocalID = second, secondID
	case strings.Contains(sep, " into ") || strings.Contains(sep, " to "):
		ir.CalleeName, ir.CalleeLocalID = first, firstID
		ir.CallerName, ir.CallerLocalID = second, secondID
	default:
		return InlineRecord{}, false
	}

	if !ir.Inlined {
		ir.Reason = cleanReason(trailing)
	}
	if rec.Location != nil {
		ir.File = rec.Location.File
		ir.Line = rec.Location.Line
		ir.Column = rec.Location.Column
	}
	return ir, true
}

// splitNode splits a "name/order" symtab_node reference. Clone names
// carry dots ("use_dispatch.constprop"), so we cut at the last slash.
func splitNode(s string) (name, id string, ok bool) {
	i := strings.LastIndex(s, "/")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// cleanReason trims the message tail down to GCC's explanation: the
// text after the callee node, minus the ", " that joins it and any
// trailing sentence punctuation. A failure record's tail can also carry
// an unrelated pass summary after a blank line, which is dropped.
func cleanReason(tail string) string {
	if i := strings.Index(tail, "\n"); i >= 0 {
		tail = tail[:i]
	}
	tail = strings.TrimSpace(tail)
	tail = strings.TrimPrefix(tail, ",")
	tail = strings.TrimSuffix(strings.TrimSpace(tail), ".")
	return strings.TrimSpace(tail)
}
