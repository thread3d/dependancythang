package mcpserver_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/thread3d/dependancythang/internal/app"
	"github.com/thread3d/dependancythang/internal/config"
	"github.com/thread3d/dependancythang/internal/mcpserver"
)

// connect wires a real MCP client to the server over an in-memory transport, so
// the protocol path is exercised without spawning a process.
func connect(t *testing.T, ctx context.Context, svc *app.Service) *mcp.ClientSession {
	t.Helper()
	server := mcpserver.New(svc)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "depmcp-test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func setup(t *testing.T) (configPath, repoPath string) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\nrepositories: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(config.EnvConfig, configPath)
	t.Setenv(config.EnvStateDir, filepath.Join(dir, "state"))

	repoPath = filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(repoPath, ".git"), 0o755); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	return configPath, repoPath
}

func TestToolSurface(t *testing.T) {
	ctx := context.Background()
	_, _ = setup(t)
	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	defer svc.Close()

	cs := connect(t, ctx, svc)
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	// The M0 surface from §7, minus the tools that belong to later milestones.
	for _, want := range []string{
		"list_repositories", "add_repository", "update_repository", "remove_repository",
		"get_config", "validate_config", "status",
	} {
		if !got[want] {
			t.Errorf("tool %q is not registered", want)
		}
	}
}

// TestToolCountMatchesRegistration keeps the number in the start-up
// announcement honest: ToolCount is printed to whoever starts the server, so a
// tool added or removed without moving it would make that line lie.
func TestToolCountMatchesRegistration(t *testing.T) {
	ctx := context.Background()
	_, _ = setup(t)
	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	defer svc.Close()

	cs := connect(t, ctx, svc)
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) == mcpserver.ToolCount {
		return
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	t.Fatalf("ToolCount says %d but the server registers %d tools: %s",
		mcpserver.ToolCount, len(res.Tools), strings.Join(names, " "))
}

// TestPromptsAreRegistered checks the four prompts from §7 are served, since a
// prompt nobody can list is a prompt nobody will use.
func TestPromptsAreRegistered(t *testing.T) {
	ctx := context.Background()
	_, _ = setup(t)
	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	defer svc.Close()
	cs := connect(t, ctx, svc)

	res, err := cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	got := map[string]bool{}
	for _, p := range res.Prompts {
		got[p.Name] = true
	}
	for _, want := range []string{
		"triage_vulnerabilities", "review_updates",
		"explain_breaking_change", "weekly_digest",
	} {
		if !got[want] {
			t.Errorf("prompt %q is not registered", want)
		}
	}

	// A prompt must render with its argument substituted.
	pr, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      "review_updates",
		Arguments: map[string]string{"repository": "/tmp/demo"},
	})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if len(pr.Messages) == 0 {
		t.Fatal("prompt returned no messages")
	}
	text := textOfPrompt(pr.Messages[0].Content)
	if !strings.Contains(text, "/tmp/demo") {
		t.Errorf("prompt did not substitute its argument:\n%s", text)
	}
}

func TestAddThenListRepository(t *testing.T) {
	ctx := context.Background()
	configPath, repoPath := setup(t)
	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	defer svc.Close()
	cs := connect(t, ctx, svc)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "add_repository",
		Arguments: map[string]any{"path": repoPath, "forge": "github"},
	})
	if err != nil {
		t.Fatalf("add_repository: %v", err)
	}
	if res.IsError {
		t.Fatalf("add_repository returned isError: %s", textOf(res))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_repositories"})
	if err != nil {
		t.Fatalf("list_repositories: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_repositories returned isError: %s", textOf(res))
	}
	body := textOf(res)
	if !strings.Contains(body, repoPath) {
		t.Errorf("list output does not mention %s:\n%s", repoPath, body)
	}

	// And the authoritative file must hold it.
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(written), repoPath) {
		t.Errorf("config file does not contain the repository:\n%s", written)
	}
}

func TestAddInvalidRepositoryReturnsError(t *testing.T) {
	ctx := context.Background()
	_, _ = setup(t)
	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	defer svc.Close()
	cs := connect(t, ctx, svc)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "add_repository",
		Arguments: map[string]any{"path": t.TempDir()}, // not a git repo
	})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an isError result for a non-git directory")
	}
	if !strings.Contains(textOf(res), app.CodeNotAGitRepo) {
		t.Errorf("error text lacks the stable code %s: %s", app.CodeNotAGitRepo, textOf(res))
	}
}

func TestValidateConfigReportsLineNumbers(t *testing.T) {
	ctx := context.Background()
	_, _ = setup(t)
	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	defer svc.Close()
	cs := connect(t, ctx, svc)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "validate_config",
		Arguments: map[string]any{
			"yaml": "version: 1\ndefaults:\n  max_open_branch: 5\n",
		},
	})
	if err != nil {
		t.Fatalf("validate_config: %v", err)
	}
	if res.IsError {
		t.Fatalf("validate_config returned isError: %s", textOf(res))
	}
	body := textOf(res)
	// structuredContent carries a numeric line; assert on that rather than prose.
	if !strings.Contains(body, `"line":3`) && !strings.Contains(body, "line 3") {
		t.Errorf("expected a line number for the unknown key, got:\n%s", body)
	}
}

func TestStatusReportsStateDir(t *testing.T) {
	ctx := context.Background()
	_, _ = setup(t)
	svc, err := app.Open(ctx)
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	defer svc.Close()
	cs := connect(t, ctx, svc)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "status"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if res.IsError {
		t.Fatalf("status returned isError: %s", textOf(res))
	}
	if !strings.Contains(textOf(res), svc.StateDir()) {
		t.Errorf("status output does not mention the state dir:\n%s", textOf(res))
	}
}

// textOfPrompt renders a prompt content block.
func textOfPrompt(c any) string {
	if tc, ok := c.(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// textOf flattens a tool result to text: structured content when present,
// otherwise the text blocks.
func textOf(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	if res.StructuredContent != nil {
		b.WriteString(jsonOf(res.StructuredContent))
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func jsonOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}
