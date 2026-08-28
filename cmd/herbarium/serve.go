package main

import (
	"flag"
	"fmt"
	"os"

	mcpsrv "github.com/mark3labs/mcp-go/server"

	herbmcp "github.com/yerden/herbarium/internal/mcp"
	"github.com/yerden/herbarium/internal/store"
)

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var (
		hbr       = fs.String("hbr", "", ".hbr file to serve (required)")
		proot     = fs.String("project-root", "", "project source root (optional; enables live-file drift tools)")
		transport = fs.String("transport", "stdio", "MCP transport: 'stdio' or 'http'")
		httpAddr  = fs.String("http-addr", ":7473", "address for the streamable-HTTP transport (used only with --transport=http)")
		checkOnly = fs.Bool("check", false, "open the .hbr, register tools, and exit without serving (used by tests)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *hbr == "" {
		fmt.Fprintln(os.Stderr, "serve: --hbr is required")
		fs.Usage()
		return 2
	}

	db, err := store.OpenReadOnly(*hbr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer db.Close()

	var ver string
	if err := db.QueryRow(
		`SELECT value FROM meta WHERE key='schema_version'`,
	).Scan(&ver); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %s does not appear to be a herbarium index: %v\n", *hbr, err)
		return 1
	}
	if ver != store.SchemaVersion {
		fmt.Fprintf(os.Stderr, "serve: schema_version %q in %s does not match supported %q\n", ver, *hbr, store.SchemaVersion)
		return 1
	}

	srv := herbmcp.New(db, herbmcp.Options{
		Version:     Version,
		ProjectRoot: *proot,
	})

	if *checkOnly {
		fmt.Printf("herbarium serve --check: %s opens (schema %s)\n", *hbr, ver)
		return 0
	}

	switch *transport {
	case "stdio":
		// stderr is safe for banners on the stdio transport; stdout is
		// reserved for the JSON-RPC framing.
		fmt.Fprintf(os.Stderr, "herbarium serve: %s over stdio (schema %s)\n", *hbr, ver)
		if err := mcpsrv.ServeStdio(srv.MCP()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case "http":
		fmt.Fprintf(os.Stderr, "herbarium serve: %s over streamable HTTP on %s (schema %s)\n", *hbr, *httpAddr, ver)
		http := mcpsrv.NewStreamableHTTPServer(srv.MCP())
		if err := http.Start(*httpAddr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	default:
		fmt.Fprintf(os.Stderr, "serve: unknown transport %q (want 'stdio' or 'http')\n", *transport)
		return 2
	}
	return 0
}
