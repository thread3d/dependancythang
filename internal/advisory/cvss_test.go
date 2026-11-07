package advisory_test

import (
	"testing"

	"github.com/thread3d/dependancythang/internal/advisory"
)

// Known-good vectors from the FIRST specification's own examples and widely
// published reference scores.
func TestParseCVSSScore(t *testing.T) {
	cases := []struct {
		name   string
		vector string
		want   float64
		label  advisory.Severity
	}{
		{
			name:   "network, no privileges, full impact on unchanged scope",
			vector: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
			want:   9.8,
			label:  advisory.SeverityCritical,
		},
		{
			name:   "network, no privileges, full impact, changed scope",
			vector: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H",
			want:   10.0,
			label:  advisory.SeverityCritical,
		},
		{
			name:   "confidentiality only",
			vector: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:N",
			want:   7.5,
			label:  advisory.SeverityHigh,
		},
		{
			name:   "low privileges, low impact",
			vector: "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:L/I:N/A:N",
			want:   4.3,
			label:  advisory.SeverityMedium,
		},
		{
			// impact 5.873 + exploitability 1.620 = 7.493, rounded up to 7.5
			name:   "high complexity with user interaction",
			vector: "CVSS:3.1/AV:N/AC:H/PR:N/UI:R/S:U/C:H/I:H/A:H",
			want:   7.5,
			label:  advisory.SeverityHigh,
		},
		{
			name:   "no impact at all scores zero",
			vector: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:N",
			want:   0,
			label:  advisory.SeverityNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := advisory.ParseCVSSScore(tc.vector)
			if !ok {
				t.Fatalf("vector %s was not understood", tc.vector)
			}
			if got != tc.want {
				t.Errorf("score = %v, want %v", got, tc.want)
			}
			if label := advisory.SeverityFromScore(got); label != tc.label {
				t.Errorf("label = %q, want %q", label, tc.label)
			}
		})
	}
}

func TestParseCVSSScoreRejectsNonsense(t *testing.T) {
	for _, bad := range []string{
		"",
		"CVSS:4.0/AV:N/AC:L", // v4 is a different model
		"CVSS:3.1/AV:N/AC:L", // missing metrics
		"CVSS:3.1/AV:Q/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", // invalid metric value
		"not a vector",
	} {
		if score, ok := advisory.ParseCVSSScore(bad); ok {
			t.Errorf("%q was accepted with score %v; want rejection", bad, score)
		}
	}
}

func TestUnknownSeverityHasAScore(t *testing.T) {
	// Every label must carry a defined score, including unknown, so callers can
	// sort and threshold without special cases.
	labels := []advisory.Severity{
		advisory.SeverityCritical, advisory.SeverityHigh, advisory.SeverityMedium,
		advisory.SeverityLow, advisory.SeverityNone, advisory.SeverityUnknown,
	}
	for _, s := range labels {
		score := advisory.ScoreFor(s)
		if score < 0 || score > 10 {
			t.Errorf("ScoreFor(%q) = %v, want 0..10", s, score)
		}
	}
	if advisory.ScoreFor(advisory.SeverityCritical) <= advisory.ScoreFor(advisory.SeverityLow) {
		t.Error("critical must score above low")
	}
	// Unknown must not outrank low: an unscored advisory is never fast-tracked.
	if advisory.ScoreFor(advisory.SeverityUnknown) >= advisory.ScoreFor(advisory.SeverityLow) {
		t.Error("unknown must not score at or above low, or it could bypass cooldown")
	}
	if advisory.SeverityUnknown.Rank() >= advisory.SeverityLow.Rank() {
		t.Error("unknown must rank below low")
	}
}

func TestWithScoreFillsInMissingNumbers(t *testing.T) {
	// A known label with no number gains its representative score.
	label, score := advisory.WithScore(advisory.SeverityHigh, 0)
	if label != advisory.SeverityHigh || score != 7.0 {
		t.Errorf("WithScore(high, 0) = %q,%v; want high,7", label, score)
	}
	// A number overrides the label.
	label, score = advisory.WithScore(advisory.SeverityUnknown, 9.1)
	if label != advisory.SeverityCritical || score != 9.1 {
		t.Errorf("WithScore(unknown, 9.1) = %q,%v; want critical,9.1", label, score)
	}
}
