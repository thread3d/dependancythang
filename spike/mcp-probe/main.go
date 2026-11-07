// Command mcp-probe is a throwaway MCP server used for milestone M-1 of the
// depmcp specification: it exists to answer five capability questions about the
// official Go SDK (github.com/modelcontextprotocol/go-sdk) by running, not by
// reading release notes.
//
//	Q1  Does server->client elicitation round-trip to a real client?
//	Q2  Does sampling (server asks the client's model) work?
//	Q3  Do notifications/progress arrive mid-call, or only at the end?
//	Q4  What does "schema generated from Go structs" actually cost?
//	Q5  Does a cancelled context stop a long tool cleanly?
//
// Run it over stdio:
//
//	go run .            # speaks MCP on stdin/stdout; logs to stderr
//
// Every tool result is deliberately small and self-describing so the answers
// appear in the transcript of whichever client is driving.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ref is a helper for the pointer-valued annotation hints.
func ref[T any](v T) *T { return &v }

// logf writes to stderr only: stdout carries protocol frames and nothing else.
func logf(format string, args ...any) {
	log.Printf(format, args...)
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-probe",
		Version: "v0.0.1",
	}, nil)

	registerTools(server)
	registerResource(server)

	logf("mcp-probe starting on stdio (go %s %s/%s)", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		logf("server stopped: %v", err)
		os.Exit(1)
	}
	logf("server exited cleanly")
}

