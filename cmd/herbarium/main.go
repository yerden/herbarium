// Command herbarium indexes a Meson C build directory and serves the
// result over MCP. See herbarium-plan.md for the full contract.
package main

import (
	"fmt"
	"os"
)

// Version is stamped by the build if built with -ldflags. Empty in
// go-test runs; that is fine — meta.herbarium_version just records "dev".
var Version = "dev"

const usage = `herbarium — a GCC-native C code index for AI agents

Usage:
  herbarium collect --builddir DIR --project-root DIR --out FILE [--strict] [--target NAME,NAME]
  herbarium serve   --hbr FILE [--project-root DIR] [--transport stdio|http] [--http-addr ADDR]
  herbarium version
  herbarium help

Subcommands:
  collect    Ingest a Meson builddir into an .hbr index.
  serve      Serve an .hbr index over MCP (stdio by default; --transport http
             switches to the streamable-HTTP transport).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "collect":
		os.Exit(runCollect(os.Args[2:]))
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println(Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
