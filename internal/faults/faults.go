// Package faults records defects depmcp finds in itself.
//
// The design follows two tools that already do this — CodeWitness's self-issue
// journal and jCheck's self-report log — because the hard part is not writing the
// file, it is making the record survive the context that found it.
//
// Three rules carry over from them:
//
//   - A fault outlives the thing that exposed it. The journal therefore lives in
//     the repository (`.depmcp/faults/`), not in a run directory, so clearing runs
//     does not erase the backlog.
//   - An id is a fingerprint, not a counter. The same defect seen on a hundred
//     runs is one entry with a count, not a hundred entries.
//   - A resolution is an event, not a deletion. If the fault is seen again after
//     it was resolved, it is open again — which is exactly what a regression
//     should look like.
//
// Recording is best effort by contract: a failure to log a defect must never
// replace or mask the defect itself.
package faults

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind is the triage axis. A later session should read these first.
type Kind string

// Kinds, matching jCheck's vocabulary where it fits.
const (
	// KindInternal is a defect in depmcp: a panic, or an impossible state.
	KindInternal Kind = "internal-error"
	// KindTool is an operation depmcp ran that failed unexpectedly.
	KindTool Kind = "tool-failure"
	// KindGap is something depmcp cannot do yet, discovered in use.
	KindGap Kind = "capability-gap"
	// KindEnv is the machine's limitation, not depmcp's fault.
	KindEnv Kind = "environment"
	// KindFriction is a note recorded by hand about the tool's behaviour.
	KindFriction Kind = "friction"
)

// Severity orders the backlog.
type Severity string

// Severities.
const (
	SevBug  Severity = "bug"
	SevGap  Severity = "gap"
	SevEnv  Severity = "env"
	SevNote Severity = "note"
)

// Fault is one recorded defect.
type Fault struct {
	// ID is a fingerprint of kind, signature, and stack frame: the same defect
	// across runs keeps one id.
	ID       string   `json:"id"`
	At       string   `json:"time"`
	Kind     Kind     `json:"kind"`
	Severity Severity `json:"severity"`
	// Summary is one line, stable across occurrences, and part of the fingerprint.
	Summary string `json:"summary"`
	// Detail is the full text of the occurrence; it may contain paths that change
	// between runs, so it is deliberately not part of the fingerprint.
	Detail string `json:"detail,omitempty"`
	// Hint tells a later session what to do. This is the field that makes the
	// backlog actionable rather than merely observant.
	Hint string `json:"hint,omitempty"`
	// Command is the tool or CLI verb that was running.
	Command   string   `json:"command,omitempty"`
	Repo      string   `json:"repo,omitempty"`
	Version   string   `json:"version,omitempty"`
	GoVersion string   `json:"go_version,omitempty"`
	Platform  string   `json:"platform,omitempty"`
	Stack     string   `json:"stack,omitempty"`
	Evidence  []string `json:"evidence,omitempty"`
}

// Resolution records that a fault was fixed, without deleting it.
type Resolution struct {
	ID     string `json:"id"`
	At     string `json:"time"`
	Note   string `json:"note,omitempty"`
	By     string `json:"by,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// Entry is one fault as the digest reports it.
type Entry struct {
	Fault
	Count      int    `json:"count"`
	FirstSeen  string `json:"first_seen"`
	LastSeen   string `json:"last_seen"`
	Resolved   bool   `json:"resolved"`
	ResolvedAt string `json:"resolved_at,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

// Dir returns the journal directory for a repository.
func Dir(repoPath string) string {
	return filepath.Join(repoPath, ".depmcp", "faults")
}

// Journal is an on-disk fault backlog.
type Journal struct {
	dir string
	mu  sync.Mutex
}

// Open prepares a journal in dir.
func Open(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create fault journal dir: %w", err)
	}
	return &Journal{dir: dir}, nil
}

// Dir reports where the journal writes.
func (j *Journal) Dir() string { return j.dir }

// issuesPath is the source of truth: one row per occurrence.
func (j *Journal) issuesPath() string { return filepath.Join(j.dir, "issues.jsonl") }

// resolvedPath holds resolution events.
func (j *Journal) resolvedPath() string { return filepath.Join(j.dir, "resolved.jsonl") }

