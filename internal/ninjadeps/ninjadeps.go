// Package ninjadeps reads ninja's binary dependency log (`.ninja_deps`)
// from a Meson/ninja build directory. Ninja folds every per-object
// Makefile-style `<obj>.d` file emitted by GCC into this single log and
// deletes the sidecars, so it is the only reliable per-TU header-set
// source in a modern Meson build.
//
// Format (ninja src/deps_log.{h,cc}, version 4):
//
//   Header: "# ninjadeps\n" then u32 little-endian version (4).
//
//   Records: each record starts with a u32 little-endian header word.
//   Bit 31 = 1 → deps record; bit 31 = 0 → path record. Bits 30..0 = the
//   size of the record body in bytes. The header itself is NOT included
//   in that size.
//
//   Path record body:
//     - path bytes (null-padded so the total body is a multiple of 4)
//     - trailing u32: ~id (bitwise NOT of the path's id, i.e. the record
//       index among path records so far). Used as a validation checksum.
//     Path length in bytes = size - 4; strip trailing NULs to recover the
//     original path.
//
//   Deps record body (version 4):
//     - u32 out_id (references a previously-registered path)
//     - u32 mtime_low
//     - u32 mtime_high
//     - remaining (size-12)/4 u32s: input ids in dependency order.
//
// Newer deps records for the same out_id supersede earlier ones —
// replaying the log in order and letting later writes overwrite yields
// the effective state ninja would use.
package ninjadeps

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// magic is the 12-byte header ninja writes at the start of .ninja_deps.
const magic = "# ninjadeps\n"

// SupportedVersion is the only deps log version herbarium accepts. Older
// versions have different field layouts (32-bit mtime, etc.); we refuse
// rather than misparse silently.
const SupportedVersion uint32 = 4

// Log is the parsed contents of a .ninja_deps file. Paths are stored as
// ninja saw them — typically relative to the builddir (e.g. `../lib/x.c`
// or `lib/foo.p/bar.c.o`) or absolute for system headers. Callers convert
// to project-relative via ingest.PathResolver.
type Log struct {
	// Version is the file's declared version (equal to SupportedVersion
	// on any successfully-parsed file).
	Version uint32
	// Deps maps each output path to its transitive input paths, in the
	// order ninja recorded them. If ninja rewrote the deps for an output
	// mid-log the last record wins.
	Deps map[string][]string
}

// Read parses the .ninja_deps file at path. Returns a non-nil Log even
// on partial files: ninja tolerates truncation (a crash mid-write leaves
// the tail unreadable) and re-derives on the next build. We do the same:
// stop at the first short read and return whatever we validated up to
// that point.
func Read(path string) (*Log, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ninjadeps: open %q: %w", path, err)
	}
	defer f.Close()

	header := make([]byte, len(magic))
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, fmt.Errorf("ninjadeps: read header %q: %w", path, err)
	}
	if string(header) != magic {
		return nil, fmt.Errorf("ninjadeps: %q: bad magic %q", path, header)
	}

	var version uint32
	if err := binary.Read(f, binary.LittleEndian, &version); err != nil {
		return nil, fmt.Errorf("ninjadeps: read version %q: %w", path, err)
	}
	if version != SupportedVersion {
		return nil, fmt.Errorf("ninjadeps: %q: version %d unsupported (want %d)", path, version, SupportedVersion)
	}

	log := &Log{Version: version, Deps: map[string][]string{}}
	// paths[i] is the string registered by the i-th path record.
	var paths []string

	buf := make([]byte, 0, 4096)
	for {
		var head uint32
		if err := binary.Read(f, binary.LittleEndian, &head); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// Short read at end-of-log: ninja tolerates this.
			return log, nil
		}
		isDeps := (head >> 31) & 1
		size := head & 0x7fffffff
		if size < 4 || size > (1<<24) {
			// Ninja's size cap is generous but not infinite; a gigantic
			// or too-small size means the log is corrupt or truncated.
			return log, nil
		}
		if cap(buf) < int(size) {
			buf = make([]byte, size)
		} else {
			buf = buf[:size]
		}
		if _, err := io.ReadFull(f, buf); err != nil {
			// Trailing partial record — return what we have.
			return log, nil
		}

		if isDeps == 1 {
			if size < 12 || (size-12)%4 != 0 {
				return log, fmt.Errorf("ninjadeps: %q: malformed deps record size %d", path, size)
			}
			outID := binary.LittleEndian.Uint32(buf[0:4])
			// mtime_low = buf[4:8], mtime_high = buf[8:12] — unused.
			numDeps := (int(size) - 12) / 4
			if int(outID) >= len(paths) {
				// deps record references a path we haven't registered
				// yet; can only happen on a truncated/corrupt log.
				continue
			}
			out := paths[outID]
			inputs := make([]string, 0, numDeps)
			for i := range numDeps {
				id := binary.LittleEndian.Uint32(buf[12+i*4 : 16+i*4])
				if int(id) >= len(paths) {
					continue
				}
				inputs = append(inputs, paths[id])
			}
			// Later deps records for the same out_id supersede earlier
			// ones. Overwrite unconditionally.
			log.Deps[out] = inputs
			continue
		}

		// Path record: last 4 bytes = ~id checksum, preceding bytes = path
		// with trailing null padding.
		pathLen := int(size) - 4
		raw := buf[:pathLen]
		// Strip trailing NULs (padding).
		for len(raw) > 0 && raw[len(raw)-1] == 0 {
			raw = raw[:len(raw)-1]
		}
		checksum := binary.LittleEndian.Uint32(buf[pathLen:])
		expected := ^uint32(len(paths))
		if checksum != expected {
			return log, fmt.Errorf("ninjadeps: %q: path record %d checksum %#x != %#x", path, len(paths), checksum, expected)
		}
		paths = append(paths, string(raw))
	}
	return log, nil
}

// DepsFor returns the transitive inputs ninja recorded for the given
// output. The output must be builddir-relative with forward slashes (the
// same shape ninja stores). Returns (nil, false) if ninja never recorded
// a deps entry for it.
func (l *Log) DepsFor(output string) ([]string, bool) {
	deps, ok := l.Deps[output]
	return deps, ok
}

// ReadForBuildDir is a convenience: reads <builddir>/.ninja_deps.
// Returns (nil, nil) if the file does not exist (source packing degrades
// to just the target sources without headers). A parse error is
// returned to the caller.
func ReadForBuildDir(builddir string) (*Log, error) {
	p := filepath.Join(builddir, ".ninja_deps")
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("ninjadeps: stat %q: %w", p, err)
	}
	return Read(p)
}
