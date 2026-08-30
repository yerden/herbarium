package ingest

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/yerden/herbarium/internal/builddir"
	"github.com/yerden/herbarium/internal/linkplane"
	"github.com/yerden/herbarium/internal/mesonintrospect"
	"github.com/yerden/herbarium/internal/usr"
)

// LinkSummary counts what Link wrote. Displayed to the user.
type LinkSummary struct {
	LinkResolutions int
	ObjdumpEdges    int
}

// Link ingests the link-plane facts: nm on each linked binary, its
// .map file if configured, and objdump for the post-optimization edge
// view. Populates link_resolutions and call_edges (source='objdump').
// symbol_reachability is a view over link_resolutions; no separate
// write is needed.
//
// Depends on the Compiler + DWARF + Targets passes — symbols must
// already exist, and targetIDs must map each Meson target name to its
// row id. objectToSource (builddir-relative object → project-relative
// source, from Summary.ObjectToSource) is used to disambiguate same-
// named internal-linkage symbols; empty is tolerated (the pass falls
// back to name-based lookup for objects it cannot translate).
func Link(db *sql.DB, bd *builddir.BuildDir, intro *mesonintrospect.Introspection, pr *PathResolver, targetIDs map[string]int64, objectToSource map[string]string) (LinkSummary, error) {
	nameToID, usrByID, err := buildSymbolLookup(db)
	if err != nil {
		return LinkSummary{}, err
	}
	usrToID := make(map[string]int64, len(usrByID))
	for id, u := range usrByID {
		usrToID[u] = id
	}

	tx, err := db.Begin()
	if err != nil {
		return LinkSummary{}, fmt.Errorf("ingest/link: begin: %w", err)
	}
	defer tx.Rollback()

	lrStmt, err := tx.Prepare(`INSERT INTO link_resolutions
		(target_id, usr, winning_object, linkage_kind, losing_objects, archive)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return LinkSummary{}, fmt.Errorf("ingest/link: prepare link_resolutions: %w", err)
	}
	defer lrStmt.Close()

	edgeStmt, err := tx.Prepare(`INSERT INTO call_edges
		(caller_id, callee_id, source, target_id)
		VALUES (?, ?, 'objdump', ?)`)
	if err != nil {
		return LinkSummary{}, fmt.Errorf("ingest/link: prepare objdump edges: %w", err)
	}
	defer edgeStmt.Close()

	// Precompute map: target name → linker map path (top-level builddir).
	mapByTarget := indexMapFiles(bd)

	var sum LinkSummary
	// Sort targets deterministically.
	targets := append([]mesonintrospect.Target(nil), intro.Targets...)
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })

	for _, t := range targets {
		if t.Kind != "executable" && t.Kind != "shared_library" {
			// Static libraries have no linked binary to inspect; skip.
			continue
		}
		if len(t.Filenames) == 0 {
			continue
		}
		binary := t.Filenames[0]
		targetID, ok := targetIDs[t.Name]
		if !ok {
			continue
		}

		syms, err := linkplane.RunNMDefined(binary)
		if err != nil {
			return LinkSummary{}, err
		}

		var mf *linkplane.MapFile
		if p := mapByTarget[t.Name]; p != "" {
			mf, err = linkplane.ReadMap(p)
			if err != nil {
				return LinkSummary{}, err
			}
		}

		// Build the per-target address-based resolver: each defined
		// function symbol in this binary maps to its symbols row id via
		// USR construction. For internal-linkage rows the source path is
		// pulled from the map file's SymbolOrigin → objectToSource chain;
		// this is what disambiguates same-named statics across TUs.
		addrToID := buildAddrIndex(syms, mf, objectToSource, usrToID, nameToID)

		// link_resolutions: one row per symbol in this binary that we
		// know about at the source level.
		for _, s := range syms {
			// Only functions and data we might track. Skip debugging,
			// runtime, and reserved sections we didn't index.
			if s.LinkageKind() == "" {
				continue
			}
			symID, ok := resolveNMSymbol(s, mf, objectToSource, usrToID, nameToID)
			if !ok {
				continue
			}

			winningObj := ""
			archive := ""
			if mf != nil {
				if orig, ok := mf.SymbolOrigin[s.Name]; ok {
					winningObj = orig
					archive = linkplane.ArchiveFor(orig)
				}
			}
			losingJSON, _ := json.Marshal([]string{})
			if _, err := lrStmt.Exec(targetID, usrByID[symID], winningObj, s.LinkageKind(), string(losingJSON), archive); err != nil {
				return LinkSummary{}, fmt.Errorf("ingest/link: link_resolutions %s: %w", s.Name, err)
			}
			sum.LinkResolutions++
		}

		// objdump: direct call edges. Resolve by address first — that
		// picks the correct USR when a name maps to multiple statics in
		// different TUs (each has a distinct address in the binary).
		// Fall back to name for PLT stubs (callee addr points at the
		// trampoline, not a symbol we track) and any gaps in the addr
		// map.
		edges, err := linkplane.RunObjdump(binary)
		if err != nil {
			return LinkSummary{}, err
		}
		seen := map[edgeKey]bool{}
		for _, e := range edges {
			callerID, ok := addrToID[e.CallerAddr]
			if !ok {
				callerID, ok = nameToID[e.Caller]
				if !ok {
					continue
				}
			}
			calleeID, ok := addrToID[e.CalleeAddr]
			if !ok {
				calleeID, ok = nameToID[e.CalleeStripped()]
				if !ok {
					continue
				}
			}
			k := edgeKey{callerID, calleeID}
			if seen[k] {
				continue
			}
			seen[k] = true
			if _, err := edgeStmt.Exec(callerID, calleeID, targetID); err != nil {
				return LinkSummary{}, fmt.Errorf("ingest/link: call_edges: %w", err)
			}
			sum.ObjdumpEdges++
		}
	}

	if err := tx.Commit(); err != nil {
		return LinkSummary{}, fmt.Errorf("ingest/link: commit: %w", err)
	}
	return sum, nil
}

// resolveNMSymbol maps one nm-defined symbol in a linked binary to its
// row id in `symbols`. For external-linkage rows the USR is
// `c:@F@<name>` (or `@V@` for data), so a name lookup is unambiguous.
// For internal-linkage rows the USR needs the defining TU's source
// path; we recover it from the map file's SymbolOrigin → objectToSource
// chain. Returns ok=false when we can't attribute the symbol to a known
// source row — the caller skips such symbols rather than pick a wrong
// candidate. Without a map file we cannot disambiguate internal-linkage
// names, so we defer to nameToID's prefer-defined heuristic.
func resolveNMSymbol(s linkplane.NMSymbol, mf *linkplane.MapFile, objectToSource map[string]string, usrToID, nameToID map[string]int64) (int64, bool) {
	if !s.Local() {
		// External linkage: unique USR, prefer the definition-bearing row
		// via the shared name lookup.
		id, ok := nameToID[s.Name]
		return id, ok
	}
	if mf != nil {
		obj, ok := mf.SymbolOrigin[s.Name]
		if ok {
			if src, ok := objectToSource[obj]; ok {
				u := usr.Function(src, s.Name)
				if s.Kind == "d" || s.Kind == "b" || s.Kind == "r" {
					u = usr.Variable(src, s.Name)
				}
				if id, ok := usrToID[u]; ok {
					return id, true
				}
			}
		}
	}
	// No map file or unresolvable object: fall back to the shared name
	// lookup — imperfect for same-named statics across TUs, but the
	// prefer-defined guard at least keeps declaration-only rows from
	// winning.
	id, ok := nameToID[s.Name]
	return id, ok
}

// buildAddrIndex maps every defined-function address in the linked
// binary to the correct symbols row id, per the same disambiguation
// rules as resolveNMSymbol. objdump's edge resolver keys on this map
// so a same-named static in a different TU doesn't collapse.
func buildAddrIndex(syms []linkplane.NMSymbol, mf *linkplane.MapFile, objectToSource map[string]string, usrToID, nameToID map[string]int64) map[uint64]int64 {
	out := make(map[uint64]int64, len(syms))
	for _, s := range syms {
		if !s.IsFunction() || s.Address == "" {
			continue
		}
		addr, err := strconv.ParseUint(s.Address, 16, 64)
		if err != nil {
			continue
		}
		id, ok := resolveNMSymbol(s, mf, objectToSource, usrToID, nameToID)
		if !ok {
			continue
		}
		out[addr] = id
	}
	return out
}

// buildSymbolLookup returns two maps:
//   nameToID:  every canonical name + linkage_names → symbol row id
//   usrByID:   symbol row id → USR (for link_resolutions.usr column)
//
// When a name resolves to multiple symbols row (same-named statics across
// TUs, or a declaration-only row that shares a name with a defined one),
// rows that have symbol_definitions win — a declaration-only row must
// never win runtime-edge or link-resolution lookups (it would surface as
// a phantom "undefined" and hide the real defined callee). Ties among
// definition-bearing rows are still arbitrary; address-based
// disambiguation (see addrIndex below) handles those.
func buildSymbolLookup(db *sql.DB) (map[string]int64, map[int64]string, error) {
	rows, err := db.Query(`
		SELECT s.id, s.name, s.usr, s.linkage_names,
		       EXISTS (SELECT 1 FROM symbol_definitions sd WHERE sd.symbol_id = s.id) AS has_def
		FROM symbols s`)
	if err != nil {
		return nil, nil, fmt.Errorf("ingest/link: query symbols: %w", err)
	}
	defer rows.Close()

	type entry struct {
		id     int64
		hasDef bool
	}
	nameEntry := map[string]entry{}
	usrByID := map[int64]string{}
	put := func(name string, e entry) {
		if name == "" {
			return
		}
		if cur, ok := nameEntry[name]; ok {
			// A defined row must not be displaced by a declaration-only row.
			if cur.hasDef && !e.hasDef {
				return
			}
			// A declaration-only incumbent yields to any defined row.
			if !cur.hasDef && e.hasDef {
				nameEntry[name] = e
				return
			}
			// Otherwise both sides agree on has-def; keep the first entry
			// so behavior is deterministic when true ambiguity exists.
			return
		}
		nameEntry[name] = e
	}
	for rows.Next() {
		var id int64
		var name, usrValue string
		var linkageNames sql.NullString
		var hasDef bool
		if err := rows.Scan(&id, &name, &usrValue, &linkageNames, &hasDef); err != nil {
			return nil, nil, fmt.Errorf("ingest/link: scan symbol: %w", err)
		}
		usrByID[id] = usrValue
		e := entry{id: id, hasDef: hasDef}
		put(name, e)
		if linkageNames.Valid && linkageNames.String != "" {
			var names []string
			if err := json.Unmarshal([]byte(linkageNames.String), &names); err == nil {
				for _, n := range names {
					put(n, e)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	nameToID := make(map[string]int64, len(nameEntry))
	for n, e := range nameEntry {
		nameToID[n] = e.id
	}
	return nameToID, usrByID, nil
}

// indexMapFiles maps target name → linker map path when present.
// GNU ld with `-Wl,-Map=X.map` (via meson.build) emits X.map at the
// builddir root.
func indexMapFiles(bd *builddir.BuildDir) map[string]string {
	out := map[string]string{}
	for _, m := range bd.LinkerMaps {
		base := filepath.Base(m)
		// "app1.map" → target "app1"
		name := strings.TrimSuffix(base, ".map")
		out[name] = m
	}
	return out
}