func registerTools(s *mcp.Server) {
	// ---- Q4: schema cost -------------------------------------------------
	// Input and output types are plain Go structs; the only schema work here
	// is `json` tags for names and `jsonschema` tags for descriptions.
	type pingIn struct {
		Message string `json:"message" jsonschema:"text to echo back"`
		Repeat  int    `json:"repeat,omitempty" jsonschema:"how many times to repeat it"`
	}
	type pingOut struct {
		Echo     string `json:"echo"`
		Length   int    `json:"length"`
		Repeated int    `json:"repeated"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ping",
		Description: "Echo a message back. Read-only. Exists so the client can show the generated input/output schemas.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  ref(false),
		},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in pingIn) (*mcp.CallToolResult, pingOut, error) {
		reps := in.Repeat
		if reps <= 0 {
			reps = 1
		}
		out := ""
		for i := 0; i < reps; i++ {
			out += in.Message
		}
		return nil, pingOut{Echo: out, Length: len(out), Repeated: reps}, nil
	})

	// ---- Q1: elicitation round-trip -------------------------------------
	// A write-shaped tool that asks the client to confirm before "applying".
	// The answer is reported back verbatim so the client transcript proves
	// whether elicitation actually reached a human.
	type confirmIn struct {
		Reason string `json:"reason" jsonschema:"what the write would do"`
	}
	type confirmOut struct {
		Action  string         `json:"action"` // accept | decline | cancel | unavailable
		Detail  string         `json:"detail"`
		Consent map[string]any `json:"consent,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "confirm_write",
		Description: "Write-shaped tool gated behind MCP elicitation. Reports exactly what the client answered.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: ref(true),
			IdempotentHint:  true,
			OpenWorldHint:   ref(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in confirmIn) (*mcp.CallToolResult, confirmOut, error) {
		if req.Session == nil {
			return nil, confirmOut{Action: "unavailable", Detail: "no session on request"}, nil
		}
		schema := &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"proceed": {Type: "boolean", Description: "apply the change"},
			},
			Required: []string{"proceed"},
		}
		res, err := req.Session.Elicit(ctx, &mcp.ElicitParams{
			Message:         "Apply this change? " + in.Reason,
			RequestedSchema: schema,
		})
		if err != nil {
			// A client without elicitation support surfaces here. That is a
			// Q1 answer in itself, so report it instead of failing the call.
			return nil, confirmOut{Action: "unavailable", Detail: err.Error()}, nil
		}
		return nil, confirmOut{Action: res.Action, Detail: "elicitation returned", Consent: res.Content}, nil
	})

	// ---- Q3 / Q5: progress + cancellation -------------------------------
	type slowIn struct {
		Steps int `json:"steps,omitempty" jsonschema:"number of steps (default 4)"`
	}
	type slowOut struct {
		Steps     int    `json:"steps"`
		HadToken  bool   `json:"had_progress_token"`
		Notices   int    `json:"notices_sent"`
		Elapsed   string `json:"elapsed"`
		Cancelled bool   `json:"cancelled"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "slow_task",
		Description: "Sleeps in steps, emitting notifications/progress as it goes. Use to test progress delivery and cancellation.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: false,
			OpenWorldHint:  ref(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in slowIn) (*mcp.CallToolResult, slowOut, error) {
		steps := in.Steps
		if steps <= 0 {
			steps = 4
		}
		token := req.Params.GetProgressToken()
		start := time.Now()
		notices := 0
		for i := 1; i <= steps; i++ {
			select {
			case <-ctx.Done():
				logf("slow_task cancelled at step %d/%d", i, steps)
				return nil, slowOut{
					Steps: steps, HadToken: token != nil, Notices: notices,
					Elapsed: time.Since(start).String(), Cancelled: true,
				}, ctx.Err()
			default:
			}
			if token != nil && req.Session != nil {
				err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: token,
					Progress:      float64(i),
					Total:         float64(steps),
					Message:       fmt.Sprintf("step %d/%d", i, steps),
				})
				if err != nil {
					logf("NotifyProgress failed at step %d: %v", i, err)
				} else {
					notices++
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		return nil, slowOut{
			Steps: steps, HadToken: token != nil, Notices: notices,
			Elapsed: time.Since(start).String(),
		}, nil
	})

	// ---- Q2: sampling ---------------------------------------------------
	type summarizeIn struct {
		Text string `json:"text" jsonschema:"text to summarize via the client's model"`
	}
	type summarizeOut struct {
		Supported bool   `json:"supported"`
		Model     string `json:"model,omitempty"`
		Summary   string `json:"summary,omitempty"`
		Error     string `json:"error,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "summarize",
		Description: "Asks the client's model to summarize text (MCP sampling). Reports whether sampling is available.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: false,
			OpenWorldHint:  ref(true),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in summarizeIn) (*mcp.CallToolResult, summarizeOut, error) {
		if req.Session == nil {
			return nil, summarizeOut{Supported: false, Error: "no session"}, nil
		}
		res, err := req.Session.CreateMessage(ctx, &mcp.CreateMessageParams{
			MaxTokens:    128,
			SystemPrompt: "Summarize the user's text in one short sentence.",
			Messages: []*mcp.SamplingMessage{{
				Role:    mcp.Role("user"),
				Content: &mcp.TextContent{Text: in.Text},
			}},
		})
		if err != nil {
			return nil, summarizeOut{Supported: false, Error: err.Error()}, nil
		}
		summary := ""
		if tc, ok := res.Content.(*mcp.TextContent); ok {
			summary = tc.Text
		}
		return nil, summarizeOut{Supported: true, Model: res.Model, Summary: summary}, nil
	})

	// ---- environment probe ---------------------------------------------
	type envOut struct {
		Go       string `json:"go"`
		Platform string `json:"platform"`
		NumCPU   int    `json:"num_cpu"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "env",
		Description: "Reports the Go runtime and platform running the server.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ref(false)},
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, envOut, error) {
		return nil, envOut{
			Go:       runtime.Version(),
			Platform: runtime.GOOS + "/" + runtime.GOARCH,
			NumCPU:   runtime.NumCPU(),
		}, nil
	})
}

// registerResource proves resource listing and reading, since §7 of the spec
// leans on ten of them.
func registerResource(s *mcp.Server) {
	s.AddResource(&mcp.Resource{
		URI:         "probe://info",
		Name:        "probe-info",
		Description: "Static JSON describing this probe server.",
		MIMEType:    "application/json",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		body, err := json.MarshalIndent(map[string]any{
			"server":   "mcp-probe",
			"version":  "v0.0.1",
			"go":       runtime.Version(),
			"platform": runtime.GOOS + "/" + runtime.GOARCH,
			"tools":    []string{"ping", "confirm_write", "slow_task", "summarize", "env"},
			"purpose":  "M-1 capability spike for the depmcp specification",
		}, "", "  ")
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     string(body),
			}},
		}, nil
	})
}
