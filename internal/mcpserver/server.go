// Package mcpserver exposes depmcp over the Model Context Protocol.
//
// M0 ships the configuration-authority surface only: list, add, update and
// remove repositories, plus config inspection, validation, and status. Nothing
// here plans or applies updates yet; see section 14 of the specification for the
// milestone that adds those.
package mcpserver

import (
	"context"
	"fmt"
	"reflect"
	"runtime/debug"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/thread3d/dependancythang/internal/app"
	"github.com/thread3d/dependancythang/internal/config"
	"github.com/thread3d/dependancythang/internal/faults"
	"github.com/thread3d/dependancythang/internal/version"
)

// ServerName is reported in the initialize handshake.
const ServerName = "depmcp"

// ToolCount is how many tools New registers. The start-up announcement reports
// it, so it is what a user reads when deciding whether the server offers what
// they came for; TestToolCountMatchesRegistration fails if a tool is added or
// removed without this number moving with it.
const ToolCount = 22

// New builds the MCP server with every M0 tool registered.
func New(svc *app.Service) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Version: version.Version,
	}, nil)

	registerRepositoryTools(s, svc)
	registerConfigTools(s, svc)
	registerStatusTool(s, svc)
	registerReadTools(s, svc)
	registerWriteTools(s, svc)
	registerForgeTools(s, svc)
	registerFaultTools(s, svc)
	registerPrompts(s)
	return s
}

// ---- repository tools ---------------------------------------------------

type listRepositoriesOut struct {
	Repositories []app.RepoView `json:"repositories"`
	Total        int            `json:"total"`
	Truncated    bool           `json:"truncated"`
}

func registerRepositoryTools(s *mcp.Server, svc *app.Service) {
	addTool(s, svc, &mcp.Tool{
		Name:        "list_repositories",
		Description: "List tracked repositories with their forge, remote, and last scan time.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listRepositoriesOut, error) {
		repos, err := svc.ListRepositories(ctx)
		if err != nil {
			return nil, listRepositoriesOut{}, wrap(svc, "list_repositories", nil, err)
		}
		_ = svc.RecordTool(ctx, "list_repositories", map[string]any{"count": len(repos)}, nil, "")
		return nil, listRepositoriesOut{Repositories: repos, Total: len(repos)}, nil
	})

	type addRepositoryIn struct {
		Path   string `json:"path" jsonschema:"absolute path (or ~/...) to a git repository to track"`
		Forge  string `json:"forge,omitempty" jsonschema:"local (default), github, gitlab, gitea"`
		Remote string `json:"remote,omitempty" jsonschema:"git remote name; defaults to the global fetch.remote"`
	}
	type addRepositoryOut struct {
		Repository app.RepoView `json:"repository"`
	}
	addTool(s, svc, &mcp.Tool{
		Name:        "add_repository",
		Description: "Track a git repository by writing it to the configuration file. Re-adding an identical entry changes nothing.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in addRepositoryIn) (*mcp.CallToolResult, addRepositoryOut, error) {
		view, err := svc.AddRepository(ctx, app.AddRepositoryRequest{
			Path: in.Path, Forge: in.Forge, Remote: in.Remote,
		})
		if err != nil {
			return nil, addRepositoryOut{}, wrap(svc, "add_repository", map[string]any{"path": in.Path}, err)
		}
		_ = svc.RecordTool(ctx, "add_repository", map[string]any{"path": in.Path}, nil, "")
		return nil, addRepositoryOut{Repository: view}, nil
	})

	type updateRepositoryIn struct {
		Path   string `json:"path" jsonschema:"path of a repository that is already tracked"`
		Forge  string `json:"forge,omitempty" jsonschema:"local, github, gitlab, gitea"`
		Remote string `json:"remote,omitempty" jsonschema:"git remote name"`
	}
	type updateRepositoryOut struct {
		Repository app.RepoView `json:"repository"`
	}
	addTool(s, svc, &mcp.Tool{
		Name:        "update_repository",
		Description: "Change a tracked repository's forge or remote in the configuration file.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in updateRepositoryIn) (*mcp.CallToolResult, updateRepositoryOut, error) {
		if _, ok := svc.Cfg.Find(in.Path); !ok {
			err := fmt.Errorf("repository %q is not tracked; use add_repository first", in.Path)
			return nil, updateRepositoryOut{}, wrap(svc, "update_repository", map[string]any{"path": in.Path}, err)
		}
		view, err := svc.AddRepository(ctx, app.AddRepositoryRequest{
			Path: in.Path, Forge: in.Forge, Remote: in.Remote,
		})
		if err != nil {
			return nil, updateRepositoryOut{}, wrap(svc, "update_repository", map[string]any{"path": in.Path}, err)
		}
		_ = svc.RecordTool(ctx, "update_repository", map[string]any{"path": in.Path}, nil, "")
		return nil, updateRepositoryOut{Repository: view}, nil
	})

	type removeRepositoryIn struct {
		Path string `json:"path" jsonschema:"path of the repository to stop tracking"`
	}
	type removeRepositoryOut struct {
		Removed bool   `json:"removed"`
		Path    string `json:"path"`
	}
	addTool(s, svc, &mcp.Tool{
		Name:        "remove_repository",
		Description: "Stop tracking a repository by editing the configuration file. Never deletes files, branches, or history.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in removeRepositoryIn) (*mcp.CallToolResult, removeRepositoryOut, error) {
		changed, err := svc.RemoveRepository(ctx, in.Path)
		if err != nil {
			return nil, removeRepositoryOut{}, wrap(svc, "remove_repository", map[string]any{"path": in.Path}, err)
		}
		_ = svc.RecordTool(ctx, "remove_repository", map[string]any{"path": in.Path, "changed": changed}, nil, "")
		return nil, removeRepositoryOut{Removed: changed, Path: in.Path}, nil
	})
}

