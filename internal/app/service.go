// Package app wires configuration, state, and auditing together for both the
// CLI and the MCP server. Everything a tool does goes through Service, so the
// authority rules from §5 are enforced in exactly one place.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/thread3d/dependancythang/internal/audit"
	"github.com/thread3d/dependancythang/internal/config"
	"github.com/thread3d/dependancythang/internal/repolock"
	"github.com/thread3d/dependancythang/internal/store"
	"gopkg.in/yaml.v3"
)

// Stable error codes from §7. Only the ones M0 can produce are listed.
const (
	CodeConfigInvalid     = "CONFIG_INVALID"
	CodeConfigReadOnly    = "CONFIG_READONLY"
	CodeChangedExternally = "CONFIG_CHANGED_EXTERNALLY"
	CodeNotAGitRepo       = "NOT_A_GIT_REPO"
	CodeRepoBusy          = "REPO_BUSY"
	CodePathOutsideRoot   = "PATH_OUTSIDE_ROOT"
)

// CodedError carries a stable code alongside the message so tool handlers do not
// have to re-derive it.
type CodedError struct {
	Code string
	Err  error
}

func (e *CodedError) Error() string { return e.Err.Error() }
func (e *CodedError) Unwrap() error { return e.Err }

func coded(code string, err error) error {
	if err == nil {
		return nil
	}
	return &CodedError{Code: code, Err: err}
}

// CodeOf extracts the stable code for an error, if it has one.
func CodeOf(err error) string {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// Service is an initialised depmcp runtime.
type Service struct {
	ConfigPath string
	Cfg        *config.Config
	Sources    *config.Sources
	Store      *store.Store
	Audit      *audit.Log
	stateDir   string
}

// Open loads configuration, prepares the state directory, opens the derived
// store, reconciles it against configuration, and returns a Service.
func Open(ctx context.Context) (*Service, error) {
	cfg, src, err := config.Load()
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			return nil, coded(CodeConfigInvalid, err)
		}
		return nil, err
	}
	stateDir, err := cfg.StateDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}

	st, err := store.Open(stateDir)
	if err != nil {
		return nil, err
	}
	al, err := audit.Open(stateDir)
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	s := &Service{
		ConfigPath: src.GlobalPath,
		Cfg:        cfg,
		Sources:    src,
		Store:      st,
		Audit:      al,
		stateDir:   stateDir,
	}
	if s.ConfigPath == "" {
		path, err := config.GlobalPath()
		if err != nil {
			_ = st.Close()
			return nil, err
		}
		s.ConfigPath = path
	}
	if err := s.reconcile(ctx); err != nil {
		_ = st.Close()
		return nil, err
	}
	return s, nil
}

// Close releases resources.
func (s *Service) Close() error { return s.Store.Close() }

// StateDir returns the resolved state directory.
func (s *Service) StateDir() string { return s.stateDir }

// ReadOnly reports whether the global kill switch is on.
func (s *Service) ReadOnly() bool { return s.Cfg.Server.ReadOnly }

// reconcile rebuilds derived state from configuration.
func (s *Service) reconcile(ctx context.Context) error {
	refs := make([]store.RepoRef, 0, len(s.Cfg.Repositories))
	for _, r := range s.Cfg.Repositories {
		expanded, err := config.ExpandPath(r.Path)
		if err != nil {
			return err
		}
		refs = append(refs, store.RepoRef{
			ID:   config.RepoID(expanded),
			Path: expanded,
			Hash: config.Hash(cfgBytes(r)),
		})
	}
	return s.Store.Reconcile(ctx, refs)
}

func cfgBytes(r config.Repository) []byte {
	return []byte(fmt.Sprintf("%s|%s|%s", r.Path, r.Forge, r.Remote))
}

// RepoView is one repository as tools report it.
type RepoView struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Forge    string `json:"forge"`
	Remote   string `json:"remote"`
	Orphaned bool   `json:"orphaned,omitempty"`
	LastScan string `json:"last_scan,omitempty"`
}

