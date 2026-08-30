package ingest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yerden/herbarium/internal/builddir"
	"github.com/yerden/herbarium/internal/dwarfingest"
	"github.com/yerden/herbarium/internal/usr"
)

// DWARF ingests debug info from every .o's ELF payload, enriching the
// symbols the Compiler pass already wrote:
//
//   - symbols.signature   ← reconstructed from DW_TAG_subprogram + params
//   - symbol_definitions.decl_file / decl_line ← from DWARF's decl_file
//     attribute on the declaration entries (typically the header)
//   - indirect_call_sites ← DW_TAG_call_site without DW_AT_call_origin,
//     with file/line/column resolved via the CU's line table, plus the
//     callee_type/field_hint dwarfingest recovered for the site
//
// Runs AFTER Compiler pass — depends on the symbol/definition rows the
// Compiler pass populated.
func DWARF(db *sql.DB, bd *builddir.BuildDir, pr *PathResolver, idByUSR map[string]int64) (DwarfSummary, error) {
	tx, err := db.Begin()
	if err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: begin: %w", err)
	}
	defer tx.Rollback()

	sigStmt, err := tx.Prepare(`UPDATE symbols SET signature = ? WHERE id = ?`)
	if err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: prepare signature update: %w", err)
	}
	defer sigStmt.Close()

	declStmt, err := tx.Prepare(`
		UPDATE symbol_definitions SET decl_file = ?, decl_line = ?
		WHERE symbol_id = ? AND (decl_file IS NULL OR decl_file = '')`)
	if err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: prepare decl update: %w", err)
	}
	defer declStmt.Close()

	indirectStmt, err := tx.Prepare(`
		INSERT INTO indirect_call_sites
		  (caller_id, file, line, column, callee_type, field_hint)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: prepare indirect insert: %w", err)
	}
	defer indirectStmt.Close()

	var sum DwarfSummary
	// Sort objects deterministically for reproducible signatures on
	// name collisions.
	objs := append([]builddir.ObjectArtifacts(nil), bd.Objects...)
	sort.Slice(objs, func(i, j int) bool { return objs[i].Object < objs[j].Object })

	for _, art := range objs {
		info, err := dwarfingest.Read(art.Object)
		if err != nil {
			return DwarfSummary{}, err
		}

		tuSourcePath := pr.ToProjectRelative(cuFilePath(info)).Rel

		// Subprograms: UPDATE signatures and decl_file/line.
		for _, sp := range info.Subprograms {
			if sp.Name == "" {
				continue
			}
			symID := resolveSymbolID(sp.Name, tuSourcePath, idByUSR)
			if symID == 0 {
				continue
			}
			if sp.Definition && sp.Signature != "" {
				if _, err := sigStmt.Exec(sp.Signature, symID); err != nil {
					return DwarfSummary{}, fmt.Errorf("ingest/dwarf: signature %s: %w", sp.Name, err)
				}
				sum.Signatures++
			}
			// Declaration entries carry decl_file/decl_line pointing
			// at the header where the prototype lives — different from
			// the def's file. Apply to symbol_definitions rows for this
			// symbol that don't yet have a decl file.
			if sp.Declaration && sp.DeclFile != "" && sp.DeclLine > 0 {
				declRel := pr.ToProjectRelative(sp.DeclFile).Rel
				res, err := declStmt.Exec(declRel, sp.DeclLine, symID)
				if err != nil {
					return DwarfSummary{}, fmt.Errorf("ingest/dwarf: decl update %s: %w", sp.Name, err)
				}
				if n, _ := res.RowsAffected(); n > 0 {
					sum.DeclLocations += int(n)
				}
			}
		}

		// Indirect call sites.
		for _, cs := range info.CallSites {
			if !cs.Indirect || cs.SourceCallerName == "" {
				continue
			}
			callerID := resolveSymbolID(cs.SourceCallerName, tuSourcePath, idByUSR)
			if callerID == 0 {
				continue
			}
			fileRel := pr.ToProjectRelative(cs.File).Rel
			if _, err := indirectStmt.Exec(callerID, fileRel, cs.Line, cs.Column, cs.CalleeType, cs.FieldHint); err != nil {
				return DwarfSummary{}, fmt.Errorf("ingest/dwarf: indirect insert: %w", err)
			}
			sum.IndirectSites++
		}
	}

	// Rebuild the FTS index once after all signature updates. FTS5
	// content-mirror tables don't auto-track content-table updates;
	// the 'rebuild' command reconstructs the index from `symbols`.
	if _, err := tx.Exec(
		`INSERT INTO symbols_fts(symbols_fts) VALUES ('rebuild')`,
	); err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: fts rebuild: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: commit: %w", err)
	}
	return sum, nil
}

// DwarfSummary counts what the DWARF pass wrote. Displayed after
// `herbarium collect` so users see whether DWARF enrichment fired.
type DwarfSummary struct {
	Signatures    int
	DeclLocations int
	IndirectSites int
}

// resolveSymbolID tries the external USR form first, then the static
// (file-scoped) form. Returns 0 if neither is present.
func resolveSymbolID(name, tuSourcePath string, idByUSR map[string]int64) int64 {
	if id, ok := idByUSR[usr.Function("", name)]; ok {
		return id
	}
	if tuSourcePath != "" {
		if id, ok := idByUSR[usr.Function(tuSourcePath, name)]; ok {
			return id
		}
	}
	return 0
}

// cuFilePath resolves a CU's DW_AT_name to a path. It's typically
// relative to DW_AT_comp_dir; join if not absolute.
func cuFilePath(info *dwarfingest.Info) string {
	if info.CUFile == "" {
		return ""
	}
	if filepath.IsAbs(info.CUFile) {
		return info.CUFile
	}
	if info.CompDir == "" {
		return info.CUFile
	}
	// info.CUFile may already contain a directory prefix (e.g., "../app1/main.c").
	// Just join and let Clean sort it out.
	joined := filepath.Join(info.CompDir, info.CUFile)
	return filepath.Clean(joined)
}

// StripFilePrefix is a helper for tests. Not used in production.
func StripFilePrefix(s, prefix string) string { return strings.TrimPrefix(s, prefix) }
