package gomod

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

// Change is one dependency version bump.
type Change struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

// ApplyResult reports what actually happened, read back from the toolchain
// rather than assumed from the request (§8: the lockfile is the truth).
type ApplyResult struct {
	// Observed maps a dependency to its version before and after.
	Observed map[string][2]string `json:"observed,omitempty"`
	// Changed lists dependencies whose version moved.
	Changed []string `json:"changed,omitempty"`
	// Unsplittable is set when the diff is broader than the requested changes,
	// which happens whenever the package manager resolves transitively.
	Unsplittable bool `json:"unsplittable,omitempty"`
	// Commands records what was executed, for the audit log.
	Commands []string `json:"commands,omitempty"`
}

// Runner executes an external command in a directory. It exists so tests can
// exercise the applier without a real Go toolchain or network.
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) (string, error)
}

// ExecRunner runs commands with the ambient environment.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Apply updates a module in dir by delegating to the Go toolchain, then reads
// the resulting go.mod back so the plan records what really changed.
//
// Note what this does *not* do: it never hand-edits go.mod or go.sum. Editing
// lockfiles by hand is the main source of subtle breakage (§6).
func (e *Ecosystem) Apply(ctx context.Context, dir string, changes []Change) (ApplyResult, error) {
	res := ApplyResult{Observed: map[string][2]string{}}
	if len(changes) == 0 {
		return res, nil
	}
	before, err := readVersions(filepath.Join(dir, "go.mod"))
	if err != nil {
		return res, err
	}

	for _, ch := range changes {
		if ch.Name == "" || ch.To == "" {
			return res, fmt.Errorf("incomplete change %+v", ch)
		}
		args := []string{"get", ch.Name + "@" + ch.To}
		res.Commands = append(res.Commands, "go "+strings.Join(args, " "))
		if _, err := e.runner.Run(ctx, dir, "go", args...); err != nil {
			return res, fmt.Errorf("go get %s@%s: %w", ch.Name, ch.To, err)
		}
	}

	// Tidy keeps indirect requirements honest after the bumps.
	res.Commands = append(res.Commands, "go mod tidy")
	if _, err := e.runner.Run(ctx, dir, "go", "mod", "tidy"); err != nil {
		return res, fmt.Errorf("go mod tidy: %w", err)
	}

	after, err := readVersions(filepath.Join(dir, "go.mod"))
	if err != nil {
		return res, err
	}
	for name, beforeVer := range before {
		afterVer, ok := after[name]
		if !ok {
			continue
		}
		if afterVer != beforeVer {
			res.Observed[name] = [2]string{beforeVer, afterVer}
			res.Changed = append(res.Changed, name)
		}
	}
	for name, afterVer := range after {
		if _, existed := before[name]; !existed {
			res.Observed[name] = [2]string{"", afterVer}
			res.Changed = append(res.Changed, name)
		}
	}
	sort.Strings(res.Changed)
	// Anything that moved outside the requested set means the toolchain pulled
	// in transitive changes; say so instead of pretending the diff is surgical.
	requested := map[string]bool{}
	for _, ch := range changes {
		requested[ch.Name] = true
	}
	for _, name := range res.Changed {
		if !requested[name] {
			res.Unsplittable = true
			break
		}
	}
	return res, nil
}

// readVersions parses the requirement list of a go.mod.
func readVersions(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := make(map[string]string, len(f.Require))
	for _, r := range f.Require {
		out[r.Mod.Path] = r.Mod.Version
	}
	return out, nil
}
