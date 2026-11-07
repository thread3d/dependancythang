package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thread3d/dependancythang/internal/config"
	"gopkg.in/yaml.v3"
)

// writeConfig writes content to a temp config file and points the loader at it.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(config.EnvConfig, path)
	t.Setenv(config.EnvStateDir, filepath.Join(dir, "state"))
	return path
}

func TestLoadLayersOverDefaults(t *testing.T) {
	writeConfig(t, `
version: 1
defaults:
  max_open_branches: 9
repositories:
  - path: /tmp/example
`)

	cfg, src, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Defaults.MaxOpenBranches, 9; got != want {
		t.Errorf("max_open_branches = %d, want %d", got, want)
	}
	// Unspecified keys must inherit, not become zero values.
	if got, want := cfg.Defaults.Cooldown.MajorDays, 7; got != want {
		t.Errorf("cooldown.major_days = %d, want default %d", got, want)
	}
	if got, want := cfg.Defaults.Verify.Sandbox, "bubblewrap"; got != want {
		t.Errorf("verify.sandbox = %q, want default %q", got, want)
	}
	if len(src.Layers) == 0 {
		t.Error("expected recorded layers")
	}
}

func TestLoadMissingGlobalFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvConfig, filepath.Join(dir, "does-not-exist.yaml"))
	t.Setenv(config.EnvStateDir, filepath.Join(dir, "state"))

	cfg, src, err := config.Load()
	if err != nil {
		t.Fatalf("Load on first run: %v", err)
	}
	if cfg.Version != 1 {
		t.Errorf("version = %d, want 1", cfg.Version)
	}
	if src.GlobalPath != "" {
		t.Errorf("GlobalPath = %q, want empty when no file exists", src.GlobalPath)
	}
}

func TestValidateReportsUnknownKeyWithLine(t *testing.T) {
	doc := `version: 1
defaults:
  max_open_branch: 5
`
	issues := config.Validate([]byte(doc))
	if len(issues) != 1 {
		t.Fatalf("issues = %d (%v), want exactly 1", len(issues), issues)
	}
	if got, want := issues[0].Line, 3; got != want {
		t.Errorf("line = %d, want %d (the misspelled key's line)", got, want)
	}
	if !strings.Contains(issues[0].Message, "unknown key") {
		t.Errorf("message = %q, want it to name an unknown key", issues[0].Message)
	}
}

func TestValidateSemanticRules(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "ignore rule needs a reason",
			doc: `version: 1
updates:
  - ecosystem: gomod
    ignore:
      - dependency: example.com/x
`,
			want: "reason is mandatory",
		},
		{
			name: "unknown forge",
			doc: `version: 1
repositories:
  - path: /tmp/x
    forge: bitbucket
`,
			want: "must be one of",
		},
		{
			name: "bad timezone",
			doc: `version: 1
defaults:
  schedule:
    timezone: Mars/Olympus
`,
			want: "unknown time zone",
		},
		{
			name: "wrong version",
			doc:  "version: 2\n",
			want: "version must be 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := config.Validate([]byte(tc.doc))
			if len(issues) == 0 {
				t.Fatalf("expected a validation issue")
			}
			joined := ""
			for _, i := range issues {
				joined += i.Message + "\n"
			}
			if !strings.Contains(joined, tc.want) {
				t.Errorf("issues = %q, want one containing %q", joined, tc.want)
			}
		})
	}
}

func TestValidateAcceptsSpecExample(t *testing.T) {
	// The configuration example published in the specification must load.
	doc := `version: 1
server:
  state_dir: ~/.local/share/depmcp
  offline: false
  read_only: false
fetch:
  enabled: true
  remote: origin
repositories:
  - path: /tmp/inference-cluster
    forge: local
  - path: /tmp/libmdast
    forge: github
    remote: origin
defaults:
  schedule: { interval: weekly, day: monday, time: "06:00", timezone: Europe/Copenhagen }
  cooldown: { patch_days: 1, minor_days: 3, major_days: 7 }
  max_open_branches: 5
  verify: { enabled: true, timeout: 15m, sandbox: bubblewrap, allow_install_scripts: false, strict_cache: false }
  bisect_max_attempts: 8
  branch_prefix: depmcp
  commit_style: conventional
  auto_pr: false
updates:
  - ecosystem: gomod
    directory: /
    groups:
      golang-x: { patterns: ["golang.org/x/*"] }
    ignore:
      - dependency: github.com/foo/bar
        versions: [">=3.0.0"]
        reason: "v3 drops our Go version"
        until: "2027-03-01"
  - ecosystem: npm
    directory: /web
    allow: [{ dependency_type: production }]
    cooldown: { minor_days: 5 }
security:
  source: osv
  bypass_cooldown: [critical, high]
  govulncheck: true
notify:
  desktop: true
  ntfy: { topic: depmcp-julian }
`
	if issues := config.Validate([]byte(doc)); len(issues) != 0 {
		for _, i := range issues {
			t.Errorf("unexpected issue: %s", i)
		}
	}
}

