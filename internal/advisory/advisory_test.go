package advisory_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thread3d/dependancythang/internal/advisory"
)

// fakeOSV serves the batch query followed by record hydration, which is how the
// real API behaves: the batch reply carries ids only.
func fakeOSV(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/querybatch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("batch query used %s, want POST", r.Method)
		}
		var body struct {
			Queries []struct {
				Package struct {
					Ecosystem string `json:"ecosystem"`
					Name      string `json:"name"`
				} `json:"package"`
				Version string `json:"version"`
			} `json:"queries"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode batch: %v", err)
		}
		results := make([]map[string]any, len(body.Queries))
		for i, q := range body.Queries {
			if q.Package.Name == "example.com/vulnerable" && q.Version == "v1.0.0" {
				results[i] = map[string]any{
					"vulns": []map[string]any{{"id": "GO-2026-0001", "modified": "2026-10-01T00:00:00Z"}},
				}
			} else {
				results[i] = map[string]any{}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	mux.HandleFunc("/v1/vulns/GO-2026-0001", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "GO-2026-0001",
			"aliases": []string{"CVE-2026-0001", "GHSA-aaaa-bbbb-cccc"},
			"summary": "Improper input validation <b>in parser</b>",
			"details": "See [the advisory](https://example.com/internal-admin) for details." +
				strings.Repeat(" padding", 200),
			"published":         "2026-09-01T00:00:00Z",
			"database_specific": map[string]any{"severity": "HIGH"},
			"affected": []map[string]any{{
				"package": map[string]any{"ecosystem": "Go", "name": "example.com/vulnerable"},
				"ranges": []map[string]any{{
					"type": "SEMVER",
					"events": []map[string]any{
						{"introduced": "0"},
						{"fixed": "v1.0.1"},
						{"fixed": "v1.1.0"},
					},
				}},
			}},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestQueryBatchHydratesAndNormalizes(t *testing.T) {
	srv := fakeOSV(t)
	c, err := advisory.NewClient(srv.URL, t.TempDir(), false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := c.QueryBatch(context.Background(), []advisory.Query{
		{Ecosystem: "Go", Name: "example.com/vulnerable", Version: "v1.0.0"},
		{Ecosystem: "Go", Name: "example.com/fine", Version: "v1.0.0"},
	})
	if err != nil {
		t.Fatalf("QueryBatch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("results = %d, want 2 (one per query)", len(got))
	}
	if len(got[1]) != 0 {
		t.Errorf("clean package reported advisories: %+v", got[1])
	}

	if len(got[0]) != 1 {
		t.Fatalf("vulnerable package advisories = %d, want 1", len(got[0]))
	}
	a := got[0][0]
	if a.ID != "GO-2026-0001" {
		t.Errorf("id = %q", a.ID)
	}
	if a.Severity != advisory.SeverityHigh {
		t.Errorf("severity = %q, want high from database_specific", a.Severity)
	}
	if len(a.FixedVersions) != 2 || a.FixedVersions[0] != "v1.0.1" {
		t.Errorf("fixed versions = %v, want [v1.0.1 v1.1.0]", a.FixedVersions)
	}
}

func TestUntrustedTextIsSanitized(t *testing.T) {
	srv := fakeOSV(t)
	c, err := advisory.NewClient(srv.URL, t.TempDir(), false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := c.QueryBatch(context.Background(), []advisory.Query{
		{Ecosystem: "Go", Name: "example.com/vulnerable", Version: "v1.0.0"},
	})
	if err != nil {
		t.Fatalf("QueryBatch: %v", err)
	}
	a := got[0][0]

	if strings.Contains(a.Summary, "<b>") || strings.Contains(a.Summary, "</b>") {
		t.Errorf("HTML survived sanitization: %q", a.Summary)
	}
	// A link that could point somewhere internal must not survive.
	if strings.Contains(a.Detail, "https://") || strings.Contains(a.Detail, "](") {
		t.Errorf("markdown link survived sanitization: %q", a.Detail)
	}
	if strings.Contains(a.Detail, "\n") {
		t.Errorf("newlines survived sanitization: %q", a.Detail)
	}
	// The detail text is bounded so a hostile advisory cannot flood a context.
	if len(a.Detail) > 600 {
		t.Errorf("detail length = %d, want it truncated", len(a.Detail))
	}
}

func TestHighestFixed(t *testing.T) {
	a := advisory.Advisory{FixedVersions: []string{"v1.0.1", "v1.1.0", "v1.0.9"}}
	if got := a.HighestFixed(); got != "v1.1.0" {
		t.Errorf("HighestFixed = %q, want v1.1.0", got)
	}
}

func TestEmptyQueryBatchMakesNoRequest(t *testing.T) {
	c, err := advisory.NewClient("http://127.0.0.1:1", t.TempDir(), false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := c.QueryBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("QueryBatch(nil) = %v, want no error", err)
	}
	if len(got) != 0 {
		t.Errorf("results = %d, want 0", len(got))
	}
}
