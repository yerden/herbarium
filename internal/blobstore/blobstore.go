// Package blobstore writes source files into the .hbr as content-addressed,
// zstd-compressed blobs and maintains the sources → blob mapping.
package blobstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Writer batches inserts inside a single transaction. Callers open one
// per ingest run and Close when done. Not goroutine-safe: use one Writer
// per goroutine or serialize externally.
type Writer struct {
	tx  *sql.Tx
	enc *zstd.Encoder
}

// New starts a new blobstore transaction on db. Commit or Rollback via
// the returned Writer's methods.
func New(db *sql.DB) (*Writer, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("blobstore: begin: %w", err)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("blobstore: zstd encoder: %w", err)
	}
	return &Writer{tx: tx, enc: enc}, nil
}

// PutResult is what Put returns for callers that want to know whether the
// blob was newly inserted or deduplicated.
type PutResult struct {
	Hash        string // sha256 hex of raw bytes
	Size        int    // raw byte length before compression
	Deduplicated bool  // true if the blob already existed
}

// Put stores content under project-relative path, keyed by SHA-256 of the
// raw bytes. If a blob with the same hash already exists it is reused.
// The (path → hash) row in sources is always upserted so re-ingest
// after a source rename produces a fresh mapping to the same blob.
func (w *Writer) Put(path string, content []byte, isGenerated bool) (PutResult, error) {
	res, err := w.putBlob(content)
	if err != nil {
		return PutResult{}, err
	}

	gen := 0
	if isGenerated {
		gen = 1
	}
	if _, err := w.tx.Exec(
		`INSERT INTO sources(path, blob_hash, is_generated) VALUES (?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET blob_hash=excluded.blob_hash, is_generated=excluded.is_generated`,
		path, res.Hash, gen,
	); err != nil {
		return PutResult{}, fmt.Errorf("blobstore: upsert source %q: %w", path, err)
	}
	return res, nil
}

// PutExternal stores content under an absolute path in external_sources.
// Same blob-dedup semantics as Put; the two tables share the blobs table,
// so a system header byte-identical to an in-project file collapses to a
// single blob.
func (w *Writer) PutExternal(absPath string, content []byte) (PutResult, error) {
	res, err := w.putBlob(content)
	if err != nil {
		return PutResult{}, err
	}
	if _, err := w.tx.Exec(
		`INSERT INTO external_sources(abs_path, blob_hash) VALUES (?, ?)
		 ON CONFLICT(abs_path) DO UPDATE SET blob_hash=excluded.blob_hash`,
		absPath, res.Hash,
	); err != nil {
		return PutResult{}, fmt.Errorf("blobstore: upsert external %q: %w", absPath, err)
	}
	return res, nil
}

// PutGenerated stores content under a builddir-relative path in
// generated_sources. Same blob-dedup semantics as Put and PutExternal.
// The key is builddir-relative (not absolute) so the value stays portable
// across machines with different builddir locations.
func (w *Writer) PutGenerated(builddirRel string, content []byte) (PutResult, error) {
	res, err := w.putBlob(content)
	if err != nil {
		return PutResult{}, err
	}
	if _, err := w.tx.Exec(
		`INSERT INTO generated_sources(builddir_rel, blob_hash) VALUES (?, ?)
		 ON CONFLICT(builddir_rel) DO UPDATE SET blob_hash=excluded.blob_hash`,
		builddirRel, res.Hash,
	); err != nil {
		return PutResult{}, fmt.Errorf("blobstore: upsert generated %q: %w", builddirRel, err)
	}
	return res, nil
}

// putBlob inserts a blob if new, returning the hash + dedup flag. Shared
// between Put and PutExternal so the two paths can't drift in how they
// hash or compress.
func (w *Writer) putBlob(content []byte) (PutResult, error) {
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	var exists int
	if err := w.tx.QueryRow(
		`SELECT COUNT(*) FROM blobs WHERE hash = ?`, hash,
	).Scan(&exists); err != nil {
		return PutResult{}, fmt.Errorf("blobstore: probe %s: %w", hash, err)
	}
	deduped := exists > 0
	if !deduped {
		compressed := w.enc.EncodeAll(content, nil)
		if _, err := w.tx.Exec(
			`INSERT INTO blobs(hash, size, content) VALUES (?, ?, ?)`,
			hash, len(content), compressed,
		); err != nil {
			return PutResult{}, fmt.Errorf("blobstore: insert blob %s: %w", hash, err)
		}
	}
	return PutResult{Hash: hash, Size: len(content), Deduplicated: deduped}, nil
}

// Commit finalizes all writes.
func (w *Writer) Commit() error {
	if w.tx == nil {
		return errors.New("blobstore: already finalized")
	}
	err := w.tx.Commit()
	w.tx = nil
	w.enc.Close()
	return err
}

// Rollback discards all writes; safe to call after Commit (no-op).
func (w *Writer) Rollback() {
	if w.tx != nil {
		w.tx.Rollback()
		w.tx = nil
		w.enc.Close()
	}
}