// digestPath is the regenerated human-readable journal.
func (j *Journal) digestPath() string { return filepath.Join(j.dir, "FAULTS.md") }

// Fingerprint derives a stable id for a fault.
//
// The signature excludes the detail text on purpose. Details carry run ids,
// temporary paths, and registry error bodies that change on every occurrence; if
// they participated in the hash, one defect would become a stream of entries and
// the backlog would be unreadable. The stack's first depmcp frame is preferred
// over the summary when one exists because it is more specific, which is the
// choice CodeWitness documents for the same reason.
//
// The consequence, stated plainly: Summary is an identity, so a caller filing a
// new kind of defect must give it a distinguishable one-line summary.
func Fingerprint(kind Kind, summary, _ string, stack string) string {
	sig := summary
	if frame := firstFrame(stack); frame != "" {
		sig = frame
	}
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + sig))
	return "DEP-" + strings.ToUpper(hex.EncodeToString(sum[:4]))
}

// firstFrame extracts the first non-runtime frame from a stack trace.
func firstFrame(stack string) string {
	for _, line := range strings.Split(stack, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "depmcp") && !strings.Contains(line, "github.com/thread3d/dependancythang") {
			continue
		}
		if strings.Contains(line, "runtime.") {
			continue
		}
		return line
	}
	return ""
}

