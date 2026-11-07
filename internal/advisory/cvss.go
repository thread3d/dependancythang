package advisory

import (
	"math"
	"strconv"
	"strings"
)

// CVSS base-score computation, so an advisory that publishes only a vector still
// gets a severity label instead of "unknown".
//
// CVSS v3.0 and v3.1 share a formula and differ only in rounding: v3.0 rounds to
// one decimal, v3.1 rounds up. Both are implemented; v4.0 is not, because its
// scoring model is a different (and much larger) system — records carrying only a
// v4 vector fall through to the alias lookup and then to unknown.
//
// Reference: FIRST CVSS v3.1 specification, section 7.1 (base equations).

// metric weights, per the specification.
var (
	cvssAV  = map[string]float64{"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.2}
	cvssAC  = map[string]float64{"L": 0.77, "H": 0.44}
	cvssUI  = map[string]float64{"N": 0.85, "R": 0.62}
	cvssCIA = map[string]float64{"H": 0.56, "L": 0.22, "N": 0}
	// PrivilegesRequired depends on Scope: unchanged vs changed.
	cvssPRUnchanged = map[string]float64{"N": 0.85, "L": 0.62, "H": 0.27}
	cvssPRChanged   = map[string]float64{"N": 0.85, "L": 0.68, "H": 0.5}
)

// ParseCVSSScore computes the base score for a CVSS v3.0/v3.1 vector string.
// The bool reports whether the vector was understood; a partially specified
// vector is rejected rather than guessed at.
func ParseCVSSScore(vector string) (float64, bool) {
	if !strings.HasPrefix(vector, "CVSS:3.") {
		return 0, false
	}
	metrics := map[string]string{}
	for _, part := range strings.Split(vector, "/") {
		key, value, ok := strings.Cut(part, ":")
		if !ok {
			continue // the leading "CVSS:3.1" element has the same shape and is kept
		}
		metrics[strings.ToUpper(key)] = strings.ToUpper(value)
	}

	// The leading element names the version; it is not a metric.
	version := strings.TrimPrefix(strings.ToUpper(strings.SplitN(vector, "/", 2)[0]), "CVSS:")

	av, ok1 := cvssAV[metrics["AV"]]
	ac, ok2 := cvssAC[metrics["AC"]]
	ui, ok3 := cvssUI[metrics["UI"]]
	c, ok4 := cvssCIA[metrics["C"]]
	integ, ok5 := cvssCIA[metrics["I"]]
	a, ok6 := cvssCIA[metrics["A"]]
	scope := metrics["S"]
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6) || (scope != "U" && scope != "C") {
		return 0, false
	}

	prTable := cvssPRUnchanged
	if scope == "C" {
		prTable = cvssPRChanged
	}
	pr, ok := prTable[metrics["PR"]]
	if !ok {
		return 0, false
	}

	iss := 1 - ((1 - c) * (1 - integ) * (1 - a))
	var impact float64
	if scope == "U" {
		impact = 6.42 * iss
	} else {
		impact = 7.52*(iss-0.029) - 3.25*math.Pow(iss-0.02, 15)
	}
	if impact <= 0 {
		return 0, true
	}

	exploitability := 8.22 * av * ac * pr * ui
	var base float64
	if scope == "U" {
		base = math.Min(impact+exploitability, 10)
	} else {
		base = math.Min(1.08*(impact+exploitability), 10)
	}

	if strings.HasPrefix(version, "CVSS:3.1") || strings.Contains(version, "3.1") {
		return cvssRoundUp(base), true
	}
	// v3.0 rounds half-up to one decimal.
	return math.Round(base*10) / 10, true
}

// cvssRoundUp implements the specification's Roundup: the smallest one-decimal
// number greater than or equal to x. The epsilon guards against binary
// floating-point noise turning 4.3 into 4.300000001 and then into 4.4.
func cvssRoundUp(x float64) float64 {
	scaled := x * 10
	if float64(int(scaled)) == scaled {
		return scaled / 10
	}
	return math.Ceil(scaled-1e-9) / 10
}

// SeverityFromScore maps a numeric base score onto a label, using the v3 ranges.
func SeverityFromScore(score float64) Severity {
	switch {
	case score >= 9.0:
		return SeverityCritical
	case score >= 7.0:
		return SeverityHigh
	case score >= 4.0:
		return SeverityMedium
	case score > 0:
		return SeverityLow
	case score == 0:
		return SeverityNone
	default:
		return SeverityUnknown
	}
}

// ScoreFor returns the representative numeric score for a severity label.
//
// "unknown" is deliberately given a defined score rather than a missing value:
// callers sort, threshold, and decide on scores, and a hole in that space is
// worse than an explicit floor. It scores below "low", which keeps it out of
// any cooldown bypass — an advisory nobody has scored must not be fast-tracked.
func ScoreFor(s Severity) float64 {
	switch s {
	case SeverityCritical:
		return 9.0
	case SeverityHigh:
		return 7.0
	case SeverityMedium:
		return 4.0
	case SeverityLow:
		return 0.1
	default:
		return 0
	}
}

// SeverityNone is the label for a scored advisory with no impact (0.0).
const SeverityNone Severity = "none"

// Rank orders severities for sorting and policy decisions. Unknown ranks below
// low so it never triggers a bypass, but it is still a real rank, not a gap.
func (s Severity) Rank() int {
	switch s {
	case SeverityCritical:
		return 4
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	case SeverityNone:
		return 0
	default: // unknown
		return -1
	}
}

// WithScore returns the severity for score, and the score itself when the label
// was already known and carried no number.
func WithScore(s Severity, score float64) (Severity, float64) {
	if score > 0 {
		return SeverityFromScore(score), score
	}
	return s, ScoreFor(s)
}

// parseScoreString accepts either a bare number ("7.5") or a CVSS vector.
func parseScoreString(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if strings.HasPrefix(s, "CVSS:") {
		return ParseCVSSScore(s)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
