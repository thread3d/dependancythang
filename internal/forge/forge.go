// Package forge publishes a verified branch to a code host.
//
// The forge layer is optional and last: the default is `local`, which stops at a
// verified branch and writes a report, because an unattended run should not speak
// to a remote unless the user asked it to (§11).
package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Marker identifies pull requests this tool owns, so later runs can find and
// update them instead of opening duplicates (§11).
const Marker = "<!-- generated-by: depmcp -->"

// PullRequest is the content of a proposed change.
type PullRequest struct {
	Repo   string   `json:"repo"`
	Branch string   `json:"branch"`
	Base   string   `json:"base"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
	Draft  bool     `json:"draft,omitempty"`
}

// PRRef identifies an opened pull request.
type PRRef struct {
	Forge  string `json:"forge"`
	Number int    `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
	// Head is the branch the request was opened from, used to avoid duplicates.
	Head string `json:"head,omitempty"`
	// Report is set by the local forge instead of a URL.
	Report string `json:"report,omitempty"`
}

// Forge publishes a branch and, optionally, a pull request.
type Forge interface {
	Name() string
	// Push publishes the branch to the remote.
	Push(ctx context.Context, repoPath, branch string) error
	// OpenPR opens (or updates) a pull request for the branch.
	OpenPR(ctx context.Context, repo string, pr PullRequest) (PRRef, error)
	// ListManagedPRs returns pull requests carrying Marker.
	ListManagedPRs(ctx context.Context, repo string) ([]PRRef, error)
}

// Local stops at a verified branch and writes a markdown report (§11).
type Local struct {
	// Dir is where reports are written; usually <state>/reports.
	Dir string
}

// Name implements Forge.
func (Local) Name() string { return "local" }

// Push is a no-op: a local forge never leaves the machine.
func (Local) Push(context.Context, string, string) error { return nil }

// OpenPR writes the report and returns its path.
func (l Local) OpenPR(_ context.Context, _ string, pr PullRequest) (PRRef, error) {
	if l.Dir == "" {
		return PRRef{}, fmt.Errorf("local forge has no report directory")
	}
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return PRRef{}, fmt.Errorf("create %s: %w", l.Dir, err)
	}
	name := safeName(pr.Branch) + ".md"
	path := filepath.Join(l.Dir, name)
	if err := os.WriteFile(path, []byte(pr.Body), 0o644); err != nil {
		return PRRef{}, fmt.Errorf("write %s: %w", path, err)
	}
	return PRRef{Forge: "local", Report: path}, nil
}

// ListManagedPRs reports the reports this forge has written.
func (l Local) ListManagedPRs(context.Context, string) ([]PRRef, error) {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []PRRef
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, PRRef{Forge: "local", Report: filepath.Join(l.Dir, e.Name())})
	}
	return out, nil
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		default:
			return '-'
		}
	}, s)
}

// Remote is the shared machinery for REST forges: one HTTP client, one token,
// one JSON POST/PUT/GET.
type Remote struct {
	// Base is the API root, e.g. https://api.github.com.
	Base string
	// Token authenticates. It is never logged or echoed.
	Token string
	// HTTP is replaced in tests.
	HTTP *http.Client
}

func (r *Remote) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// do performs a request and decodes a JSON response into out when non-nil.
func (r *Remote) do(ctx context.Context, method, url string, body any, out any, headers map[string]string) (int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, firstLine(string(payload)))
	}
	if out != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s response: %w", url, err)
		}
	}
	return resp.StatusCode, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// Body renders the pull request body from verified facts only (§11): a table of
// changes, advisory ids, the verification result, and how to undo it.
type BodyInput struct {
	RunID        string
	Branch       string
	Changes      []ChangeLine
	Verification string
	StaleRef     bool
	ReportedBy   string
}

// ChangeLine is one row of the change table.
type ChangeLine struct {
	Dependency string   `json:"dependency"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Class      string   `json:"class"`
	Advisories []string `json:"advisories,omitempty"`
}

// RenderBody builds the markdown body shared by every remote forge.
func RenderBody(in BodyInput) string {
	var b strings.Builder
	b.WriteString("## Dependency updates\n\n")
	b.WriteString("| Package | From | To | Update | Advisories |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, c := range in.Changes {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s | %s |\n",
			c.Dependency, c.From, c.To, c.Class, strings.Join(c.Advisories, ", "))
	}
	b.WriteString("\n")
	if in.Verification != "" {
		fmt.Fprintf(&b, "**Verification:** %s\n\n", in.Verification)
	}
	if in.StaleRef {
		b.WriteString("**Note:** this branch was based on a local default branch because fetching was disabled or failed.\n\n")
	}
	if in.RunID != "" {
		fmt.Fprintf(&b, "To undo: `depmcp revert %s`\n\n", in.RunID)
	}
	b.WriteString("---\n")
	b.WriteString(Marker)
	if in.ReportedBy != "" {
		fmt.Fprintf(&b, "\n\nGenerated by depmcp for %s.", in.ReportedBy)
	}
	return b.String()
}

// Title renders a conventional pull request title.
func Title(changes []ChangeLine, group string) string {
	switch {
	case len(changes) == 1:
		c := changes[0]
		return fmt.Sprintf("build(deps): bump %s from %s to %s", c.Dependency, c.From, c.To)
	case group != "":
		return fmt.Sprintf("build(deps): bump the %s group with %d updates", group, len(changes))
	default:
		return fmt.Sprintf("build(deps): apply %d dependency updates", len(changes))
	}
}
