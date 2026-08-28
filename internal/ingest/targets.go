package ingest

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/yerden/herbarium/internal/mesonintrospect"
)

// Targets writes the `targets` and `target_sources` tables from Meson
// introspection. Precondition for Phase 4 link ingest, which references
// targets(id) via link_resolutions.
//
// Returns a name → id map so callers can look up target ids by name
// (e.g., "app1" → 3) when populating per-target rows.
func Targets(db *sql.DB, intro *mesonintrospect.Introspection, pr *PathResolver) (map[string]int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("ingest/targets: begin: %w", err)
	}
	defer tx.Rollback()

	tStmt, err := tx.Prepare(`INSERT INTO targets (name, kind, link_command) VALUES (?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("ingest/targets: prepare targets: %w", err)
	}
	defer tStmt.Close()

	sStmt, err := tx.Prepare(`INSERT INTO target_sources (target_id, file) VALUES (?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("ingest/targets: prepare target_sources: %w", err)
	}
	defer sStmt.Close()

	// Sort by name for deterministic ids across runs.
	tgts := append([]mesonintrospect.Target(nil), intro.Targets...)
	sort.Slice(tgts, func(i, j int) bool { return tgts[i].Name < tgts[j].Name })

	idByName := map[string]int64{}
	for _, t := range tgts {
		linkCmd := strings.Join(t.LinkCmd, " ")
		res, err := tStmt.Exec(t.Name, t.Kind, linkCmd)
		if err != nil {
			return nil, fmt.Errorf("ingest/targets: insert %s: %w", t.Name, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		idByName[t.Name] = id

		// Source files → project-relative.
		for _, absSrc := range t.Sources {
			rel := pr.ToProjectRelative(absSrc).Rel
			if _, err := sStmt.Exec(id, rel); err != nil {
				return nil, fmt.Errorf("ingest/targets: source insert %s: %w", absSrc, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("ingest/targets: commit: %w", err)
	}
	return idByName, nil
}