// ---- configuration tools ------------------------------------------------

type effectiveConfigOut struct {
	ConfigPath string        `json:"config_path"`
	StateDir   string        `json:"state_dir"`
	ReadOnly   bool          `json:"read_only"`
	Layers     []string      `json:"layers"`
	Config     config.Config `json:"config"`
}

func registerConfigTools(s *mcp.Server, svc *app.Service) {
	addTool(s, svc, &mcp.Tool{
		Name:        "get_config",
		Description: "Show the effective merged configuration and which layers produced it.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, effectiveConfigOut, error) {
		stateDir, err := svc.Cfg.StateDir()
		if err != nil {
			return nil, effectiveConfigOut{}, wrap(svc, "get_config", nil, err)
		}
		_ = svc.RecordTool(ctx, "get_config", nil, nil, "")
		return nil, effectiveConfigOut{
			ConfigPath: svc.ConfigPath,
			StateDir:   stateDir,
			ReadOnly:   svc.ReadOnly(),
			Layers:     svc.Sources.Layers,
			Config:     *svc.Cfg,
		}, nil
	})

	type validateConfigIn struct {
		YAML string `json:"yaml" jsonschema:"candidate configuration document to validate; nothing is saved"`
	}
	type validateConfigOut struct {
		Valid  bool           `json:"valid"`
		Issues []config.Issue `json:"issues,omitempty"`
	}
	addTool(s, svc, &mcp.Tool{
		Name:        "validate_config",
		Description: "Validate a candidate configuration without saving it. Reports unknown keys with line numbers.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in validateConfigIn) (*mcp.CallToolResult, validateConfigOut, error) {
		issues := config.Validate([]byte(in.YAML))
		_ = svc.RecordTool(ctx, "validate_config", map[string]any{"bytes": len(in.YAML)}, nil, "")
		return nil, validateConfigOut{Valid: len(issues) == 0, Issues: issues}, nil
	})
}

// ---- status -------------------------------------------------------------

