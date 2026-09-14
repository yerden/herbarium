package mcp_test

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
)

// runSQL executes one statement through sql_query, failing the test on
// a tool-level error.
func runSQL(t *testing.T, client *mcpclient.Client, query string, params []any) herbmcp.SQLResponse {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "sql_query"
	args := map[string]any{"sql": query}
	if params != nil {
		args["params"] = params
	}
	req.Params.Arguments = args
	res, err := client.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("sql_query failed for %q: %s", query, textOf(t, res))
	}
	var payload herbmcp.SQLResponse
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return payload
}

// distinctStrings runs a single-column DISTINCT query and returns the
// non-null values, sorted.
func distinctStrings(t *testing.T, client *mcpclient.Client, table, column string) []string {
	t.Helper()
	resp := runSQL(t, client,
		"SELECT DISTINCT "+column+" FROM "+table+" WHERE "+column+" IS NOT NULL", nil)
	var out []string
	for _, row := range resp.Rows {
		if len(row) == 0 {
			continue
		}
		if v, ok := row[0].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

var namedParam = regexp.MustCompile(`:[A-Za-z_][A-Za-z0-9_]*`)

// Every documented join recipe must be valid SQL against the real
// schema. An agent copies these into sql_query verbatim, so a recipe
// that does not run is worse than no recipe — it reads as a herbarium
// bug from the other side of the transport. (One shipped broken: it
// referenced json_each.value with json_each only in a subquery's FROM.)
func TestSchemaJoinRecipesExecute(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	for _, r := range herbmcp.SchemaJoinRecipes {
		t.Run(r.Purpose, func(t *testing.T) {
			n := len(namedParam.FindAllString(r.SQL, -1))
			positional := namedParam.ReplaceAllString(r.SQL, "?")
			params := make([]any, n)
			for i := range params {
				params[i] = ""
			}
			runSQL(t, client, positional, params)
		})
	}
}

// describe_schema's enum glossary is a contract an agent filters on: a
// value listed there that ingest never writes sends it looking for rows
// that cannot exist, and reads the empty result as absence.
func TestSchemaEnumsMatchIndexedValues(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	for _, e := range herbmcp.SchemaEnums {
		table, column, ok := strings.Cut(e.Column, ".")
		if !ok {
			t.Fatalf("malformed enum column %q", e.Column)
		}
		// symbol_reachability only ever emits reachable=1 — documented
		// as such, and there is no second value to observe.
		if e.Column == "symbol_reachability.reachable" {
			continue
		}
		t.Run(e.Column, func(t *testing.T) {
			for _, v := range distinctStrings(t, client, table, column) {
				if !slices.Contains(e.Values, v) {
					t.Errorf("%s holds %q, not in documented enum %v", e.Column, v, e.Values)
				}
			}
		})
	}
}

// The inverse of the test above, for the one column where the fixture
// is known to exercise the full vocabulary. symbols.kind is what the
// find_symbol `kind` argument documents, and it advertised a 'typedef'
// value that GCC's cgraph can never produce.
func TestSymbolKindEnumIsExhaustive(t *testing.T) {
	client := startClient(t, fixtureHBR(t))
	idx := slices.IndexFunc(herbmcp.SchemaEnums, func(e herbmcp.SchemaEnum) bool {
		return e.Column == "symbols.kind"
	})
	if idx < 0 {
		t.Fatal("symbols.kind missing from SchemaEnums")
	}
	want := herbmcp.SchemaEnums[idx].Values
	if !slices.Equal(want, []string{"function", "variable"}) {
		t.Fatalf("symbols.kind enum = %v; ingest writes only the cgraph `Type:` token, which is function or variable", want)
	}
	if got := distinctStrings(t, client, "symbols", "kind"); !slices.Equal(got, want) {
		t.Errorf("fixture symbols.kind = %v, documented enum = %v", got, want)
	}
}
