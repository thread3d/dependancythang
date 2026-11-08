package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/thread3d/dependancythang/internal/app"
	"github.com/thread3d/dependancythang/internal/config"
	"github.com/thread3d/dependancythang/internal/faults"
)

// TestPanicIsRecordedAndReturned is DEP-A569750B: a panic inside a tool handler used
// to escape into the transport, where the client saw a broken call and the defect
// left no trace.
func TestPanicIsRecordedAndReturned(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath,
		[]byte("version: 1\nrepositories:\n  - path: "+repo+"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(config.EnvConfig, configPath)
	t.Setenv(config.EnvStateDir, filepath.Join(dir, "state"))

	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	defer svc.Close()

	server := mcp.NewServer(&mcp.Implementation{Name: "panic-probe", Version: "0"}, nil)
	type in struct {
		Repository string `json:"repository"`
	}
	addTool(server, svc, &mcp.Tool{Name: "boom", Description: "panics on purpose"},
		func(context.Context, *mcp.CallToolRequest, in) (*mcp.CallToolResult, struct{}, error) {
			var m map[string]string
			m["write to nil map"] = "panics" //nolint:staticcheck // the panic is the point
			return nil, struct{}{}, nil
		})

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "boom",
		Arguments: map[string]any{"repository": repo},
	})
	if err != nil {
		t.Fatalf("CallTool returned a transport error instead of an isError result: %v", err)
	}
	if !res.IsError {
		t.Fatal("a panic did not produce an isError result")
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !strings.Contains(text.String(), app.CodeInternal) {
		t.Errorf("error text lacks %s: %s", app.CodeInternal, text.String())
	}

	// And the defect must be on file for a later session.
	journal, err := faults.Open(faults.Dir(repo))
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	entries, err := journal.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("faults = %d, want 1", len(entries))
	}
	if entries[0].Kind != faults.KindInternal {
		t.Errorf("kind = %q, want %q", entries[0].Kind, faults.KindInternal)
	}
	if entries[0].Stack == "" {
		t.Error("the stack was not captured, so the hint cannot be acted on")
	}
}
