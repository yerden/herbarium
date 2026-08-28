package blobstore_test

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/yerden/herbarium/internal/blobstore"
	"github.com/yerden/herbarium/internal/store"
)

func TestPutDedupAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hbr")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := store.Init(db); err != nil {
		t.Fatalf("Init: %v", err)
	}

	w, err := blobstore.New(db)
	if err != nil {
		t.Fatalf("blobstore.New: %v", err)
	}

	corpus := map[string][]byte{
		"src/a.c":     []byte("int main(void) { return 0; }\n"),
		"src/b.c":     []byte("int main(void) { return 0; }\n"), // duplicate content, different path
		"include/h.h": []byte("#ifndef H\n#define H\n#endif\n"),
	}

	got := map[string]blobstore.PutResult{}
	for p, c := range corpus {
		r, err := w.Put(p, c, false)
		if err != nil {
			t.Fatalf("Put %q: %v", p, err)
		}
		got[p] = r
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// a.c and b.c must share a hash.
	if got["src/a.c"].Hash != got["src/b.c"].Hash {
		t.Errorf("expected shared hash for a.c and b.c, got %q vs %q",
			got["src/a.c"].Hash, got["src/b.c"].Hash)
	}
	// Exactly one of a.c/b.c should be marked deduplicated (whichever
	// arrived second — map iteration order is random, so we assert the
	// exactly-one property, not which one).
	if got["src/a.c"].Deduplicated == got["src/b.c"].Deduplicated {
		t.Errorf("expected exactly one of a.c/b.c to be marked deduplicated, got a=%v b=%v",
			got["src/a.c"].Deduplicated, got["src/b.c"].Deduplicated)
	}

	// h.h should not be deduplicated (unique content).
	if got["include/h.h"].Deduplicated {
		t.Error("h.h marked deduplicated, expected fresh insert")
	}

	// blobs table should have 2 distinct rows (main-shared + h.h).
	var blobCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobCount); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if blobCount != 2 {
		t.Errorf("blobs row count = %d, want 2", blobCount)
	}

	// sources table should have 3 rows (one per path).
	var srcCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&srcCount); err != nil {
		t.Fatalf("count sources: %v", err)
	}
	if srcCount != 3 {
		t.Errorf("sources row count = %d, want 3", srcCount)
	}

	// Round-trip: fetch the blob for a.c, decompress, confirm bytes match.
	var compressed []byte
	if err := db.QueryRow(
		`SELECT content FROM blobs WHERE hash = ?`, got["src/a.c"].Hash,
	).Scan(&compressed); err != nil {
		t.Fatalf("fetch blob: %v", err)
	}
	dec, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer dec.Close()
	raw, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("zstd decode: %v", err)
	}
	if !bytes.Equal(raw, corpus["src/a.c"]) {
		t.Errorf("round-trip mismatch:\n got: %q\nwant: %q", raw, corpus["src/a.c"])
	}
}
