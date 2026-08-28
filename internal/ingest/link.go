package ingest

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yerden/herbarium/internal/builddir"
	"github.com/yerden/herbarium/internal/linkplane"
	"github.com/yerden/herbarium/internal/mesonintrospect"
)

// LinkSummary counts what Link wrote. Displayed to the user.
type LinkSummary struct {
	LinkResolutions int
	ObjdumpEdges    int
	Reachability    int
}

// Link ingests the link-plane facts: nm on each linked binary, its
// .map file if configured, and objdump for the post-optimization edge
// view. Populates link_resolutions, call_edges (source='objdump'),
// and symbol_reachability.
//
// Depends on the Compiler + DWARF + Targets passes — symbols must
// already exist, and targetIDs must map each Meson target name to its
// row id.
func Link(db *sql.DB, bd *builddir.BuildDir, intro *mesonintrospect.Introspection, pr *PathResolver, targetIDs map[string]int64) (LinkSummary, error) {
	nameToID, usrByID, err := buildSymbolLookup(db)
	if err != nil {
		return LinkSummary{}, err
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

	reachStmt, err := tx.Prepare(`INSERT INTO symbol_reachability
		(target_id, symbol_id, reachable, section_kept)
		VALUES (?, ?, ?, 1)`)
	if err != nil {
		return LinkSummary{}, fmt.Errorf("ingest/link: prepare reachability: %w", err)
	}
	defer reachStmt.Close()

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

		// link_resolutions: one row per symbol in this binary that we
		// know about at the source level.
		reachable := map[int64]bool{}
		for _, s := range syms {
			// Only functions and data we might track. Skip debugging,
			// runtime, and reserved sections we didn't index.
			if s.LinkageKind() == "" {
				continue
			}
			symID, ok := nameToID[s.Name]
			if !ok {
				continue
			}
			reachable[symID] = true

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

		// objdump: direct call edges.
		edges, err := linkplane.RunObjdump(binary)
		if err != nil {
			return LinkSummary{}, err
		}
		seen := map[edgeKey]bool{}
		for _, e := range edges {
			callerID, ok := nameToID[e.Caller]
			if !ok {
				continue
			}
			calleeID, ok := nameToID[e.CalleeStripped()]
			if !ok {
				continue
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

		// symbol_reachability: for every indexed symbol, record whether
		// the binary contains a definition of it.
		for symID := range usrByID {
			reachInt := 0
			if reachable[symID] {
				reachInt = 1
			}
			if _, err := reachStmt.Exec(targetID, symID, reachInt); err != nil {
				return LinkSummary{}, fmt.Errorf("ingest/link: reachability: %w", err)
			}
			sum.Reachability++
		}
	}

	if err := tx.Commit(); err != nil {
		return LinkSummary{}, fmt.Errorf("ingest/link: commit: %w", err)
	}
	return sum, nil
}

// buildSymbolLookup returns two maps:
//   nameToID:  every canonical name + linkage_names → symbol row id
//   usrByID:   symbol row id → USR (for link_resolutions.usr column)
func buildSymbolLookup(db *sql.DB) (map[string]int64, map[int64]string, error) {
	rows, err := db.Query(`SELECT id, name, usr, linkage_names FROM symbols`)
	if err != nil {
		return nil, nil, fmt.Errorf("ingest/link: query symbols: %w", err)
	}
	defer rows.Close()

	nameToID := map[string]int64{}
	usrByID := map[int64]string{}
	for rows.Next() {
		var id int64
		var name, usrValue string
		var linkageNames sql.NullString
		if err := rows.Scan(&id, &name, &usrValue, &linkageNames); err != nil {
			return nil, nil, fmt.Errorf("ingest/link: scan symbol: %w", err)
		}
		usrByID[id] = usrValue
		if name != "" {
			nameToID[name] = id
		}
		if linkageNames.Valid && linkageNames.String != "" {
			var names []string
			if err := json.Unmarshal([]byte(linkageNames.String), &names); err == nil {
				for _, n := range names {
					if n != "" {
						nameToID[n] = id
					}
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
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