// ListRepositories returns tracked repositories with their derived state.
func (s *Service) ListRepositories(ctx context.Context) ([]RepoView, error) {
	states, err := s.Store.States(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]store.RepoState, len(states))
	for _, st := range states {
		byID[st.RepoID] = st
	}

	out := make([]RepoView, 0, len(s.Cfg.Repositories))
	for _, r := range s.Cfg.Repositories {
		expanded, err := config.ExpandPath(r.Path)
		if err != nil {
			return nil, err
		}
		id := config.RepoID(expanded)
		v := RepoView{
			ID:     id,
			Path:   expanded,
			Forge:  s.Cfg.ForgeFor(r),
			Remote: s.Cfg.RemoteFor(r),
		}
		if st, ok := byID[id]; ok && st.LastScanAt != nil {
			v.LastScan = time.Unix(*st.LastScanAt, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, v)
	}
	return out, nil
}

// AddRepositoryRequest is the input for registering a repository.
type AddRepositoryRequest struct {
	Path   string `json:"path"`
	Forge  string `json:"forge,omitempty"`
	Remote string `json:"remote,omitempty"`
}

// AddRepository records a repository in YAML, then reconciles state. The edit is
// the only authoritative act; the state row is a consequence.
func (s *Service) AddRepository(ctx context.Context, req AddRepositoryRequest) (RepoView, error) {
	if s.ReadOnly() {
		return RepoView{}, coded(CodeConfigReadOnly, errors.New("server.read_only is true; refusing to write configuration"))
	}
	expanded, err := config.ExpandPath(req.Path)
	if err != nil {
		return RepoView{}, err
	}
	if expanded == "" {
		return RepoView{}, fmt.Errorf("path is required")
	}
	if err := requireGitRepo(expanded); err != nil {
		return RepoView{}, coded(CodeNotAGitRepo, err)
	}
	if _, err := config.Edit(s.ConfigPath, func(root *yaml.Node) (bool, error) {
		return config.UpsertRepository(root, config.Repository{
			Path:   expanded,
			Forge:  req.Forge,
			Remote: req.Remote,
		})
	}); err != nil {
		return RepoView{}, mapConfigError(err)
	}
	if err := s.Reload(ctx); err != nil {
		return RepoView{}, err
	}
	views, err := s.ListRepositories(ctx)
	if err != nil {
		return RepoView{}, err
	}
	for _, v := range views {
		if v.Path == expanded {
			return v, nil
		}
	}
	return RepoView{}, fmt.Errorf("repository %s was written but not found after reload", expanded)
}

// RemoveRepository unregisters a repository. Files and branches are untouched.
func (s *Service) RemoveRepository(ctx context.Context, path string) (bool, error) {
	if s.ReadOnly() {
		return false, coded(CodeConfigReadOnly, errors.New("server.read_only is true; refusing to write configuration"))
	}
	changed, err := editBool(s.ConfigPath, func(root *yaml.Node) (bool, error) {
		return config.RemoveRepository(root, path)
	})
	if err != nil {
		return false, mapConfigError(err)
	}
	if err := s.Reload(ctx); err != nil {
		return changed, err
	}
	return changed, nil
}

// Reload re-reads configuration from disk and reconciles derived state.
func (s *Service) Reload(ctx context.Context) error {
	cfg, src, err := config.Load()
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			return coded(CodeConfigInvalid, err)
		}
		return err
	}
	s.Cfg, s.Sources = cfg, src
	if src.GlobalPath != "" {
		s.ConfigPath = src.GlobalPath
	}
	return s.reconcile(ctx)
}

func mapConfigError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, config.ErrReadOnly):
		return coded(CodeConfigReadOnly, err)
	case errors.Is(err, config.ErrChangedExternally):
		return coded(CodeChangedExternally, err)
	default:
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			return coded(CodeConfigInvalid, err)
		}
		return err
	}
}

// Lock takes the per-repository write lock (§12).
func (s *Service) Lock(repoID string) (*repolock.Lock, error) {
	l, err := repolock.Acquire(s.stateDir, repoID)
	if err != nil {
		if errors.Is(err, repolock.ErrBusy) {
			return nil, coded(CodeRepoBusy, err)
		}
		return nil, err
	}
	return l, nil
}

// requireGitRepo verifies a path is a git working tree or bare repository.
// Without git there is no worktree, so no safe place to write.
func requireGitRepo(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	gitPath := filepath.Join(path, ".git")
	if _, err := os.Stat(gitPath); err != nil {
		return fmt.Errorf("%s is not a git repository (no .git)", path)
	}
	return nil
}

// ---- auditing -----------------------------------------------------------

