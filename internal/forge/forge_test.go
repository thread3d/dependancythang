package forge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/thread3d/dependancythang/internal/forge"
)

func TestLocalForgeWritesReport(t *testing.T) {
	dir := t.TempDir()
	fg := forge.Local{Dir: dir}
	ref, err := fg.OpenPR(context.Background(), "/repo", forge.PullRequest{
		Branch: "depmcp/gomod-20261107",
		Body:   "# report\n",
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if ref.Report == "" {
		t.Fatal("local forge returned no report path")
	}
	data, err := os.ReadFile(ref.Report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(data), "# report") {
		t.Errorf("report content = %q", data)
	}
	if filepath.Dir(ref.Report) != dir {
		t.Errorf("report written to %s, want %s", ref.Report, dir)
	}
}

// fakeGitHub serves the two endpoints the forge uses.
func fakeGitHub(t *testing.T, posts *int64, existing []map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(existing)
		case http.MethodPost:
			atomic.AddInt64(posts, 1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, key := range []string{"title", "head", "base", "body"} {
				if _, ok := body[key]; !ok {
					t.Errorf("pull request payload missing %q", key)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "html_url": "https://example/pr/42"})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestGitHubOpenPR(t *testing.T) {
	var posts int64
	srv := fakeGitHub(t, &posts, nil)
	fg := &forge.GitHub{
		Remote: forge.Remote{Base: srv.URL, Token: "secret-token"},
		Owner:  "o", Repo: "r",
	}
	ref, err := fg.OpenPR(context.Background(), "", forge.PullRequest{
		Branch: "depmcp/x-20260101", Base: "main",
		Title: "build(deps): bump x", Body: forge.RenderBody(forge.BodyInput{}),
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if posts != 1 {
		t.Errorf("POSTs = %d, want 1", posts)
	}
	if ref.Number != 42 || ref.URL == "" {
		t.Errorf("ref = %+v, want number 42 and a URL", ref)
	}
}

// TestGitHubIsIdempotent is the §11 promise: a later run recognises its own pull
// request instead of opening a duplicate.
// TestGitHubSendsBearerToken checks the token reaches the API, which is the one
// thing a fake handler can verify about authentication.
func TestGitHubSendsBearerToken(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		seen = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "html_url": "u"})
	}))
	defer srv.Close()

	fg := &forge.GitHub{Remote: forge.Remote{Base: srv.URL, Token: "secret-token"}, Owner: "o", Repo: "r"}
	if _, err := fg.OpenPR(context.Background(), "", forge.PullRequest{Branch: "b", Base: "main"}); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if seen != "Bearer secret-token" {
		t.Errorf("authorization header = %q, want a bearer token", seen)
	}
}

func TestGitHubIsIdempotent(t *testing.T) {
	var posts int64
	existing := []map[string]any{{
		"number": 7, "html_url": "https://example/pr/7",
		"body": "… " + forge.Marker,
		"head": map[string]any{"ref": "depmcp/x-20260101"},
	}}
	srv := fakeGitHub(t, &posts, existing)
	fg := &forge.GitHub{Remote: forge.Remote{Base: srv.URL}, Owner: "o", Repo: "r"}

	ref, err := fg.OpenPR(context.Background(), "", forge.PullRequest{
		Branch: "depmcp/x-20260101", Base: "main", Title: "t", Body: "b",
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if posts != 0 {
		t.Error("a duplicate pull request was opened")
	}
	if ref.Number != 7 {
		t.Errorf("returned %+v, want the existing PR", ref)
	}
}

func TestGitHubIgnoresForeignPRs(t *testing.T) {
	var posts int64
	// An open PR on the same branch that depmcp did not create.
	existing := []map[string]any{{
		"number": 9, "body": "hand-written", "head": map[string]any{"ref": "depmcp/x-20260101"},
	}}
	srv := fakeGitHub(t, &posts, existing)
	fg := &forge.GitHub{Remote: forge.Remote{Base: srv.URL}, Owner: "o", Repo: "r"}

	if _, err := fg.OpenPR(context.Background(), "", forge.PullRequest{
		Branch: "depmcp/x-20260101", Base: "main", Title: "t", Body: "b",
	}); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if posts != 1 {
		t.Errorf("POSTs = %d, want 1: a PR without our marker is not ours", posts)
	}
}

func TestBodyCarriesMarkerAndRevertCommand(t *testing.T) {
	body := forge.RenderBody(forge.BodyInput{
		RunID:        "run-1",
		Changes:      []forge.ChangeLine{{Dependency: "x", From: "v1", To: "v2", Class: "minor", Advisories: []string{"GO-1"}}},
		Verification: "passed in bubblewrap",
	})
	for _, want := range []string{forge.Marker, "depmcp revert run-1", "GO-1", "passed in bubblewrap"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestTitleShapes(t *testing.T) {
	one := []forge.ChangeLine{{Dependency: "x", From: "v1", To: "v2"}}
	if got := forge.Title(one, "g"); !strings.Contains(got, "bump x from v1 to v2") {
		t.Errorf("single title = %q", got)
	}
	many := []forge.ChangeLine{{Dependency: "x"}, {Dependency: "y"}}
	if got := forge.Title(many, "golang-x"); !strings.Contains(got, "golang-x group with 2 updates") {
		t.Errorf("group title = %q", got)
	}
}
