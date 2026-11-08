package faults_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thread3d/dependancythang/internal/faults"
)

func newJournal(t *testing.T) *faults.Journal {
	t.Helper()
	j, err := faults.Open(faults.Dir(t.TempDir()))
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	return j
}

func TestFingerprintIsStableAndDiscriminating(t *testing.T) {
	a := faults.Fingerprint(faults.KindTool, "plan: boom", "detail one", "")
	b := faults.Fingerprint(faults.KindTool, "plan: boom", "detail two", "")
	if a != b {
		t.Errorf("the same fault got two ids: %q vs %q (detail must not change the fingerprint)", a, b)
	}
	c := faults.Fingerprint(faults.KindInternal, "plan: boom", "detail one", "")
	if a == c {
		t.Error("different kinds share an id")
	}
	if !strings.HasPrefix(a, "DEP-") {
		t.Errorf("id = %q, want a DEP- prefix", a)
	}
}

func TestRecordDedupesByFingerprint(t *testing.T) {
	j := newJournal(t)
	for i := 0; i < 3; i++ {
		// A changing detail (a run id, a temp path) must not create new entries.
		if _, err := j.Record(faults.Fault{
			Kind: faults.KindTool, Summary: "check_updates: registry said no",
			Detail: "attempt " + string(rune('A'+i)),
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	entries, err := j.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (one fault, three sightings)", len(entries))
	}
	if entries[0].Count != 3 {
		t.Errorf("count = %d, want 3", entries[0].Count)
	}
	if entries[0].FirstSeen == "" || entries[0].LastSeen == "" {
		t.Error("first/last seen were not recorded")
	}
}

func TestResolutionIsAnEventAndRecurrenceReopens(t *testing.T) {
	j := newJournal(t)
	rec, err := j.Record(faults.Fault{Kind: faults.KindInternal, Summary: "panic in scan"})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := j.Resolve(faults.Resolution{ID: rec.ID, Note: "fixed the nil deref"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	entries, _ := j.List()
	if len(entries) != 1 || !entries[0].Resolved {
		t.Fatalf("after resolving: %+v", entries)
	}
	if entries[0].Resolution != "fixed the nil deref" {
		t.Errorf("resolution note lost: %q", entries[0].Resolution)
	}

	// The same fault seen again is a regression, so it reopens.
	if _, err := j.Record(faults.Fault{Kind: faults.KindInternal, Summary: "panic in scan"}); err != nil {
		t.Fatalf("Record after resolve: %v", err)
	}
	entries, _ = j.List()
	if entries[0].Resolved {
		t.Error("a fault seen after its resolution stayed resolved; that hides a regression")
	}
	if entries[0].Count != 2 {
		t.Errorf("count = %d, want 2", entries[0].Count)
	}
}

func TestDigestIsRegenerated(t *testing.T) {
	j := newJournal(t)
	rec, err := j.Record(faults.Fault{
		Kind: faults.KindGap, Summary: "yarn berry unsupported",
		Hint: "implement the berry lockfile reader", Command: "scan_dependencies",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(j.Dir(), "FAULTS.md"))
	if err != nil {
		t.Fatalf("read digest: %v", err)
	}
	for _, want := range []string{rec.ID, "yarn berry unsupported", "implement the berry lockfile reader", "1 open"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("digest missing %q:\n%s", want, data)
		}
	}

	if err := j.Resolve(faults.Resolution{ID: rec.ID, Note: "done"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(j.Dir(), "FAULTS.md"))
	if !strings.Contains(string(data), "Resolved") {
		t.Errorf("digest was not regenerated after a resolution:\n%s", data)
	}
}

func TestOpenEntriesSortBeforeResolvedAndBugsFirst(t *testing.T) {
	j := newJournal(t)
	if _, err := j.Record(faults.Fault{Kind: faults.KindEnv, Summary: "no keyring"}); err != nil {
		t.Fatal(err)
	}
	bug, err := j.Record(faults.Fault{Kind: faults.KindInternal, Summary: "index out of range"})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Resolve(faults.Resolution{ID: bug.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Record(faults.Fault{Kind: faults.KindFriction, Summary: "message unclear"}); err != nil {
		t.Fatal(err)
	}

	entries, _ := j.List()
	if len(entries) != 3 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[len(entries)-1].ID != bug.ID {
		t.Error("the resolved entry did not sort last")
	}
	if entries[0].Resolved {
		t.Error("an open entry sorted behind a resolved one")
	}
}

func TestCorruptLineDoesNotHideTheRest(t *testing.T) {
	j := newJournal(t)
	if _, err := j.Record(faults.Fault{Kind: faults.KindTool, Summary: "first"}); err != nil {
		t.Fatal(err)
	}
	// A hand-edit that broke one line must not erase the backlog.
	path := filepath.Join(j.Dir(), "issues.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{not json\n")
	_ = f.Close()

	if _, err := j.Record(faults.Fault{Kind: faults.KindTool, Summary: "second"}); err != nil {
		t.Fatal(err)
	}
	entries, err := j.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("entries = %d, want 2 (the corrupt line is skipped, not fatal)", len(entries))
	}
}

func TestRecordsAreJSONLinesWithTheDocumentedFields(t *testing.T) {
	j := newJournal(t)
	rec, err := j.Record(faults.Fault{
		Kind: faults.KindTool, Summary: "apply failed", Detail: "git said no",
		Hint: "check the worktree", Command: "apply_plan", Repo: "/tmp/repo",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if rec.Severity != faults.SevBug {
		t.Errorf("severity = %q, want the bug default for a tool failure", rec.Severity)
	}
	if rec.GoVersion == "" || rec.Platform == "" {
		t.Error("environment fields were not filled in")
	}

	data, err := os.ReadFile(filepath.Join(j.Dir(), "issues.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("record is not JSON: %v", err)
	}
	for _, key := range []string{"id", "time", "kind", "severity", "summary", "detail", "hint", "command", "repo"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("record is missing %q", key)
		}
	}
}

func TestNilJournalIsSafe(t *testing.T) {
	var j *faults.Journal
	if _, err := j.Record(faults.Fault{Summary: "x"}); err == nil {
		t.Error("recording into a nil journal should report an error, not panic")
	}
	if _, err := j.List(); err == nil {
		t.Error("listing a nil journal should report an error, not panic")
	}
}
