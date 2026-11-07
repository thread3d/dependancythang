// Package advisory queries OSV.dev for vulnerabilities affecting a version.
//
// Everything returned here is untrusted data: summaries and details come from
// third parties and are truncated and marked as data before they ever reach a
// model's context (§11).
package advisory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/thread3d/dependancythang/internal/httpcache"
)

// OSVURL is the OSV API base.
const OSVURL = "https://api.osv.dev"

// Severity labels, normalized across feeds.
type Severity string

// Normalized severities.
const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityUnknown  Severity = "unknown"
)

// maxText bounds untrusted free text carried into tool output.
const maxText = 512

// Advisory is a normalized vulnerability record.
type Advisory struct {
	ID       string   `json:"id"`
	Aliases  []string `json:"aliases,omitempty"`
	Summary  string   `json:"summary,omitempty"`
	Detail   string   `json:"detail,omitempty"` // truncated; untrusted
	Severity Severity `json:"severity"`
	// Score is the numeric base score (CVSS when available, otherwise the
	// representative score for the label). Every advisory has one.
	Score float64 `json:"score"`
	// SeveritySource records where the label came from, so "unknown" is
	// distinguishable from "deliberately unscored".
	SeveritySource string   `json:"severity_source,omitempty"`
	Published      string   `json:"published,omitempty"`
	Withdrawn      string   `json:"withdrawn,omitempty"`
	FixedVersions  []string `json:"fixed_versions,omitempty"`
}

// Query asks about one package version.
type Query struct {
	Ecosystem string // "Go"
	Name      string
	Version   string
}

// Client talks to the OSV API.
type Client struct {
	base string
	http *httpcache.Client
}

// NewClient builds an OSV client.
func NewClient(base, cacheDir string, offline bool) (*Client, error) {
	if base == "" {
		base = OSVURL
	}
	hc, err := httpcache.NewClient(cacheDir, offline)
	if err != nil {
		return nil, err
	}
	return &Client{base: strings.TrimSuffix(base, "/"), http: hc}, nil
}