func TestEditPreservesCommentsAndUnrelatedKeys(t *testing.T) {
	path := writeConfig(t, `# top-of-file note
version: 1
# policy section
defaults:
  max_open_branches: 4   # keep it small
repositories: []
`)

	_, err := config.Edit(path, func(root *yaml.Node) (bool, error) {
		return config.UpsertRepository(root, config.Repository{Path: "/tmp/newrepo", Forge: "github"})
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	text := string(got)
	for _, want := range []string{
		"# top-of-file note",
		"# policy section",
		"# keep it small",
		"max_open_branches: 4",
		"/tmp/newrepo",
		"forge: github",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("edited document lost %q:\n%s", want, text)
		}
	}
}

func TestEditIsIdempotentForIdenticalEntry(t *testing.T) {
	path := writeConfig(t, `version: 1
repositories:
  - path: /tmp/same
    forge: local
`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	changed, err := editChanged(t, path, func(root *yaml.Node) (bool, error) {
		return config.UpsertRepository(root, config.Repository{Path: "/tmp/same", Forge: "local"})
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if changed {
		t.Error("re-adding an identical repository reported a change")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("no-op edit rewrote the file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestEditReadOnlyFile(t *testing.T) {
	path := writeConfig(t, "version: 1\nrepositories: []\n")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_, err := config.Edit(path, func(root *yaml.Node) (bool, error) {
		return config.UpsertRepository(root, config.Repository{Path: "/tmp/x"})
	})
	if err == nil {
		t.Fatal("expected an error writing a read-only config")
	}
	if !errorIs(err, config.ErrReadOnly) {
		t.Errorf("error = %v, want ErrReadOnly", err)
	}
}

func TestEditDetectsExternalChange(t *testing.T) {
	path := writeConfig(t, "version: 1\nrepositories: []\n")

	_, err := config.Edit(path, func(root *yaml.Node) (bool, error) {
		// Simulate a human saving the file while our edit is in flight.
		if err := os.WriteFile(path, []byte("version: 1\nrepositories: []\n# hand edit\n"), 0o644); err != nil {
			return false, err
		}
		return config.UpsertRepository(root, config.Repository{Path: "/tmp/raced"})
	})
	if err == nil {
		t.Fatal("expected a concurrent-edit error")
	}
	if !errorIs(err, config.ErrChangedExternally) {
		t.Errorf("error = %v, want ErrChangedExternally", err)
	}
}

func TestRemoveRepository(t *testing.T) {
	path := writeConfig(t, `version: 1
repositories:
  - path: /tmp/keep
  - path: /tmp/drop
`)
	changed, err := editChanged(t, path, func(root *yaml.Node) (bool, error) {
		return config.RemoveRepository(root, "/tmp/drop")
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !changed {
		t.Error("removal reported no change")
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.Find("/tmp/drop"); ok {
		t.Error("/tmp/drop still tracked")
	}
	if _, ok := cfg.Find("/tmp/keep"); !ok {
		t.Error("/tmp/keep was removed too")
	}
}

func TestRepoIDIsStableAndPathBased(t *testing.T) {
	a := config.RepoID("/tmp/one")
	b := config.RepoID("/tmp/one/") // same path after cleaning
	c := config.RepoID("/tmp/two")
	if a != b {
		t.Errorf("RepoID not stable across equivalent paths: %q vs %q", a, b)
	}
	if a == c {
		t.Error("different paths produced the same id")
	}
	if len(a) != 12 {
		t.Errorf("id length = %d, want 12", len(a))
	}
}

// helpers

func editChanged(t *testing.T, path string, fn func(*yaml.Node) (bool, error)) (bool, error) {
	t.Helper()
	changed := false
	_, err := config.Edit(path, func(root *yaml.Node) (bool, error) {
		c, err := fn(root)
		changed = c
		return c, err
	})
	return changed, err
}

func errorIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
