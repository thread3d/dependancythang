package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/thread3d/dependancythang/internal/app"
	"github.com/thread3d/dependancythang/internal/planner"
	"github.com/thread3d/dependancythang/internal/store"
	"github.com/thread3d/dependancythang/internal/verify"
)

// registerWriteTools adds the M2 write path: apply a plan, revert a run, and
// inspect run history.
//
// Confirmation is the control, not the annotations (§7 annotation note). The
// summary shown to the user is computed from the stored plan, so neither a model
// nor a changelog can author the text somebody approves.
func registerWriteTools(s *mcp.Server, svc *app.Service) {
	type applyIn struct {
		PlanID  string `json:"plan_id" jsonschema:"plan id returned by plan_updates"`
		Confirm bool   `json:"confirm,omitempty" jsonschema:"set true only when the client cannot show a confirmation prompt"`
	}
	type runOut struct {
		Run store.Run `json:"run"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "apply_plan",
		Description: "Create a worktree and branch from the default branch, apply the plan there, and commit. " +
			"Never touches your working tree. Re-validates the plan against the registry first.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in applyIn) (*mcp.CallToolResult, runOut, error) {
		confirmed := in.Confirm
		if !confirmed {
			ok, err := confirm(ctx, req, svc, in.PlanID)
			if err != nil {
				return nil, runOut{}, err
			}
			confirmed = ok
		}
		run, err := svc.ApplyPlan(ctx, in.PlanID, confirmed)
		if err != nil {
			return nil, runOut{}, wrap(svc, "apply_plan",
				map[string]any{"plan_id": in.PlanID, "confirm": in.Confirm}, err)
		}
		_ = svc.RecordTool(ctx, "apply_plan",
			map[string]any{"plan_id": in.PlanID, "run_id": run.RunID, "branch": run.Branch}, nil, "")
		return nil, runOut{Run: run}, nil
	})

	type verifyIn struct {
		RunID string `json:"run_id" jsonschema:"run id returned by apply_plan"`
	}
	type verifyOut struct {
		Run    store.Run     `json:"run"`
		Result verify.Result `json:"result"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "verify_run",
		Description: "Build and test a run's branch inside the sandbox, with no network and no credentials. " +
			"A failing verification is what blocks a bad update.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(true), IdempotentHint: false, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in verifyIn) (*mcp.CallToolResult, verifyOut, error) {
		run, result, err := svc.VerifyRun(ctx, in.RunID)
		if err != nil {
			return nil, verifyOut{Run: run, Result: result}, wrap(svc, "verify_run",
				map[string]any{"run_id": in.RunID}, err)
		}
		_ = svc.RecordTool(ctx, "verify_run",
			map[string]any{"run_id": in.RunID, "summary": result.Summary}, nil, "")
		return nil, verifyOut{Run: run, Result: result}, nil
	})

	type revertIn struct {
		RunID string `json:"run_id" jsonschema:"run id returned by apply_plan"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "revert_run",
		Description: "Delete a run's branch and worktree. A merged or closed run is terminal and is only reported.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in revertIn) (*mcp.CallToolResult, runOut, error) {
		run, err := svc.RevertRun(ctx, in.RunID)
		if err != nil {
			return nil, runOut{Run: run}, wrap(svc, "revert_run", map[string]any{"run_id": in.RunID}, err)
		}
		_ = svc.RecordTool(ctx, "revert_run", map[string]any{"run_id": in.RunID}, nil, "")
		return nil, runOut{Run: run}, nil
	})

	type listRunsIn struct {
		Repository string `json:"repository,omitempty" jsonschema:"optional repository id or path"`
		Limit      int    `json:"limit,omitempty" jsonschema:"maximum runs to return (default 20)"`
	}
	type listRunsOut struct {
		Runs  []store.Run `json:"runs"`
		Total int         `json:"total"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_runs",
		Description: "Recent apply runs with their branches, worktrees, and status.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listRunsIn) (*mcp.CallToolResult, listRunsOut, error) {
		runs, err := svc.ListRuns(ctx, in.Repository, in.Limit)
		if err != nil {
			return nil, listRunsOut{}, wrap(svc, "list_runs", map[string]any{"repository": in.Repository}, err)
		}
		_ = svc.RecordTool(ctx, "list_runs", map[string]any{"count": len(runs)}, nil, "")
		return nil, listRunsOut{Runs: runs, Total: len(runs)}, nil
	})

	type getRunIn struct {
		RunID string `json:"run_id" jsonschema:"run id"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_run",
		Description: "One run record, including its branch, worktree, commit, and status.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getRunIn) (*mcp.CallToolResult, runOut, error) {
		run, err := svc.GetRun(ctx, in.RunID)
		if err != nil {
			return nil, runOut{}, wrap(svc, "get_run", map[string]any{"run_id": in.RunID}, err)
		}
		_ = svc.RecordTool(ctx, "get_run", map[string]any{"run_id": in.RunID}, nil, "")
		return nil, runOut{Run: run}, nil
	})
}

// summarize renders the plan's changes for the confirmation prompt. It reads
// only stored plan fields, so no untrusted text can appear here.
func summarize(p planner.Plan) string {
	var b strings.Builder
	for i, g := range p.Groups {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s:", g.Name)
		for j, c := range g.Changes {
			if j > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, " %s %s->%s", c.Dependency, c.From, c.To)
		}
	}
	return b.String()
}

// confirm asks the user through MCP elicitation, and reports whether they said
// yes. It never guesses: a client without elicitation gets an error naming the
// fallback, and a decline is a decline.
func confirm(ctx context.Context, req *mcp.CallToolRequest, svc *app.Service, planID string) (bool, error) {
	if req.Session == nil {
		return false, fmt.Errorf("%s: no session to confirm with; pass confirm=true",
			app.CodeConfirmationRequired)
	}
	plan, err := svc.GetPlan(ctx, planID)
	if err != nil {
		return false, err
	}
	// Computed from the plan, never authored by a model.
	message := fmt.Sprintf("Apply %d dependency update(s) on a new branch? %s",
		plan.TotalChanges, summarize(plan))
	res, err := req.Session.Elicit(ctx, &mcp.ElicitParams{
		Message: message,
		RequestedSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"proceed": {Type: "boolean", Description: "apply the plan on a new branch"},
			},
			Required: []string{"proceed"},
		},
	})
	if err != nil {
		// The SDK refuses outright unless the client advertised the capability,
		// which is the common case in scripts and many CLI clients.
		return false, fmt.Errorf("%s: client did not confirm (%v); pass confirm=true to proceed",
			app.CodeConfirmationRequired, err)
	}
	if res.Action != "accept" {
		return false, fmt.Errorf("%s: user chose %q", app.CodeConfirmationRequired, res.Action)
	}
	if proceed, ok := res.Content["proceed"].(bool); ok && !proceed {
		return false, fmt.Errorf("%s: user declined", app.CodeConfirmationRequired)
	}
	return true, nil
}