type statusOut struct {
	StateDir   string       `json:"state_dir"`
	ConfigPath string       `json:"config_path"`
	ReadOnly   bool         `json:"read_only"`
	Stats      storeStats   `json:"stats"`
	Locks      []lockHolder `json:"held_locks,omitempty"`
}

type storeStats struct {
	Repos        int   `json:"repos"`
	Orphans      int   `json:"orphans"`
	AuditEvents  int   `json:"audit_events"`
	DatabaseSize int64 `json:"database_bytes"`
}

type lockHolder struct {
	RepoID string `json:"repo_id"`
	PID    int    `json:"pid"`
}

func registerStatusTool(s *mcp.Server, svc *app.Service) {
	addTool(s, svc, &mcp.Tool{
		Name:        "status",
		Description: "Report state directory, database statistics, and any repository locks currently held.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, statusOut, error) {
		stats, err := svc.Store.Stats(ctx)
		if err != nil {
			return nil, statusOut{}, wrap(svc, "status", nil, err)
		}
		out := statusOut{
			StateDir:   svc.StateDir(),
			ConfigPath: svc.ConfigPath,
			ReadOnly:   svc.ReadOnly(),
			Stats: storeStats{
				Repos: stats.Repos, Orphans: stats.Orphans,
				AuditEvents: stats.Audit, DatabaseSize: stats.Bytes,
			},
		}
		for _, h := range svc.HeldLocks() {
			out.Locks = append(out.Locks, lockHolder{RepoID: h.RepoID, PID: h.PID})
		}
		_ = svc.RecordTool(ctx, "status", nil, nil, "")
		return nil, out, nil
	})
}

// ---- helpers ------------------------------------------------------------

// addTool registers a tool behind panic recovery.
//
// A panic that escapes a handler is the one failure the transport cannot describe:
// the client sees a broken call with no explanation, and the defect leaves no trace
// unless it is caught here. It is recorded as an internal fault and returned as an
// ordinary isError result.
func addTool[In, Out any](s *mcp.Server, svc *app.Service, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out Out, err error) {
		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				_, _ = svc.RecordFault(ctx, repositoryOf(in), faults.Fault{
					Kind:    faults.KindInternal,
					Summary: fmt.Sprintf("panic in %s: %v", t.Name, r),
					Detail:  fmt.Sprintf("panic: %v", r),
					Stack:   stack,
					Command: t.Name,
					Hint: "The stack in issues.jsonl names the frame. Fix the nil dereference or " +
						"impossible state, and add a regression test that reaches this handler with " +
						"the same input.",
				})
				err = fmt.Errorf("%s: internal panic in %s: %v", app.CodeInternal, t.Name, r)
				var zero Out
				out = zero
				res = nil
			}
		}()
		return h(ctx, req, in)
	})
}

// repositoryOf reads a "repository" argument out of a typed tool input, so a fault
// lands in the right checkout's journal. It is reflection because the handlers are
// generic; a missing field simply yields "".
func repositoryOf(v any) string {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Struct {
		return ""
	}
	if f := rv.FieldByName("Repository"); f.IsValid() && f.Kind() == reflect.String {
		return f.String()
	}
	return ""
}

func ptr[T any](v T) *T { return &v }

// wrap records the failure and returns an error carrying the stable code, which
// the SDK renders as an isError tool result (§7).
//
// It is also where depmcp notices its own defects: an error with no stable code
// means the code took a path nobody planned for, so it goes into the repository's
// fault backlog for a later session to fix.
func wrap(svc *app.Service, tool string, args map[string]any, err error) error {
	if err == nil {
		return nil
	}
	code := app.CodeOf(err)
	_ = svc.RecordTool(context.Background(), tool, args, err, code)
	if code == "" {
		repo, _ := args["repository"].(string)
		svc.RecordUnexpectedError(context.Background(), tool, repo, err)
	}
	if code == "" {
		return err
	}
	return fmt.Errorf("%s: %w", code, err)
}
