package ingest

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/yerden/herbarium/internal/builddir"
	"github.com/yerden/herbarium/internal/gccdump"
	"github.com/yerden/herbarium/internal/usr"
)

// tuData is the parsed collection of dumps for one .o.
type tuData struct {
	object string
	ci     *gccdump.CI
	cgraph *gccdump.Cgraph
	inline *gccdump.InlineDump
	icf    *gccdump.ICFDump
	devirt *gccdump.DevirtDump
}

// symbolRec is the identity + aggregated attributes for one USR. defs
// carries per-observation location data — one entry per TU that
// contributed a definition. DWARF (Phase 3) will UPSERT signature and
// refine decl_file/decl_line on the def rows.
type symbolRec struct {
	usr          string
	name         string
	kind         string
	linkage      string
	signature    string
	addressTaken bool
	linkageNames map[string]struct{}
	defs         []defRec // per-def location entries
}

// defRec becomes one row in symbol_definitions. Dedup by (file, line,
// linkage_name) — two observations of the same static-inline header
// from different TUs contribute one canonical def row.
type defRec struct {
	file        string
	line        int
	declFile    string
	declLine    int
	isWeak      bool
	linkageName string
}

// perTUResolve is the local-id → USR map for one TU, produced during
// symbol aggregation and consumed during edge resolution.
type perTUResolve map[string]string

// Compiler ingests every .o's compiler-side dumps into the given DB.
// The caller has already opened db (via store.Open + store.Init) and is
// responsible for wrapping in a transaction if desired. This function
// runs its inserts inside a single transaction so a partial ingest
// leaves an empty index rather than a half-populated one.
func Compiler(db *sql.DB, bd *builddir.BuildDir, pr *PathResolver) (Summary, error) {
	tus := make([]*tuData, 0, len(bd.Objects))
	for _, o := range bd.Objects {
		tu, err := parseTU(o)
		if err != nil {
			return Summary{}, err
		}
		tus = append(tus, tu)
	}

	symbols := map[string]*symbolRec{}
	// resolves[objectPath][localID] = USR — populated as we synthesize.
	resolves := map[string]perTUResolve{}

	for _, tu := range tus {
		res := perTUResolve{}
		resolves[tu.object] = res
		if tu.cgraph == nil {
			continue
		}
		aggregateSymbols(tu, symbols, res, pr)
	}

	tx, err := db.Begin()
	if err != nil {
		return Summary{}, fmt.Errorf("ingest: begin: %w", err)
	}
	defer tx.Rollback()

	idByUSR, err := insertSymbols(tx, symbols)
	if err != nil {
		return Summary{}, err
	}

	// Shared across TUs so cross-TU collisions (e.g., two `main`s that
	// map to c:@F@main) don't produce duplicate edges.
	seen := &edgeAccum{
		callEdge:   map[edgeKey]bool{},
		inlineFlag: map[edgeKey]int{},
	}
	sum := Summary{Symbols: len(idByUSR)}
	for _, tu := range tus {
		if tu.cgraph == nil {
			continue
		}
		n, err := insertEdges(tx, tu, resolves[tu.object], idByUSR, seen)
		if err != nil {
			return Summary{}, err
		}
		sum.CallEdges += n
	}
	// Emit inline_decisions once per (caller, callee) pair globally.
	inlineStmt, err := tx.Prepare(
		`INSERT INTO inline_decisions (caller_id, callee_id, inlined) VALUES (?, ?, ?)`,
	)
	if err != nil {
		return Summary{}, fmt.Errorf("ingest: prepare inline_decisions: %w", err)
	}
	defer inlineStmt.Close()
	for k, inlined := range seen.inlineFlag {
		if _, err := inlineStmt.Exec(k.caller, k.callee, inlined); err != nil {
			return Summary{}, fmt.Errorf("ingest: inline_decisions insert: %w", err)
		}
		sum.InlineDecisions++
	}

	nICF, err := insertICFGroups(tx, tus, resolves, idByUSR, bd.Root)
	if err != nil {
		return Summary{}, err
	}
	sum.ICFGroups = nICF

	if err := tx.Commit(); err != nil {
		return Summary{}, fmt.Errorf("ingest: commit: %w", err)
	}
	sum.IDByUSR = idByUSR
	sum.ObjectToSource = buildObjectToSource(tus, bd.Root, pr)
	return sum, nil
}

