package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/thread3d/dependancythang/internal/faults"
	"github.com/thread3d/dependancythang/internal/version"
)

// journalFor opens the fault journal belonging to a repository.
//
// The journal lives in the repository (`.depmcp/faults/`), not in the state
// directory, for the reason both reference implementations give: a defect in the
// tool outlives the run that exposed it, and the backlog has to be somewhere a
// later session will look while working on the code.
func (s *Service) journalFor(repoRef string) (*faults.Journal, error) {
	path := ""
	if strings.TrimSpace(repoRef) != "" {
		_, repoPath, err := s.repoByID(repoRef)
		if err != nil {
			return nil, err
		}
		path = repoPath
	} else if len(s.Cfg.Repositories) == 1 {
		expanded, err := s.repoByRef(s.Cfg.Repositories[0].Path)
		if err != nil {
			return nil, err
		}
		path = expanded
	} else {
		// With no repository in context the backlog belongs to the state
		// directory: still durable, just not attached to a checkout.
		path = s.stateDir
	}
	return faults.Open(faults.Dir(path))
}

// repoByRef expands a repository reference into an absolute path.
func (s *Service) repoByRef(ref string) (string, error) {
	_, path, err := s.repoByID(ref)
	return path, err
}

// RecordFault appends a defect to a repository's backlog.
//
// It never fails its caller: a defect in the recorder must not become the defect
// being recorded.
func (s *Service) RecordFault(ctx context.Context, repoRef string, f faults.Fault) (faults.Fault, error) {
	journal, err := s.journalFor(repoRef)
	if err != nil {
		return f, err
	}
	if f.Version == "" {
		f.Version = version.Version
	}
	recorded, err := journal.Record(f)
	if err != nil {
		return recorded, err
	}
	_ = s.RecordTool(ctx, "record_fault", map[string]any{
		"id": recorded.ID, "kind": string(recorded.Kind), "repo": repoRef,
	}, nil, "")
	return recorded, nil
}

// ListFaults returns a repository's backlog, unresolved first.
func (s *Service) ListFaults(ctx context.Context, repoRef string, includeResolved bool) ([]faults.Entry, string, error) {
	journal, err := s.journalFor(repoRef)
	if err != nil {
		return nil, "", err
	}
	entries, err := journal.List()
	if err != nil {
		return nil, journal.Dir(), err
	}
	if includeResolved {
		return entries, journal.Dir(), nil
	}
	open := make([]faults.Entry, 0, len(entries))
	for _, e := range entries {
		if !e.Resolved {
			open = append(open, e)
		}
	}
	return open, journal.Dir(), nil
}

// ResolveFault records a resolution event for a fault id.
func (s *Service) ResolveFault(ctx context.Context, repoRef, id, note string) error {
	journal, err := s.journalFor(repoRef)
	if err != nil {
		return err
	}
	if err := journal.Resolve(faults.Resolution{ID: id, Note: note, By: "depmcp"}); err != nil {
		return err
	}
	_ = s.RecordTool(ctx, "resolve_fault", map[string]any{"id": id, "repo": repoRef}, nil, "")
	return nil
}

// RecordUnexpectedError files a tool error that carried no stable code, which is
// how depmcp notices a fault it did not anticipate.
//
// The stable-code set is the taxonomy of things that are *supposed* to go wrong
// (a dirty config, a busy repository, an unavailable sandbox). An error outside
// it means the code took a path nobody planned for, and that is exactly what
// belongs in the backlog.
func (s *Service) RecordUnexpectedError(ctx context.Context, tool string, repoRef string, err error) {
	if err == nil || s == nil {
		return
	}
	code := CodeOf(err)
	if code != "" {
		return
	}
	summary := tool + ": " + firstLine(err.Error())
	_, _ = s.RecordFault(ctx, repoRef, faults.Fault{
		Kind:    faults.KindTool,
		Summary: summary,
		Detail:  err.Error(),
		Command: tool,
		Hint: "An error with no stable code reached a tool boundary. Either it needs a code " +
			"in the documented set (if it is a legitimate failure) or the path it took is a bug. " +
			"Reproduce with " + tool + " and check where the error was constructed.",
	})
}

// firstLine shortens an error for use as a fingerprint summary.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// FaultJournalPath reports where the backlog for a repository lives, for status.
func (s *Service) FaultJournalPath(repoRef string) string {
	journal, err := s.journalFor(repoRef)
	if err != nil {
		return ""
	}
	return filepath.Join(journal.Dir())
}

// ensure os stays referenced for the state-dir fallback path above.
var _ = os.Stat