// Record appends one occurrence and regenerates the digest.
//
// It is safe to call from a deferred recover, and callers are expected to ignore
// its error: logging a defect must never become the defect.
func (j *Journal) Record(f Fault) (Fault, error) {
	if j == nil {
		return f, fmt.Errorf("no fault journal")
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	if f.At == "" {
		f.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if f.ID == "" {
		f.ID = Fingerprint(f.Kind, f.Summary, f.Detail, f.Stack)
	}
	if f.Severity == "" {
		f.Severity = defaultSeverity(f.Kind)
	}
	if f.GoVersion == "" {
		f.GoVersion = runtime.Version()
	}
	if f.Platform == "" {
		f.Platform = runtime.GOOS + "/" + runtime.GOARCH
	}
	if f.Detail != "" {
		f.Detail = truncate(f.Detail, 4000)
	}
	if f.Stack != "" {
		f.Stack = truncate(f.Stack, 4000)
	}

	line, err := json.Marshal(f)
	if err != nil {
		return f, fmt.Errorf("encode fault: %w", err)
	}
	if err := appendLine(j.issuesPath(), line); err != nil {
		return f, err
	}
	// The digest is derived, so a failure to write it must not fail the record.
	_ = j.regenerate()
	return f, nil
}

// Resolve records a resolution event.
func (j *Journal) Resolve(r Resolution) error {
	if j == nil {
		return fmt.Errorf("no fault journal")
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	if r.At == "" {
		r.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode resolution: %w", err)
	}
	if err := appendLine(j.resolvedPath(), line); err != nil {
		return err
	}
	return j.regenerate()
}

// List returns the backlog grouped by id, unresolved first and bugs before the
// rest. It is what a later session reads.
func (j *Journal) List() ([]Entry, error) {
	if j == nil {
		return nil, fmt.Errorf("no fault journal")
	}
	occurrences, err := j.readIssues()
	if err != nil {
		return nil, err
	}
	resolutions, err := j.readResolutions()
	if err != nil {
		return nil, err
	}
	return merge(occurrences, resolutions), nil
}

// merge folds occurrences and resolutions into one entry per id.
func merge(occurrences []Fault, resolutions []Resolution) []Entry {
	byID := map[string]*Entry{}
	order := make([]string, 0, len(occurrences))

	for _, f := range occurrences {
		e, ok := byID[f.ID]
		if !ok {
			copyOf := f
			e = &Entry{Fault: copyOf, FirstSeen: f.At, LastSeen: f.At}
			byID[f.ID] = e
			order = append(order, f.ID)
		}
		e.Count++
		if f.At < e.FirstSeen {
			e.FirstSeen = f.At
		}
		// The newest occurrence supplies the detail and hint a reader should see.
		if f.At >= e.LastSeen {
			e.LastSeen = f.At
			if f.Detail != "" {
				e.Detail = f.Detail
			}
			if f.Hint != "" {
				e.Hint = f.Hint
			}
		}
	}

	for _, r := range resolutions {
		if e, ok := byID[r.ID]; ok {
			e.Resolved = true
			e.ResolvedAt = r.At
			e.Resolution = r.Note
			// A resolution only holds if nothing was seen afterwards: a later
			// occurrence is a regression and reopens the entry.
			if e.LastSeen > r.At {
				e.Resolved = false
			}
		}
	}

	out := make([]Entry, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	sort.SliceStable(out, func(i, k int) bool {
		if out[i].Resolved != out[k].Resolved {
			return !out[i].Resolved // open first
		}
		if out[i].Severity != out[k].Severity {
			return severityRank(out[i].Severity) > severityRank(out[k].Severity)
		}
		return out[i].LastSeen > out[k].LastSeen
	})
	return out
}

func (j *Journal) readIssues() ([]Fault, error) {
	lines, err := readLines(j.issuesPath())
	if err != nil {
		return nil, err
	}
	out := make([]Fault, 0, len(lines))
	for _, line := range lines {
		var f Fault
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			continue // a corrupt line must not hide the rest of the backlog
		}
		out = append(out, f)
	}
	return out, nil
}

func (j *Journal) readResolutions() ([]Resolution, error) {
	lines, err := readLines(j.resolvedPath())
	if err != nil {
		return nil, err
	}
	out := make([]Resolution, 0, len(lines))
	for _, line := range lines {
		var r Resolution
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// regenerate rewrites the markdown digest from the records.
func (j *Journal) regenerate() error {
	entries, err := j.List()
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# depmcp faults found in itself\n\n")
	b.WriteString("Generated from `issues.jsonl` and `resolved.jsonl`; do not edit by hand.\n")
	b.WriteString("Read the open entries, fix them, then record a resolution with `depmcp faults resolve <id>`.\n")
	b.WriteString("A fault seen again after its resolution reopens automatically.\n\n")

	open, resolved := 0, 0
	for _, e := range entries {
		if e.Resolved {
			resolved++
		} else {
			open++
		}
	}
	fmt.Fprintf(&b, "%d open, %d resolved.\n\n", open, resolved)

	for _, e := range entries {
		if e.Resolved {
			continue
		}
		fmt.Fprintf(&b, "## %s — %s\n\n", e.ID, e.Summary)
		fmt.Fprintf(&b, "- kind: `%s` · severity: `%s` · seen %d time(s)\n", e.Kind, e.Severity, e.Count)
		fmt.Fprintf(&b, "- first: %s · last: %s\n", e.FirstSeen, e.LastSeen)
		if e.Command != "" {
			fmt.Fprintf(&b, "- command: `%s`\n", e.Command)
		}
		if e.Hint != "" {
			fmt.Fprintf(&b, "- **fix:** %s\n", e.Hint)
		}
		if e.Detail != "" {
			fmt.Fprintf(&b, "\n```\n%s\n```\n", strings.TrimSpace(e.Detail))
		}
		b.WriteString("\n")
	}

	if resolved > 0 {
		b.WriteString("## Resolved\n\n")
		for _, e := range entries {
			if !e.Resolved {
				continue
			}
			fmt.Fprintf(&b, "- %s — %s (resolved %s", e.ID, e.Summary, e.ResolvedAt)
			if e.Resolution != "" {
				fmt.Fprintf(&b, ": %s", e.Resolution)
			}
			b.WriteString(")\n")
		}
	}
	return os.WriteFile(j.digestPath(), []byte(b.String()), 0o644)
}

// appendLine appends one JSON line, creating the file when needed.
func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// readLines reads a JSONL file, treating a missing file as empty.
func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func defaultSeverity(k Kind) Severity {
	switch k {
	case KindInternal, KindTool:
		return SevBug
	case KindGap:
		return SevGap
	case KindEnv:
		return SevEnv
	default:
		return SevNote
	}
}

func severityRank(s Severity) int {
	switch s {
	case SevBug:
		return 3
	case SevGap:
		return 2
	case SevEnv:
		return 1
	default:
		return 0
	}
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n…truncated"
}
