//go:build integration

// Package mcp integration tests.
//
// These tests connect to a real, public, open-source MCP server
// (@modelcontextprotocol/server-filesystem) over stdio to exercise the
// full MCPManager lifecycle: Connect -> initialize -> tools/list ->
// tools/call. They require network access (to download the server via
// npx) and a working Node.js toolchain, so they are gated behind the
// `integration` build tag and skipped automatically when `npx` is
// unavailable.
//
// Run with:
//
//	go test -tags integration ./internal/mcp/ -run TestFilesystemMCP -v
package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"go-code-agent/internal/llm"
)

// writeFileForTest writes content to path (used for test fixtures).
func writeFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// quoteJSON returns a JSON-quoted string literal (without surrounding
// object braces) for embedding into a hand-built JSON argument blob.
func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// requireNpx skips the test if the `npx` binary is not on PATH.
func requireNpx(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not found on PATH; skipping integration test")
	}
}

// startFilesystemServer connects a filesystem MCP server rooted at dir
// and returns the manager (already connected + tools discovered).
func startFilesystemServer(t *testing.T, dir string) *MCPManager {
	t.Helper()
	requireNpx(t)

	mgr := NewMCPManager(dir)
	// npx cold-downloads the server on first run; give it a generous budget
	// via the test context. Connect is synchronous (initialize + tools/list).
	const serverName = "fs"
	err := mgr.Connect(serverName, "npx",
		[]string{"--yes", "@modelcontextprotocol/server-filesystem", dir},
		nil)
	if err != nil {
		mgr.DisconnectAll()
		t.Fatalf("Connect to filesystem MCP server failed: %v", err)
	}
	t.Cleanup(mgr.DisconnectAll)
	return mgr
}

// connectServer launches a named MCP server via `npx --yes <pkg>` (or any
// command) and returns the connected manager. workdir is irrelevant for a
// direct Connect, so a throwaway temp dir is used.
func connectServer(t *testing.T, name, cmd string, args []string) *MCPManager {
	t.Helper()
	requireNpx(t)
	mgr := NewMCPManager(t.TempDir())
	if err := mgr.Connect(name, cmd, args, nil); err != nil {
		mgr.DisconnectAll()
		t.Fatalf("Connect(%s) failed: %v", name, err)
	}
	t.Cleanup(mgr.DisconnectAll)
	return mgr
}

// toolNames is a small helper for diagnostics.
func toolNames(defs []llm.ToolDef) []string {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	return names
}

// TestPublicMCP_Filesystem connects to a real, public, open-source MCP
// server (@modelcontextprotocol/server-filesystem, pulled via npx) rooted
// at an actual external directory and verifies a live tools/call round-trip.
// This is the canonical "connect a public MCP server" smoke test.
//
// Security note: do NOT substitute the npm package named `mcp-server-fetch`
// here. That name is a typosquat/canary trap that exfiltrates host metadata
// on launch (it phones home to an external endpoint at startup, which our
// tool-output secrets scanner cannot catch because it happens inside the
// server process, not via a tool result). Always use the official
// @modelcontextprotocol/* scoped packages.
func TestPublicMCP_Filesystem(t *testing.T) {
	// Use a real external directory (not t.TempDir) to prove we can point
	// the public server at an arbitrary location and read from it.
	extDir, err := os.MkdirTemp("", "mcp-public-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(extDir)
	probe := extDir + "/probe.txt"
	if err := writeFileForTest(probe, "public-mcp-probe\n"); err != nil {
		t.Fatalf("write probe: %v", err)
	}

	mgr := connectServer(t, "fs", "npx",
		[]string{"--yes", "@modelcontextprotocol/server-filesystem", extDir})

	res := mgr.Call("mcp__fs__read_file",
		[]byte(`{"path":`+quoteJSON(probe)+`}`))
	if !res.OK {
		t.Fatalf("read_file failed: %s", res.Output)
	}
	if !strings.Contains(res.Output, "public-mcp-probe") {
		t.Errorf("read_file output = %q, want it to contain probe content", res.Output)
	}
}

// TestFilesystemMCP_ConnectAndDiscover verifies the happy path: a public
// MCP server can be launched, initialized, and its tools discovered and
// surfaced under the mcp__<server>__<tool> naming convention.
func TestFilesystemMCP_ConnectAndDiscover(t *testing.T) {
	dir := t.TempDir()
	mgr := startFilesystemServer(t, dir)

	if got := mgr.ServerCount(); got != 1 {
		t.Fatalf("ServerCount = %d, want 1", got)
	}
	if got := mgr.ToolCount(); got == 0 {
		t.Fatal("ToolCount = 0, want > 0")
	}

	defs := mgr.ToolDefs()
	if len(defs) == 0 {
		t.Fatal("ToolDefs returned no tools")
	}
	hasReadFile := false
	allPrefixed := true
	for _, d := range defs {
		if !strings.HasPrefix(d.Name, "mcp__fs__") {
			allPrefixed = false
			t.Logf("unexpected tool name: %s", d.Name)
		}
		if d.Name == "mcp__fs__read_file" {
			hasReadFile = true
		}
		if d.Parameters == nil {
			t.Errorf("tool %s has nil Parameters", d.Name)
		}
	}
	if !allPrefixed {
		t.Error("some tool names are not prefixed with mcp__fs__")
	}
	if !hasReadFile {
		t.Fatal("expected mcp__fs__read_file to be discovered")
	}
	if !mgr.IsMCPTool("mcp__fs__read_file") {
		t.Error("IsMCPTool(mcp__fs__read_file) = false, want true")
	}
	if mgr.IsMCPTool("bash") {
		t.Error("IsMCPTool(bash) = true, want false")
	}
}

// TestFilesystemMCP_ReadOnlyCall exercises an actual tools/call round-trip
// against the live server using only read-only operations.
func TestFilesystemMCP_ReadOnlyCall(t *testing.T) {
	dir := t.TempDir()
	content := "hello-from-mcp-integration-test\n"
	helloPath := dir + "/hello.txt"
	if err := writeFileForTest(helloPath, content); err != nil {
		t.Fatalf("setup: write test file: %v", err)
	}

	mgr := startFilesystemServer(t, dir)

	// read_file should return the file contents we wrote.
	res := mgr.Call("mcp__fs__read_file",
		[]byte(`{"path":`+quoteJSON(helloPath)+`}`))
	if !res.OK {
		t.Fatalf("read_file failed: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hello-from-mcp-integration-test") {
		t.Errorf("read_file content = %q, want it to contain the written text", res.Output)
	}

	// list_directory should include our hello.txt entry.
	lres := mgr.Call("mcp__fs__list_directory",
		[]byte(`{"path":`+quoteJSON(dir)+`}`))
	if !lres.OK {
		t.Fatalf("list_directory failed: %s", lres.Output)
	}
	if !strings.Contains(lres.Output, "hello.txt") {
		t.Errorf("list_directory content = %q, want it to list hello.txt", lres.Output)
	}
}

// TestFilesystemMCP_ListAllSmoke simply ensures ListAll produces output
// without panicking (sanity for the REPL /mcp command path).
func TestFilesystemMCP_ListAllSmoke(t *testing.T) {
	dir := t.TempDir()
	mgr := startFilesystemServer(t, dir)
	out := mgr.ListAll()
	if !strings.Contains(out, "fs") {
		t.Errorf("ListAll = %q, want it to mention server 'fs'", out)
	}
}
