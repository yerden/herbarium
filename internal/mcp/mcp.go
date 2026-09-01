// Package mcp exposes an .hbr index over MCP (see herbarium-plan.md
// § MCP tools for the full tool contract). The server is stateless
// beyond its read-only DB handle, so it is safe to run the same server
// value across stdio and streamable-HTTP transports concurrently.
//
// Tool groups live in sibling files:
//
//   schema_tool.go             describe_schema
//   sql_tool.go                sql_query   (read-only escape hatch)
//   source_tools.go            read_source, list_source_files,
//                              verify_source, list_source_drift,
//                              search_source
//   target_tools.go            list_targets, describe_target
//   symbol_tools.go            find_symbol, describe_symbol
//   callgraph_source_tools.go  list_callers, list_callees, list_call_paths
//   callgraph_runtime_tools.go list_linked_callers, list_linked_callees,
//                              describe_inline_decisions, list_inline_sites
//   indirect_tools.go          list_indirect_call_sites,
//                              list_address_taken_functions,
//                              resolve_indirect_call, list_devirt_hints
//   linkage_tools.go           describe_link_resolution, list_weak_symbols,
//                              list_undefined_symbols, list_icf_groups,
//                              list_unreachable_symbols, list_entry_points
package mcp

import (
	"database/sql"

	"github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"
)

// ServerName is what an MCP client sees for the server side of the
// handshake. Kept short and stable so client-side allowlists (Claude
// Desktop, etc.) can key on it.
const ServerName = "herbarium"

// Options configures a new Server. Fields correspond to serve flags.
type Options struct {
	// Version is displayed in the MCP handshake. Empty → "dev".
	Version string
	// ProjectRoot enables filesystem-backed tools (verify_source live
	// hashing, list_source_drift). Empty means "index-only, no live
	// checkout access" — the intended shipping shape for shared .hbr
	// artifacts.
	ProjectRoot string
}

// Server holds the mcp-go server and the read-only DB handle every
// tool needs. Constructed once per serve invocation.
type Server struct {
	db  *sql.DB
	opts Options
	mcp *mcpsrv.MCPServer
}

// New builds a Server, registers every tool, and returns it ready to
// attach to a transport. The DB must have been opened via
// store.OpenReadOnly (mode=ro + query_only) — Server does not enforce
// this itself; the driver-level rejection of writes is the enforcement.
func New(db *sql.DB, opts Options) *Server {
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	m := mcpsrv.NewMCPServer(
		ServerName, version,
		mcpsrv.WithToolCapabilities(false),
		// Advertise empty prompt + resource capabilities so clients that
		// blind-poll `prompts/list` / `resources/list` right after
		// initialize (opencode does this) get an empty list back rather
		// than a `-32601: prompts not supported` error — some clients
		// treat that error as fatal and disconnect with
		// "MCP error -32000: Connection closed".
		mcpsrv.WithPromptCapabilities(false),
		mcpsrv.WithResourceCapabilities(false, false),
		mcpsrv.WithRecovery(),
	)
	s := &Server{db: db, opts: opts, mcp: m}
	s.registerSchemaTool()
	s.registerSQLTool()
	s.registerSourceTools()
	s.registerTargetTools()
	s.registerSymbolTools()
	s.registerCallGraphSourceTools()
	s.registerCallGraphRuntimeTools()
	s.registerIndirectTools()
	s.registerLinkageTools()
	return s
}

// MCP returns the underlying mcp-go server, so callers can attach it to
// stdio or streamable-HTTP transports.
func (s *Server) MCP() *mcpsrv.MCPServer { return s.mcp }

// newTool wraps mcp.NewTool with the annotation defaults every herbarium
// tool needs: read-only, non-destructive, closed-world (we do not touch
// external systems). Without this, mcp.NewTool leaves DestructiveHint at
// its `true` default even when the caller sets ReadOnlyHint=true — the
// resulting "read-only AND destructive" combination is contradictory
// metadata that some MCP clients (opencode among them) refuse.
func newTool(name string, opts ...mcp.ToolOption) mcp.Tool {
	defaults := []mcp.ToolOption{
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	}
	return mcp.NewTool(name, append(defaults, opts...)...)
}