// buildObjectToSource inverts the per-TU CI Title into a builddir-
// relative object path → project-relative source path map. Objects
// without a .ci or without a resolvable in-project source contribute no
// entry; the caller (ingest.Link) tolerates gaps by falling back to
// name-based lookup for symbols whose object it cannot resolve.
func buildObjectToSource(tus []*tuData, builddirRoot string, pr *PathResolver) map[string]string {
	out := make(map[string]string, len(tus))
	for _, tu := range tus {
		if tu.ci == nil || tu.ci.Title == "" {
			continue
		}
		src := pr.ToProjectRelative(tu.ci.Title)
		if !src.InProject {
			continue
		}
		rel, err := filepath.Rel(builddirRoot, tu.object)
		if err != nil {
			continue
		}
		out[filepath.ToSlash(rel)] = src.Rel
	}
	return out
}

// insertICFGroups walks each TU's parsed .icf dump and writes one
// icf_groups row per non-singular class plus one icf_group_members row
// per loser. Names are resolved to symbol IDs via the same per-TU
// local-id → USR map used for edges. Groups whose winner or losers
// cannot be resolved (missing from cgraph, or in a TU that had no
// .cgraph dump) are dropped rather than partially written.
func insertICFGroups(tx *sql.Tx, tus []*tuData, resolves map[string]perTUResolve, idByUSR map[string]int64, builddirRoot string) (int, error) {
	groupStmt, err := tx.Prepare(
		`INSERT INTO icf_groups (winner_symbol_id, object_file) VALUES (?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("ingest: prepare icf_groups: %w", err)
	}
	defer groupStmt.Close()
	memberStmt, err := tx.Prepare(
		`INSERT INTO icf_group_members (group_id, symbol_id) VALUES (?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("ingest: prepare icf_group_members: %w", err)
	}
	defer memberStmt.Close()

	written := 0
	for _, tu := range tus {
		if tu.icf == nil || len(tu.icf.Groups) == 0 || tu.cgraph == nil {
			continue
		}
		res := resolves[tu.object]
		if res == nil {
			continue
		}
		nameToID := buildNameToSymbolID(tu, res, idByUSR)
		for _, g := range tu.icf.Groups {
			winnerID, ok := nameToID[g.WinnerName]
			if !ok {
				continue
			}
			var loserIDs []int64
			for _, l := range g.LoserNames {
				if id, ok := nameToID[l]; ok && id != winnerID {
					loserIDs = append(loserIDs, id)
				}
			}
			if len(loserIDs) == 0 {
				continue
			}
			objRel := tu.object
			if rel, err := filepath.Rel(builddirRoot, tu.object); err == nil {
				objRel = rel
			}
			res, err := groupStmt.Exec(winnerID, objRel)
			if err != nil {
				return 0, fmt.Errorf("ingest: insert icf_group: %w", err)
			}
			gid, err := res.LastInsertId()
			if err != nil {
				return 0, err
			}
			for _, lid := range loserIDs {
				if _, err := memberStmt.Exec(gid, lid); err != nil {
					return 0, fmt.Errorf("ingest: insert icf_group_member: %w", err)
				}
			}
			written++
		}
	}
	return written, nil
}

// buildNameToSymbolID inverts the TU's local-id resolver into a
// name → symbol_id map for the symbols defined in that TU. Multiple
// local IDs may share a name (clones), so the first stable entry wins;
// this is only used for ICF resolution where names come straight from
// the .icf dump's alias references and are always the original names.
func buildNameToSymbolID(tu *tuData, res perTUResolve, idByUSR map[string]int64) map[string]int64 {
	out := map[string]int64{}
	if tu.cgraph == nil {
		return out
	}
	ids := make([]string, 0, len(tu.cgraph.Symbols))
	for id := range tu.cgraph.Symbols {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, localID := range ids {
		fn := tu.cgraph.Symbols[localID]
		if fn.Name == "" {
			continue
		}
		if _, seen := out[fn.Name]; seen {
			continue
		}
		usr, ok := res[localID]
		if !ok {
			continue
		}
		symID, ok := idByUSR[usr]
		if !ok {
			continue
		}
		out[fn.Name] = symID
	}
	return out
}

// Summary counts what Compiler wrote. Displayed by cmd/herbarium after
// a successful collect so the user sees whether the ingest matched
// their expectations. IndirectSites is populated by the DWARF pass.
type Summary struct {
	Symbols         int
	CallEdges       int
	InlineDecisions int
	ICFGroups       int
	// idByUSR is exposed so the DWARF pass can look up symbol row ids
	// by USR to UPSERT signatures and enrich decl locations.
	IDByUSR map[string]int64
	// ObjectToSource maps builddir-relative object path (as it appears in
	// map files, e.g. "lib/libshared.a.p/shared_utils.c.o") to the
	// project-relative source path (e.g. "lib/shared_utils.c"). Consumed
	// by ingest.Link to translate map-file SymbolOrigin entries into the
	// USR key `c:<src>@F@<name>` for internal-linkage disambiguation.
	ObjectToSource map[string]string
}

// parseTU reads the four required dump kinds. Missing dumps are
// tolerated at parse time — preflight has already gated required kinds.
func parseTU(art builddir.ObjectArtifacts) (*tuData, error) {
	tu := &tuData{object: art.Object}
	var err error
	if art.CI != "" {
		if tu.ci, err = gccdump.ParseCIFile(art.CI); err != nil {
			return nil, err
		}
	}
	if art.Cgraph != "" {
		if tu.cgraph, err = gccdump.ParseCgraphFile(art.Cgraph); err != nil {
			return nil, err
		}
	}
	if art.Inline != "" {
		if tu.inline, err = gccdump.ParseInlineFile(art.Inline); err != nil {
			return nil, err
		}
	}
	if art.ICF != "" {
		if tu.icf, err = gccdump.ParseICFFile(art.ICF); err != nil {
			return nil, err
		}
	}
	if art.Devirt != "" {
		if tu.devirt, err = gccdump.ParseDevirtFile(art.Devirt); err != nil {
			return nil, err
		}
	}
	return tu, nil
}

// aggregateSymbols walks one TU's cgraph, resolves each symbol to a
// USR, and folds it into the cross-TU map. Clones are attributed to
// their parent USR and their linkage name is recorded.
func aggregateSymbols(tu *tuData, symbols map[string]*symbolRec, res perTUResolve, pr *PathResolver) {
	// Sort by LocalID so behavior is deterministic across runs.
	ids := make([]string, 0, len(tu.cgraph.Symbols))
	for id := range tu.cgraph.Symbols {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// First: determine the anchor path per symbol. Static symbols need
	// the TU's source path in their USR. We derive it from .ci nodes
	// (each carries a decl file+line for locally-defined functions).
	// Fallback: use the TU's .ci Title (the source path being compiled).
	tuSourcePath := ""
	if tu.ci != nil {
		tuSourcePath = pr.ToProjectRelative(tu.ci.Title).Rel
	}

	// Two-phase: non-clones first, then clones (so clones can look up
	// their parent's USR from res after phase one).
	for _, id := range ids {
		fn := tu.cgraph.Symbols[id]
		if fn.CloneOfID != "" {
			continue
		}
		rec := recordFor(fn, tu, tuSourcePath, pr)
		if rec == nil {
			continue
		}
		res[id] = rec.usr
		mergeSymbol(symbols, rec)
	}
	for _, id := range ids {
		fn := tu.cgraph.Symbols[id]
		if fn.CloneOfID == "" {
			continue
		}
		parentUSR, ok := res[fn.CloneOfID]
		if !ok {
			// Parent wasn't recorded (e.g., body removed and no ci node)
			// — synthesize a bare rec so we don't lose the clone's edges.
			parentFn, hasParent := tu.cgraph.Symbols[fn.CloneOfID]
			if hasParent {
				parentRec := recordFor(parentFn, tu, tuSourcePath, pr)
				if parentRec != nil {
					parentUSR = parentRec.usr
					res[fn.CloneOfID] = parentUSR
					mergeSymbol(symbols, parentRec)
				}
			}
		}
		if parentUSR == "" {
			continue
		}
		res[id] = parentUSR
		// Record the clone's linkage name on the parent.
		if rec, ok := symbols[parentUSR]; ok {
			if fn.Name != "" {
				rec.linkageNames[fn.Name] = struct{}{}
			}
			if fn.LinkageName != "" {
				rec.linkageNames[fn.LinkageName] = struct{}{}
			}
			// A clone with (addr) referrers propagates address_taken up.
			if fn.AddressTaken {
				rec.addressTaken = true
			}
		}
	}
}

// recordFor builds a symbolRec for one non-clone cgraph entry. Returns
// nil when the entry has no usable identity (empty name).
func recordFor(fn *gccdump.Function, tu *tuData, tuSourcePath string, pr *PathResolver) *symbolRec {
	name := fn.Name
	if name == "" {
		name = fn.LinkageName
	}
	if name == "" {
		return nil
	}

	kind := fn.Kind
	if kind == "" {
		kind = "function"
	}

	// Static or external? "public" in VisibilityFlags → external.
	// Absence of "public" → internal (static/file-scope).
	isPublic := containsTok(fn.VisibilityFlags, "public") || containsTok(fn.VisibilityFlags, "externally_visible")
	isWeak := containsTok(fn.VisibilityFlags, "weak")

	linkage := "internal"
	switch {
	case isWeak:
		linkage = "weak"
	case isPublic:
		linkage = "external"
	}

	// Anchor path: static symbols get the TU's source path in their USR;
	// external symbols get an empty path.
	pathForUSR := ""
	if linkage == "internal" {
		pathForUSR = tuSourcePath
	}

	var symUSR string
	switch kind {
	case "variable":
		symUSR = usr.Variable(pathForUSR, name)
	default:
		symUSR = usr.Function(pathForUSR, name)
	}

	rec := &symbolRec{
		usr:          symUSR,
		name:         name,
		kind:         kind,
		linkage:      linkage,
		addressTaken: fn.AddressTaken,
		linkageNames: map[string]struct{}{fn.LinkageName: {}, fn.Name: {}},
	}

	// Only symbols with a body in THIS TU contribute a def entry.
	// Declarations-only (Analyzed=false or BodyRemoved) never do — the
	// def row will come from whichever TU actually defines the symbol.
	if fn.Analyzed && !fn.BodyRemoved {
		def := defRec{
			isWeak:      isWeak,
			linkageName: fn.LinkageName,
		}
		if tu.ci != nil {
			if n, ok := tu.ci.Nodes[name]; ok {
				def.file = pr.ToProjectRelative(n.DeclFile).Rel
				def.line = n.DeclLine
			}
		}
		if def.file == "" {
			def.file = tuSourcePath
		}
		rec.defs = []defRec{def}
	}

	return rec
}

// mergeSymbol folds newRec into symbols[newRec.usr]. Rule per field:
// - linkage: precedence 'external' > 'weak' > 'common' > 'internal' —
//   the linker will pick a strong def over a weak one if both are seen,
//   so we surface the strongest observed linkage.
// - address_taken: OR.
// - linkage_names: union.
// - defs: appended, deduplicated by (file, line, linkage_name).
func mergeSymbol(symbols map[string]*symbolRec, obs *symbolRec) {
	prev, ok := symbols[obs.usr]
	if !ok {
		symbols[obs.usr] = obs
		return
	}
	if prev.name == "" {
		prev.name = obs.name
	}
	if prev.kind == "" {
		prev.kind = obs.kind
	}
	prev.linkage = strongerLinkage(prev.linkage, obs.linkage)
	prev.addressTaken = prev.addressTaken || obs.addressTaken
	for n := range obs.linkageNames {
		if n != "" {
			prev.linkageNames[n] = struct{}{}
		}
	}
	for _, d := range obs.defs {
		if !hasDef(prev.defs, d) {
			prev.defs = append(prev.defs, d)
		}
	}
}

func hasDef(defs []defRec, d defRec) bool {
	for _, existing := range defs {
		if existing.file == d.file && existing.line == d.line && existing.linkageName == d.linkageName {
			return true
		}
	}
	return false
}

func strongerLinkage(a, b string) string {
	rank := map[string]int{"external": 3, "weak": 2, "common": 2, "internal": 1, "": 0}
	if rank[a] >= rank[b] {
		return a
	}
	return b
}

func containsTok(s []string, tok string) bool {
	for _, x := range s {
		if x == tok {
			return true
		}
	}
	return false
}

// insertSymbols writes every aggregated symbolRec into `symbols` and
// its per-def records into `symbol_definitions`, returning a
// USR → row-id map for edge resolution.
func insertSymbols(tx *sql.Tx, symbols map[string]*symbolRec) (map[string]int64, error) {
	symStmt, err := tx.Prepare(`INSERT INTO symbols
		(usr, name, kind, linkage, signature, address_taken, linkage_names)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("ingest: prepare symbols insert: %w", err)
	}
	defer symStmt.Close()

	defStmt, err := tx.Prepare(`INSERT INTO symbol_definitions
		(symbol_id, file, line, decl_file, decl_line, is_weak, linkage_name)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("ingest: prepare symbol_definitions insert: %w", err)
	}
	defer defStmt.Close()

	// symbols_fts is populated via a single 'rebuild' after DWARF pass
	// finishes filling in signatures. Manual per-row inserts here would
	// require tearing them down and re-inserting when signatures land,
	// which is more code with no benefit for a build-anchored ingest.

	// Insert deterministically so schemas dump identically across runs.
	usrs := make([]string, 0, len(symbols))
	for u := range symbols {
		usrs = append(usrs, u)
	}
	sort.Strings(usrs)

	idByUSR := map[string]int64{}
	for _, u := range usrs {
		s := symbols[u]
		addr := 0
		if s.addressTaken {
			addr = 1
		}
		names := sortedKeys(s.linkageNames)
		namesJSON, _ := json.Marshal(names)
		res, err := symStmt.Exec(s.usr, s.name, s.kind, s.linkage, s.signature, addr, string(namesJSON))
		if err != nil {
			return nil, fmt.Errorf("ingest: insert symbol %s: %w", s.usr, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		idByUSR[s.usr] = id

		// Sort defs deterministically before insert.
		sort.Slice(s.defs, func(i, j int) bool {
			if s.defs[i].file != s.defs[j].file {
				return s.defs[i].file < s.defs[j].file
			}
			return s.defs[i].line < s.defs[j].line
		})
		for _, d := range s.defs {
			weak := 0
			if d.isWeak {
				weak = 1
			}
			if _, err := defStmt.Exec(id, d.file, d.line, d.declFile, d.declLine, weak, d.linkageName); err != nil {
				return nil, fmt.Errorf("ingest: insert def for %s: %w", s.usr, err)
			}
		}
	}
	return idByUSR, nil
}

type edgeKey struct {
	caller, callee int64
}

// edgeAccum carries dedup state across TUs so cross-TU collisions
// (e.g., two `main`s aliased to c:@F@main) don't produce duplicates.
// Note: indirect_call_sites are populated by the DWARF pass, not here —
// DWARF has file/line/column that .cgraph lacks.
type edgeAccum struct {
	callEdge   map[edgeKey]bool
	inlineFlag map[edgeKey]int // caller/callee → OR of inlined flags
}

// insertEdges resolves per-TU local-id references against the symbol
// table and writes call_edges. inline_decisions are aggregated into
// seen.inlineFlag and emitted once at the end of the ingest run so we
// get exactly one row per (caller, callee) pair. Returns the number of
// call_edges rows this TU contributed.
//
// indirect_call_sites are NOT written here — the DWARF pass owns that
// table because it has file/line/column that .cgraph lacks.
func insertEdges(tx *sql.Tx, tu *tuData, res perTUResolve, idByUSR map[string]int64, seen *edgeAccum) (int, error) {
	edgeStmt, err := tx.Prepare(
		`INSERT INTO call_edges (caller_id, callee_id, source, target_id)
		 VALUES (?, ?, 'compiler_cgraph', NULL)`,
	)
	if err != nil {
		return 0, fmt.Errorf("ingest: prepare call_edges: %w", err)
	}
	defer edgeStmt.Close()

	ids := make([]string, 0, len(tu.cgraph.Symbols))
	for id := range tu.cgraph.Symbols {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var n int
	for _, callerLocal := range ids {
		callerUSR, ok := res[callerLocal]
		if !ok {
			continue
		}
		callerID, ok := idByUSR[callerUSR]
		if !ok {
			continue
		}
		fn := tu.cgraph.Symbols[callerLocal]

		for _, call := range fn.Called {
			calleeUSR, ok := res[call.TargetLocalID]
			if !ok {
				continue
			}
			calleeID, ok := idByUSR[calleeUSR]
			if !ok {
				continue
			}
			k := edgeKey{callerID, calleeID}
			if !seen.callEdge[k] {
				if _, err := edgeStmt.Exec(callerID, calleeID); err != nil {
					return 0, fmt.Errorf("ingest: call_edges insert: %w", err)
				}
				seen.callEdge[k] = true
				n++
			}
			inlined := 0
			if call.Inlined {
				inlined = 1
			}
			seen.inlineFlag[k] = seen.inlineFlag[k] | inlined
		}
	}
	return n, nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k == "" {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
