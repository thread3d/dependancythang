package verify_test

import (
	"context"
	"testing"
	"time"

	"github.com/thread3d/dependancythang/internal/sandbox"
	"github.com/thread3d/dependancythang/internal/verify"
)

// fakeSandbox returns canned results per command, so verification logic can be
// tested without namespaces.
type fakeSandbox struct {
	results map[string]sandbox.Result
	ran     []string
}

func (f *fakeSandbox) Name() string { return "fake" }

func (f *fakeSandbox) Run(_ context.Context, req sandbox.Request) (sandbox.Result, error) {
	key := ""
	if len(req.Argv) > 0 {
		key = req.Argv[0]
	}
	f.ran = append(f.ran, key)
	if res, ok := f.results[key]; ok {
		res.Argv = req.Argv
		return res, nil
	}
	return sandbox.Result{Argv: req.Argv}, nil
}

func TestAllCommandsPassing(t *testing.T) {
	sb := &fakeSandbox{results: map[string]sandbox.Result{
		"go": {ExitCode: 0, Output: "ok"},
	}}
	cmds := []verify.Command{
		{Label: "build", Argv: []string{"go", "build", "./..."}},
		{Label: "test", Argv: []string{"go", "test", "./..."}},
	}
	res, err := verify.Run(context.Background(), sb, cmds, verify.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Passed {
		t.Errorf("passed = false: %s", res.Summary)
	}
	if len(res.Commands) != 2 {
		t.Errorf("commands run = %d, want 2", len(res.Commands))
	}
	if len(sb.ran) != 2 {
		t.Errorf("sandbox invoked %d times, want 2", len(sb.ran))
	}
}

func TestFailureStopsTheSequence(t *testing.T) {
	sb := &fakeSandbox{results: map[string]sandbox.Result{
		"go": {ExitCode: 2, Output: "build failed"},
	}}
	cmds := []verify.Command{
		{Label: "build", Argv: []string{"go", "build", "./..."}},
		{Label: "test", Argv: []string{"go", "test", "./..."}},
	}
	res, err := verify.Run(context.Background(), sb, cmds, verify.Options{
		Dir: t.TempDir(), StopOnFailure: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Passed {
		t.Error("passed = true despite a failing command")
	}
	if len(res.Commands) != 1 {
		t.Errorf("commands run = %d, want 1 (stop at first failure)", len(res.Commands))
	}
	if len(sb.ran) != 1 {
		t.Errorf("sandbox invoked %d times, want 1", len(sb.ran))
	}
	if res.Summary == "" {
		t.Error("summary is empty for a failure")
	}
}

func TestTimeoutIsReportedInSummary(t *testing.T) {
	sb := &fakeSandbox{results: map[string]sandbox.Result{
		"go": {TimedOut: true, ExitCode: -1},
	}}
	res, err := verify.Run(context.Background(), sb,
		[]verify.Command{{Label: "test", Argv: []string{"go", "test"}}},
		verify.Options{Dir: t.TempDir(), StopOnFailure: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Passed {
		t.Error("a timed-out command reported success")
	}
	if !contains(res.Summary, "timed out") {
		t.Errorf("summary = %q, want it to mention the timeout", res.Summary)
	}
}

func TestUnconfinedIsRecorded(t *testing.T) {
	res, err := verify.Run(context.Background(), sandbox.Unconfined{},
		[]verify.Command{{Label: "true", Argv: []string{"/bin/sh", "-c", "true"}}},
		verify.Options{Dir: t.TempDir(), Env: map[string]string{"PATH": "/usr/bin:/bin"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Unconfined {
		t.Error("unconfined result was not marked as such")
	}
	if !res.Passed {
		t.Errorf("expected pass: %s", res.Summary)
	}
}

func TestEmptyCommandListIsAnError(t *testing.T) {
	if _, err := verify.Run(context.Background(), sandbox.Unconfined{}, nil, verify.Options{}); err == nil {
		t.Error("expected an error for no commands")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(haystack) > 0 && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