// QueryBatch resolves advisories for many package versions in one round trip.
// Results are returned positionally to match queries.
//
// OSV's batch endpoint answers with only ids and timestamps, so each unknown
// advisory is hydrated from /v1/vulns/{id}; the HTTP cache makes repeat scans
// nearly free.
func (c *Client) QueryBatch(ctx context.Context, queries []Query) ([][]Advisory, error) {
	out := make([][]Advisory, len(queries))
	if len(queries) == 0 {
		return out, nil
	}

	type pkg struct {
		Ecosystem string `json:"ecosystem"`
		Name      string `json:"name"`
	}
	type q struct {
		Package pkg    `json:"package"`
		Version string `json:"version"`
	}
	body := struct {
		Queries []q `json:"queries"`
	}{}
	for _, query := range queries {
		body.Queries = append(body.Queries, q{
			Package: pkg{Ecosystem: query.Ecosystem, Name: query.Name},
			Version: query.Version,
		})
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	respBody, err := c.http.Post(ctx, c.base+"/v1/querybatch", payload)
	if err != nil {
		return nil, fmt.Errorf("osv querybatch: %w", err)
	}
	var resp struct {
		Results []struct {
			Vulns []struct {
				ID string `json:"id"`
			} `json:"vulns"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(respBody), &resp); err != nil {
		return nil, fmt.Errorf("decode osv querybatch: %w", err)
	}
	if len(resp.Results) != len(queries) {
		return nil, fmt.Errorf("osv returned %d results for %d queries", len(resp.Results), len(queries))
	}

	cache := map[string]Advisory{}
	for i, result := range resp.Results {
		for _, v := range result.Vulns {
			if v.ID == "" {
				continue
			}
			a, ok := cache[v.ID]
			if !ok {
				full, err := c.fetchAdvisory(ctx, v.ID)
				if err != nil {
					// A single hydration failure must not sink the batch; record
					// the id so the caller can still report it.
					full = Advisory{ID: v.ID, Severity: SeverityUnknown}
				}
				cache[v.ID] = full
				a = full
			}
			out[i] = append(out[i], a)
		}
	}
	return out, nil
}

// fetchAdvisory hydrates one record by id.
func (c *Client) fetchAdvisory(ctx context.Context, id string) (Advisory, error) {
	body, err := c.http.Get(ctx, c.base+"/v1/vulns/"+url.PathEscape(id))
	if err != nil {
		return Advisory{}, err
	}
	var raw rawVuln
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return Advisory{}, fmt.Errorf("decode osv %s: %w", id, err)
	}
	a := normalize(raw)
	if a.Severity == SeverityUnknown {
		a.Severity, a.Score, a.SeveritySource = c.escalateFromAliases(ctx, a)
	}
	return a, nil
}

type rawVuln struct {
	ID               string   `json:"id"`
	Aliases          []string `json:"aliases"`
	Summary          string   `json:"summary"`
	Details          string   `json:"details"`
	Published        string   `json:"published"`
	Withdrawn        string   `json:"withdrawn"`
	DatabaseSpecific struct {
		Severity string `json:"severity"`
		Score    string `json:"score"`
	} `json:"database_specific"`
	Severity []struct {
		Type  string `json:"type"`
		Score string `json:"score"`
	} `json:"severity"`
	Affected []struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		Ranges []struct {
			Type   string `json:"type"`
			Events []struct {
				Introduced string `json:"introduced"`
				Fixed      string `json:"fixed"`
			} `json:"events"`
		} `json:"ranges"`
	} `json:"affected"`
}

// normalize maps an OSV record onto our shape, keeping only facts and treating
// prose as bounded untrusted data.
func normalize(raw rawVuln) Advisory {
	a := Advisory{
		ID:        raw.ID,
		Aliases:   raw.Aliases,
		Summary:   sanitize(raw.Summary, 200),
		Detail:    sanitize(raw.Details, maxText),
		Published: raw.Published,
		Withdrawn: raw.Withdrawn,
	}
	a.Severity, a.Score, a.SeveritySource = resolveSeverity(raw)
	seen := map[string]bool{}
	for _, affected := range raw.Affected {
		if !strings.EqualFold(affected.Package.Ecosystem, "Go") &&
			affected.Package.Ecosystem != "" {
			continue
		}
		for _, r := range affected.Ranges {
			for _, ev := range r.Events {
				if ev.Fixed == "" || seen[ev.Fixed] {
					continue
				}
				seen[ev.Fixed] = true
				a.FixedVersions = append(a.FixedVersions, ev.Fixed)
			}
		}
	}
	return a
}

// resolveSeverity determines the label and score for a record, in order of
// trustworthiness: an explicit feed label, then a computable CVSS vector, then
// the representative score for the label. The source is reported so callers can
// tell a computed severity from a declared one.
func resolveSeverity(raw rawVuln) (Severity, float64, string) {
	if label := normalizeSeverity(raw.DatabaseSpecific.Severity); label != SeverityUnknown {
		return label, ScoreFor(label), "database"
	}
	if score, ok := parseScoreString(raw.DatabaseSpecific.Score); ok && score > 0 {
		return SeverityFromScore(score), score, "database"
	}
	// CVSS vectors are the most common source in practice; GHSA-sourced records
	// almost always carry one even when no label is given.
	for _, sv := range raw.Severity {
		if !strings.HasPrefix(strings.ToUpper(sv.Type), "CVSS_V3") {
			continue // v4 uses a different scoring model and is not implemented
		}
		if score, ok := parseScoreString(sv.Score); ok && score > 0 {
			return SeverityFromScore(score), score, "cvss"
		}
	}
	return SeverityUnknown, ScoreFor(SeverityUnknown), "none"
}

// escalateFromAliases consults aliased records when a record carries no severity
// at all. Go advisories usually alias a GHSA/CVE record that does, so this
// recovers a label for the common case without inventing one.
func (c *Client) escalateFromAliases(ctx context.Context, a Advisory) (Severity, float64, string) {
	for _, alias := range a.Aliases {
		if alias == "" || alias == a.ID {
			continue
		}
		body, err := c.http.Get(ctx, c.base+"/v1/vulns/"+url.PathEscape(alias))
		if err != nil {
			continue
		}
		var raw rawVuln
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			continue
		}
		label, score, _ := resolveSeverity(raw)
		if label != SeverityUnknown {
			return label, score, "alias:" + alias
		}
	}
	return SeverityUnknown, ScoreFor(SeverityUnknown), "none"
}

// normalizeSeverity maps feed labels onto our four buckets.
func normalizeSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "crit":
		return SeverityCritical
	case "high":
		return SeverityHigh
	case "moderate", "medium":
		return SeverityMedium
	case "low":
		return SeverityLow
	default:
		return SeverityUnknown
	}
}

// sanitize bounds untrusted text: collapse whitespace, drop control characters,
// and truncate. Markup and links are stripped because they are the vector for
// prompt injection (§11).
func sanitize(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	s = stripMarkup(s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}

// stripMarkup removes angle-bracket tags and markdown links, leaving plain text.
func stripMarkup(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
			continue
		case '>':
			if depth > 0 {
				depth--
				continue
			}
		}
		if depth == 0 {
			b.WriteRune(r)
		}
	}
	out := b.String()
	// Markdown links: [text](url) -> text
	for {
		open := strings.Index(out, "](")
		if open < 0 {
			break
		}
		start := strings.LastIndex(out[:open], "[")
		if start < 0 {
			break
		}
		close := strings.Index(out[open:], ")")
		if close < 0 {
			break
		}
		out = out[:start] + out[start+1:open] + out[open+close+1:]
	}
	return out
}

// Highest reports the newest fixed version at or above current, or "".
func (a Advisory) HighestFixed() string {
	best := ""
	for _, v := range a.FixedVersions {
		if best == "" || v > best {
			best = v
		}
	}
	return best
}
