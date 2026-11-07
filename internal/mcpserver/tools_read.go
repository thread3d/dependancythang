package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/thread3d/dependancythang/internal/app"
	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/planner"
)

// registerReadTools adds the M1 read path: inventory, candidates, advisories,
// and plans. None of these write to a repository; plan_updates stores a plan
// record, which is derived state.
func registerReadTools(s *mcp.Server, svc *app.Service) {
	type repoIn struct {
		Repository string `json:"repository" jsonschema:"repository id or absolute path"`
	}

	type scanOut struct {
		Repository   app.RepoTarget         `json:"repository"`
		Total        int                    `json:"total"`
		Direct       int                    `json:"direct"`
		Dependencies []ecosystem.Dependency `json:"dependencies"`
		Truncated    bool                   `json:"truncated"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "scan_dependencies",
		Description: "Inventory a tracked repository's Go module dependencies, direct and indirect.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in repoIn) (*mcp.CallToolResult, scanOut, error) {
		target, deps, err := svc.ScanDependencies(ctx, in.Repository)
		if err != nil {
			return nil, scanOut{}, wrap(svc, "scan_dependencies", map[string]any{"repository": in.Repository}, err)
		}
		out := scanOut{Repository: target, Total: len(deps), Dependencies: deps}
		for _, d := range deps {
			if d.Direct {
				out.Direct++
			}
		}
		_ = svc.RecordTool(ctx, "scan_dependencies", map[string]any{"repository": in.Repository, "total": out.Total}, nil, "")
		return nil, out, nil
	})

	type checkUpdatesIn struct {
		Repository      string `json:"repository" jsonschema:"repository id or absolute path"`
		IncludeIndirect bool   `json:"include_indirect,omitempty" jsonschema:"also resolve transitive dependencies (slower)"`
		Limit           int    `json:"limit,omitempty" jsonschema:"maximum candidates to resolve"`
	}
	type checkUpdatesOut struct {
		Repository app.RepoTarget  `json:"repository"`
		Candidates []app.Candidate `json:"candidates"`
		Total      int             `json:"total"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "check_updates",
		Description: "Report newer versions available for a repository's dependencies, with the update class and publish time.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in checkUpdatesIn) (*mcp.CallToolResult, checkUpdatesOut, error) {
		target, cands, err := svc.CheckUpdates(ctx, in.Repository, in.IncludeIndirect, in.Limit)
		if err != nil {
			return nil, checkUpdatesOut{}, wrap(svc, "check_updates", map[string]any{"repository": in.Repository}, err)
		}
		_ = svc.RecordTool(ctx, "check_updates", map[string]any{"repository": in.Repository, "count": len(cands)}, nil, "")
		return nil, checkUpdatesOut{Repository: target, Candidates: cands, Total: len(cands)}, nil
	})

	type vulnOut struct {
		Repository      app.RepoTarget      `json:"repository"`
		Vulnerabilities []app.Vulnerability `json:"vulnerabilities"`
		Total           int                 `json:"total"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "check_vulnerabilities",
		Description: "Query OSV for advisories affecting the versions currently in use. Always refreshes rather than trusting the cache.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in repoIn) (*mcp.CallToolResult, vulnOut, error) {
		target, vulns, err := svc.CheckVulnerabilities(ctx, in.Repository)
		if err != nil {
			return nil, vulnOut{}, wrap(svc, "check_vulnerabilities", map[string]any{"repository": in.Repository}, err)
		}
		_ = svc.RecordTool(ctx, "check_vulnerabilities", map[string]any{"repository": in.Repository, "count": len(vulns)}, nil, "")
		return nil, vulnOut{Repository: target, Vulnerabilities: vulns, Total: len(vulns)}, nil
	})

	type planOut struct {
		Plan planner.Plan `json:"plan"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "plan_updates",
		Description: "Build an update plan under current policy. Mutates no repository; stores the plan and returns its id.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: false, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in repoIn) (*mcp.CallToolResult, planOut, error) {
		plan, err := svc.PlanUpdates(ctx, in.Repository)
		if err != nil {
			return nil, planOut{}, wrap(svc, "plan_updates", map[string]any{"repository": in.Repository}, err)
		}
		_ = svc.RecordTool(ctx, "plan_updates", map[string]any{"repository": in.Repository, "plan_id": plan.PlanID}, nil, "")
		return nil, planOut{Plan: plan}, nil
	})

	type getPlanIn struct {
		PlanID string `json:"plan_id" jsonschema:"plan id returned by plan_updates"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_plan",
		Description: "Retrieve a stored update plan by id.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getPlanIn) (*mcp.CallToolResult, planOut, error) {
		plan, err := svc.GetPlan(ctx, in.PlanID)
		if err != nil {
			return nil, planOut{}, wrap(svc, "get_plan", map[string]any{"plan_id": in.PlanID}, err)
		}
		_ = svc.RecordTool(ctx, "get_plan", map[string]any{"plan_id": in.PlanID}, nil, "")
		return nil, planOut{Plan: plan}, nil
	})
}
