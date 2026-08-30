package ingest

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/yerden/herbarium/internal/linkplane"
)

// TestBuildSymbolLookupPrefersDefined guards Defect 2: a name→id map
// built from `symbols` must never let a declaration-only row (no
// symbol_definitions row) win over a defined one that shares the same
// name. Left unguarded, list_undefined_symbols surfaces the internal
// static as "unresolved external" and list_linked_callees mis-attributes
// the edge to the wrong TU.
func TestBuildSymbolLookupPrefersDefined(t *testing.T) {
	db := newTestDB(t)
	// Two rows share the name "flw_net_parse_mpls": one decl-only in
	// serializer.c, one defined in net.c. Insertion order intentionally
	// puts the decl-only row last so the pre-fix code (last-write-wins)
	// would pick the wrong id.
	mustExec(t, db, `INSERT INTO symbols(id, usr, name, kind) VALUES
		(10, 'c:lib/net/net.c@F@flw_net_parse_mpls',        'flw_net_parse_mpls', 'function'),
		(20, 'c:lib/net/serializer.c@F@flw_net_parse_mpls', 'flw_net_parse_mpls', 'function')`)
	mustExec(t, db, `INSERT INTO symbol_definitions(symbol_id, file, line) VALUES
		(10, 'lib/net/net.c', 42)`)

	nameToID, _, err := buildSymbolLookup(db)
	if err != nil {
		t.Fatalf("buildSymbolLookup: %v", err)
	}
	if got := nameToID["flw_net_parse_mpls"]; got != 10 {
		t.Errorf("nameToID[flw_net_parse_mpls] = %d, want 10 (defined row)", got)
	}
}

// TestResolveNMSymbolInternalLinkage guards Defect 1: the runtime edge
// resolver picks the correct USR for a same-named internal-linkage
// symbol by chaining nm.name → map.SymbolOrigin → objectToSource →
// c:<src>@F@<name>. Without the chain, both TUs' rows share the same
// name and the resolver would coin-flip.
func TestResolveNMSymbolInternalLinkage(t *testing.T) {
	// Two static `flw_net_parse_mpls` in different TUs. Only net.c's
	// USR ever appears in nameToID (prefer-defined guard), so the
	// address-based resolver must NOT fall through to nameToID for
	// serializer.c's copy — it has to construct the right USR from the
	// map file's object attribution.
	usrToID := map[string]int64{
		"c:lib/net/net.c@F@flw_net_parse_mpls":        10,
		"c:lib/net/serializer.c@F@flw_net_parse_mpls": 20,
	}
	nameToID := map[string]int64{"flw_net_parse_mpls": 10}
	objectToSource := map[string]string{
		"lib/libnet.a.p/net.c.o":        "lib/net/net.c",
		"lib/libnet.a.p/serializer.c.o": "lib/net/serializer.c",
	}
	mf := &linkplane.MapFile{SymbolOrigin: map[string]string{
		"flw_net_parse_mpls": "lib/libnet.a.p/serializer.c.o",
	}}
	s := linkplane.NMSymbol{Name: "flw_net_parse_mpls", Kind: "t", Address: "11a0"}

	got, ok := resolveNMSymbol(s, mf, objectToSource, usrToID, nameToID)
	if !ok {
		t.Fatalf("resolveNMSymbol: not found")
	}
	if got != 20 {
		t.Errorf("resolveNMSymbol = %d, want 20 (serializer.c copy)", got)
	}
}

// TestResolveNMSymbolExternalLinkage confirms external-linkage names
// still resolve via the shared name lookup (unique USR, no
// disambiguation needed).
func TestResolveNMSymbolExternalLinkage(t *testing.T) {
	usrToID := map[string]int64{"c:@F@compute": 42}
	nameToID := map[string]int64{"compute": 42}
	s := linkplane.NMSymbol{Name: "compute", Kind: "T", Address: "11e0"}

	got, ok := resolveNMSymbol(s, nil, nil, usrToID, nameToID)
	if !ok || got != 42 {
		t.Errorf("resolveNMSymbol = (%d, %v), want (42, true)", got, ok)
	}
}

// TestBuildAddrIndexUsesResolveNMSymbol verifies buildAddrIndex keys the
// map by the nm-reported address of each defined function, using
// resolveNMSymbol's disambiguation.
func TestBuildAddrIndexUsesResolveNMSymbol(t *testing.T) {
	usrToID := map[string]int64{
		"c:lib/a.c@F@foo": 1,
		"c:lib/b.c@F@foo": 2,
	}
	nameToID := map[string]int64{"foo": 1} // decl-preference already applied
	objectToSource := map[string]string{
		"a.p/a.c.o": "lib/a.c",
		"a.p/b.c.o": "lib/b.c",
	}
	mf := &linkplane.MapFile{SymbolOrigin: map[string]string{
		"foo": "a.p/b.c.o", // this binary's foo came from b.c
	}}
	syms := []linkplane.NMSymbol{
		{Name: "foo", Kind: "t", Address: "1234"},
	}
	got := buildAddrIndex(syms, mf, objectToSource, usrToID, nameToID)
	if id := got[0x1234]; id != 2 {
		t.Errorf("addrToID[0x1234] = %d, want 2 (b.c copy per map file)", id)
	}
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `CREATE TABLE symbols (
		id INTEGER PRIMARY KEY, usr TEXT, name TEXT, kind TEXT,
		linkage TEXT, signature TEXT, address_taken INTEGER, linkage_names TEXT)`)
	mustExec(t, db, `CREATE TABLE symbol_definitions (
		id INTEGER PRIMARY KEY, symbol_id INTEGER, file TEXT, line INTEGER,
		decl_file TEXT, decl_line INTEGER, is_weak INTEGER, linkage_name TEXT)`)
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