// RecordTool writes one audit line and its queryable mirror (§11). The JSONL
// file is the record; the table is an index over it.
func (s *Service) RecordTool(ctx context.Context, tool string, args map[string]any, err error, code string) error {
	rec := audit.Record{Event: "tool.call", Tool: tool, Args: args, Code: code, Result: "ok"}
	if err != nil {
		rec.Result = "error"
		rec.Error = err.Error()
	}
	detail := ""
	if len(args) > 0 {
		if b, mErr := json.Marshal(args); mErr == nil {
			detail = string(b)
		}
	}
	if wErr := s.Audit.Write(rec); wErr != nil {
		return wErr
	}
	return s.Store.AppendAudit(ctx, rec.At, rec.Event, rec.Tool, detail, rec.Result)
}

// LockHolderInfo identifies a repository lock currently held by a process.
type LockHolderInfo struct {
	RepoID string `json:"repo_id"`
	PID    int    `json:"pid"`
}

// HeldLocks reports locks held by other processes, for status output.
func (s *Service) HeldLocks() []LockHolderInfo {
	var out []LockHolderInfo
	for _, r := range s.Cfg.Repositories {
		expanded, err := config.ExpandPath(r.Path)
		if err != nil {
			continue
		}
		id := config.RepoID(expanded)
		if h, held := repolock.Inspect(s.stateDir, id); held {
			out = append(out, LockHolderInfo{RepoID: id, PID: h.PID})
		}
	}
	return out
}

// ---- doctor -------------------------------------------------------------

// Check is one doctor result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail,omitempty"`
}

// Doctor inspects the environment and reports what is and is not ready (§12).
func (s *Service) Doctor(ctx context.Context) []Check {
	var checks []Check
	add := func(name, status, detail string) {
		checks = append(checks, Check{Name: name, Status: status, Detail: detail})
	}

	add("config", "ok", fmt.Sprintf("%d repositories, layers: %s",
		len(s.Cfg.Repositories), strings.Join(s.Sources.Layers, " -> ")))

	if s.ReadOnly() {
		add("read_only", "warn", "server.read_only is true: every write tool will refuse")
	} else {
		add("read_only", "ok", "writes enabled")
	}

	if err := checkWritable(s.stateDir); err != nil {
		add("state_dir", "fail", fmt.Sprintf("%s: %v", s.stateDir, err))
	} else {
		add("state_dir", "ok", s.stateDir)
	}

	stats, err := s.Store.Stats(ctx)
	if err != nil {
		add("store", "fail", err.Error())
	} else {
		add("store", "ok", fmt.Sprintf("%d repos, %d orphans, schema %d", stats.Repos, stats.Orphans, store.SchemaVersion))
	}

	if out, err := exec.CommandContext(ctx, "git", "--version").Output(); err != nil {
		add("git", "fail", "git not found; worktrees are impossible")
	} else {
		add("git", "ok", strings.TrimSpace(string(out)))
	}

	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("bwrap"); err != nil {
			add("sandbox", "warn", "bwrap not found; verify will fail with SANDBOX_UNAVAILABLE unless a container runtime is configured")
		} else {
			add("sandbox", "ok", "bubblewrap available (Phase B)")
		}
	default:
		add("sandbox", "warn", "no bubblewrap on this platform; container runtime required for verify")
	}

	for _, cli := range []string{"go", "npm", "pnpm"} {
		if p, err := exec.LookPath(cli); err != nil {
			add("cli:"+cli, "warn", "not found")
		} else {
			add("cli:"+cli, "ok", p)
		}
	}

	if out, err := exec.CommandContext(ctx, "git", "config", "--get", "commit.gpgsign").Output(); err == nil &&
		strings.TrimSpace(string(out)) == "true" {
		add("signing", "warn", "commit.gpgsign is on; non-interactive runs will fail with SIGNING_FAILED if gpg cannot prompt")
	} else {
		add("signing", "ok", "no forced commit signing")
	}

	// Keyring is only needed from M6 (forge tokens); report the prerequisite
	// rather than pretending to test it.
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		add("keyring", "ok", "session bus present")
	} else {
		add("keyring", "warn", "no DBUS session bus; OS keyring unavailable (needed for forge tokens in M6)")
	}

	return checks
}

func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// ---- small helpers ------------------------------------------------------

// editBool adapts config.Edit to callers that only need the changed flag.
func editBool(path string, mutation func(root *yaml.Node) (bool, error)) (bool, error) {
	changed := false
	_, err := config.Edit(path, func(root *yaml.Node) (bool, error) {
		c, err := mutation(root)
		changed = c
		return c, err
	})
	return changed, err
}
