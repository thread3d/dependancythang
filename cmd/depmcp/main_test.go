package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/thread3d/dependancythang/internal/mcpserver"
	"github.com/thread3d/dependancythang/internal/version"
)

// The start-up line is the only thing depmcp says about itself in the stream it
// shares with dsh's own output and the other MCP servers' banners, so it has to
// be findable by its tag, stay on one line, and state the three things a reader
// decides on: which build, how many tools, and whether writes are possible.
func TestStartupLineAnnouncesCapability(t *testing.T) {
	const stateDir = "/home/someone/.local/share/depmcp"

	want := "[dependancythang] MCP server ready — depmcp " + version.Version +
		", " + strconv.Itoa(mcpserver.ToolCount) + " tools, writes enabled (state " + stateDir + ")"
	if got := startupLine(stateDir, false); got != want {
		t.Fatalf("start-up line\n got: %q\nwant: %q", got, want)
	}

	writable := startupLine(stateDir, false)
	if !strings.Contains(writable, "writes enabled") {
		t.Fatalf("a writable server must say so: %q", writable)
	}

	readOnly := startupLine(stateDir, true)
	if !strings.Contains(readOnly, "read-only") || strings.Contains(readOnly, "writes enabled") {
		t.Fatalf("a read-only server must not claim writes: %q", readOnly)
	}
	if strings.ContainsAny(readOnly, "\r\n") {
		t.Fatalf("start-up line must stay on one line: %q", readOnly)
	}
	if !strings.HasPrefix(readOnly, logTag+" MCP server ready — ") {
		t.Fatalf("start-up line does not lead with %q: %q", logTag, readOnly)
	}
}
