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
//   - symbol_definitions.file / line ← repaired for functions GCC
//     inlined everywhere, which get no .ci node and so reached this
//     pass carrying the Compiler pass's TU fallback (see defLocStmt)
//   - indirect_call_sites ← DW_TAG_call_site without DW_AT_call_origin,
//     with file/line/column resolved via the CU's line table, plus the
//     callee_type/field_hint dwarfingest recovered for the site
//   - inline_instances ← DW_TAG_inlined_subroutine, the inlined bodies
//     that survived into the object's code. This is the only plane that
//     sees the early inliner's work; the .cgraph and .inline dumps are
//     written after it has already run.
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

	// A function GCC inlined at every call site gets no node in the .ci
	// dump — callgraph-info only describes functions that reached the
	// assembler — so the Compiler pass had nothing to anchor its def row
	// to and fell back to the TU path with line 0. DWARF's abstract
	// instance root is the only place the real location survives, and for
	// a static inline written in a header that location is the header,
	// not the TU. line = 0 is exactly the set of rows the fallback
	// produced; a .ci-anchored row is already authoritative.
	defLocStmt, err := tx.Prepare(`
		UPDATE symbol_definitions SET file = ?, line = ?
		WHERE symbol_id = ? AND line = 0`)
	if err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: prepare def location update: %w", err)
	}
	defer defLocStmt.Close()

	indirectStmt, err := tx.Prepare(`
		INSERT INTO indirect_call_sites
		  (caller_id, file, line, column, callee_type, field_hint)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: prepare indirect insert: %w", err)
	}
	defer indirectStmt.Close()

	// ON CONFLICT DO NOTHING is the dedup: a type declared in a header
	// has one file-scoped USR and one identical DIE in every TU that
	// included it, so the first object to mention it wins. Objects are
	// walked in sorted order, so "first" is deterministic.
	typeStmt, err := tx.Prepare(`
		INSERT INTO types (usr, name, kind, decl_file, decl_line, byte_size, underlying)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(usr) DO NOTHING`)
	if err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: prepare types insert: %w", err)
	}
	defer typeStmt.Close()

	fieldStmt, err := tx.Prepare(`
		INSERT INTO type_fields (type_id, name, type, ordinal, byte_offset)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: prepare type_fields insert: %w", err)
	}
	defer fieldStmt.Close()

	enumConstStmt, err := tx.Prepare(`
		INSERT INTO enum_constants (usr, type_id, name, value)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(usr) DO NOTHING`)
	if err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: prepare enum_constants insert: %w", err)
	}
	defer enumConstStmt.Close()

	inlineStmt, err := tx.Prepare(`
		INSERT INTO inline_instances
		  (callee_id, caller_id, parent_callee_id, depth, file, line, column, object)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: prepare inline_instances insert: %w", err)
	}
	defer inlineStmt.Close()

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
			if sp.AbstractInline && sp.DeclFile != "" && sp.DeclLine > 0 {
				defRel := pr.ToProjectRelative(sp.DeclFile).Rel
				res, err := defLocStmt.Exec(defRel, sp.DeclLine, symID)
				if err != nil {
					return DwarfSummary{}, fmt.Errorf("ingest/dwarf: def location update %s: %w", sp.Name, err)
				}
				if n, _ := res.RowsAffected(); n > 0 {
					sum.DefLocations += int(n)
				}
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

		// Inlined bodies present in this object.
		objRel := art.Object
		if rel, err := filepath.Rel(bd.Root, art.Object); err == nil {
			objRel = rel
		}
		for _, ii := range info.InlineInstances {
			calleeID := resolveSymbolID(ii.CalleeName, tuSourcePath, idByUSR)
			callerID := resolveSymbolID(ii.CallerName, tuSourcePath, idByUSR)
			if calleeID == 0 || callerID == 0 {
				continue
			}
			// A nested instance whose parent doesn't resolve still
			// records the fold and its depth — only the parent link is
			// lost, and NULL says so.
			var parentID any
			if ii.ParentCalleeName != "" {
				if id := resolveSymbolID(ii.ParentCalleeName, tuSourcePath, idByUSR); id != 0 {
					parentID = id
				}
			}
			fileRel := pr.ToProjectRelative(ii.File).Rel
			if _, err := inlineStmt.Exec(
				calleeID, callerID, parentID, ii.Depth,
				fileRel, ii.Line, ii.Column, objRel,
			); err != nil {
				return DwarfSummary{}, fmt.Errorf("ingest/dwarf: inline_instances insert: %w", err)
			}
			sum.InlineInstances++
		}

		// Types. Declared-but-unused types are absent from DWARF at every
		// -g level, so this plane is "types the compiler emitted", with
		// the same reached-the-assembler caveat `symbols` carries.
		insertType := func(name, kind, declFile string, line, col, byteSize int, underlying string) (int64, bool, error) {
			// A type declared outside --project-root is a system or
			// vendored header's — indexing those would bury the project's
			// own types under libc's. Mirrors the appendix's rule that
			// out-of-root sources are not given USRs.
			rp := pr.ToProjectRelative(declFile)
			if declFile == "" || !rp.InProject {
				return 0, false, nil
			}
			var u string
			switch kind {
			case "typedef":
				if name == "" {
					return 0, false, nil
				}
				u = usr.Typedef(rp.Rel, name)
			case "struct":
				u = usr.Struct(rp.Rel, name, line, col)
			case "union":
				u = usr.Union(rp.Rel, name, line, col)
			case "enum":
				u = usr.Enum(rp.Rel, name, line, col)
			}
			var sizeArg any
			if byteSize > 0 {
				sizeArg = byteSize
			}
			res, err := typeStmt.Exec(u, name, kind, rp.Rel, line, sizeArg, underlying)
			if err != nil {
				return 0, false, fmt.Errorf("ingest/dwarf: types insert %s: %w", u, err)
			}
			// RowsAffected 0 means another TU already contributed this
			// type; its children are already in, so the caller skips them.
			if n, _ := res.RowsAffected(); n == 0 {
				return 0, false, nil
			}
			id, err := res.LastInsertId()
			if err != nil {
				return 0, false, fmt.Errorf("ingest/dwarf: types id %s: %w", u, err)
			}
			sum.Types++
			return id, true, nil
		}

		for _, st := range info.Structs {
			id, fresh, err := insertType(st.Name, st.Kind, st.DeclFile, st.DeclLine, st.DeclColumn, st.ByteSize, "")
			if err != nil {
				return DwarfSummary{}, err
			}
			if !fresh {
				continue
			}
			for i, f := range st.Fields {
				if _, err := fieldStmt.Exec(id, f.Name, f.Type, i, f.ByteOffset); err != nil {
					return DwarfSummary{}, fmt.Errorf("ingest/dwarf: type_fields insert: %w", err)
				}
				sum.TypeFields++
			}
		}

		for _, td := range info.Typedefs {
			if _, _, err := insertType(td.Name, "typedef", td.DeclFile, td.DeclLine, 0, 0, td.Target); err != nil {
				return DwarfSummary{}, err
			}
		}

		for _, en := range info.Enums {
			id, fresh, err := insertType(en.Name, "enum", en.DeclFile, en.DeclLine, en.DeclColumn, en.ByteSize, "")
			if err != nil {
				return DwarfSummary{}, err
			}
			if !fresh {
				continue
			}
			rp := pr.ToProjectRelative(en.DeclFile)
			for _, c := range en.Constants {
				cu := usr.EnumMember(rp.Rel, en.Name, en.DeclLine, en.DeclColumn, c.Name)
				if _, err := enumConstStmt.Exec(cu, id, c.Name, c.Value); err != nil {
					return DwarfSummary{}, fmt.Errorf("ingest/dwarf: enum_constants insert: %w", err)
				}
				sum.EnumConstants++
			}
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
	if _, err := tx.Exec(
		`INSERT INTO types_fts(types_fts) VALUES ('rebuild')`,
	); err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: types fts rebuild: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return DwarfSummary{}, fmt.Errorf("ingest/dwarf: commit: %w", err)
	}
	return sum, nil
}

// DwarfSummary counts what the DWARF pass wrote. Displayed after
// `herbarium collect` so users see whether DWARF enrichment fired.
type DwarfSummary struct {
	Signatures      int
	DeclLocations   int
	DefLocations    int
	IndirectSites   int
	InlineInstances int
	Types           int
	TypeFields      int
	EnumConstants   int
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
