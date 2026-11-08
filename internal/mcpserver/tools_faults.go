package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/thread3d/dependancythang/internal/app"
	"github.com/thread3d/dependancythang/internal/faults"
)

// registerFaultTools exposes depmcp's own defect backlog.
//
// The point of the journal is that the context which finds a defect is not the one
// that fixes it, so the tools are deliberately plain: read the backlog, file an
// entry, and record a resolution.
func registerFaultTools(s *mcp.Server, svc *app.Service) {
	type listIn struct {
		Repository      string `json:"repository,omitempty" jsonschema:"repository id or path holding .depmcp/faults"`
		IncludeResolved bool   `json:"include_resolved,omitempty" jsonschema:"also return entries already resolved"`
	}
	type listOut struct {
		Journal string         `json:"journal"`
		Open    int            `json:"open"`
		Total   int            `json:"total"`
		Faults  []faults.Entry `json:"faults"`
	}
	addTool(s, svc, &mcp.Tool{
		Name: "list_faults",
		Description: "List defects depmcp has recorded against itself, open first. " +
			"Each entry carries a fix hint, which is what a later session should act on.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, listOut, error) {
		all, dir, err := svc.ListFaults(ctx, in.Repository)
		if err != nil {
			return nil, listOut{}, wrap(svc, "list_faults", map[string]any{"repository": in.Repository}, err)
		}
		out := listOut{Journal: dir, Total: len(all)}
		for _, e := range all {
			if !e.Resolved {
				out.Open++
			}
			if in.IncludeResolved || !e.Resolved {
				out.Faults = append(out.Faults, e)
			}
		}
		_ = svc.RecordTool(ctx, "list_faults", map[string]any{"open": out.Open}, nil, "")
		return nil, out, nil
	})

	type recordIn struct {
		Repository string `json:"repository,omitempty" jsonschema:"repository id or path whose .depmcp/faults should hold the entry"`
		Kind       string `json:"kind" jsonschema:"internal-error | tool-failure | capability-gap | environment | friction"`
		Summary    string `json:"summary" jsonschema:"one line, stable across occurrences; it is part of the fingerprint"`
		Detail     string `json:"detail,omitempty" jsonschema:"the full text of this occurrence"`
		Hint       string `json:"hint,omitempty" jsonschema:"what a later session should do about it"`
		Severity   string `json:"severity,omitempty" jsonschema:"bug | gap | env | note"`
	}
	type recordOut struct {
		Fault faults.Fault `json:"fault"`
	}
	addTool(s, svc, &mcp.Tool{
		Name: "record_fault",
		Description: "Record a defect depmcp found in itself, or friction noticed while using it. " +
			"Recording is best effort and never changes the outcome of the operation that exposed it.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in recordIn) (*mcp.CallToolResult, recordOut, error) {
		kind := faults.Kind(in.Kind)
		if kind == "" {
			kind = faults.KindFriction
		}
		f, err := svc.RecordFault(ctx, in.Repository, faults.Fault{
			Kind:     kind,
			Severity: faults.Severity(in.Severity),
			Summary:  in.Summary,
			Detail:   in.Detail,
			Hint:     in.Hint,
			Command:  "record_fault",
		})
		if err != nil {
			return nil, recordOut{}, wrap(svc, "record_fault", map[string]any{"summary": in.Summary}, err)
		}
		return nil, recordOut{Fault: f}, nil
	})

	type resolveIn struct {
		Repository string `json:"repository,omitempty" jsonschema:"repository id or path"`
		FaultID    string `json:"fault_id" jsonschema:"id returned by list_faults, e.g. DEP-1A2B3C4D"`
		Note       string `json:"note,omitempty" jsonschema:"what fixed it"`
	}
	type resolveOut struct {
		Resolved bool   `json:"resolved"`
		FaultID  string `json:"fault_id"`
		Note     string `json:"note,omitempty"`
	}
	addTool(s, svc, &mcp.Tool{
		Name: "resolve_fault",
		Description: "Record that a fault was fixed. A resolution is an event, not a deletion: " +
			"if the fault is seen again afterwards it reopens, which is what a regression looks like.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in resolveIn) (*mcp.CallToolResult, resolveOut, error) {
		if err := svc.ResolveFault(ctx, in.Repository, in.FaultID, in.Note); err != nil {
			return nil, resolveOut{}, wrap(svc, "resolve_fault", map[string]any{"fault_id": in.FaultID}, err)
		}
		return nil, resolveOut{Resolved: true, FaultID: in.FaultID, Note: in.Note}, nil
	})
}
