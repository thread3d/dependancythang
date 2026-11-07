package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Environment overrides. They exist so tests and one-off runs never touch a
// developer's real configuration or state directory.
const (
	EnvConfig   = "DEPMCP_CONFIG"
	EnvStateDir = "DEPMCP_STATE_DIR"
)

// Errors surfaced to callers. Tool handlers map these onto the stable codes
// listed in §7 of the specification.
var (
	ErrReadOnly          = errors.New("configuration is read-only")
	ErrChangedExternally = errors.New("configuration changed externally during edit")
)

// Default returns the built-in defaults from §10. Every layer is decoded on top
// of this value, so a missing key inherits rather than becoming a zero value.
func Default() Config {
	return Config{
		Version: 1,
		Server: Server{
			StateDir: "~/.local/share/depmcp",
		},
		Fetch: Fetch{Enabled: true, Remote: "origin"},
		Defaults: Defaults{
			Schedule: Schedule{
				Interval: "weekly",
				Day:      "monday",
				Time:     "06:00",
				Timezone: "UTC",
			},
			Cooldown:          Cooldown{PatchDays: 1, MinorDays: 3, MajorDays: 7},
			MaxOpenBranches:   5,
			BisectMaxAttempts: 8,
			Verify: Verify{
				Enabled: true,
				Timeout: "15m",
				Sandbox: "bubblewrap",
			},
			BranchPrefix: "depmcp",
			CommitStyle:  "conventional",
		},
		Security: Security{
			Source:         "osv",
			BypassCooldown: []string{"critical", "high"},
			Govulncheck:    true,
		},
	}
}

// Sources records which files contributed to a resolved configuration, so that
// "why is this value what it is?" is answerable (§5).
type Sources struct {
	GlobalPath string   // "" when no global config file exists yet
	RepoPath   string   // per-repository .depmcp.yml, when one was merged
	Layers     []string // human-readable load order
}

// GlobalPath returns the global configuration path, honouring DEPMCP_CONFIG.
func GlobalPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv(EnvConfig)); p != "" {
		return ExpandPath(p)
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "depmcp", "config.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "depmcp", "config.yaml"), nil
}

// ExpandPath expands a leading ~ and any environment variables in p.
func ExpandPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	p = os.ExpandEnv(p)
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", fmt.Errorf("resolve %q: %w", p, err)
		}
		p = abs
	}
	return filepath.Clean(p), nil
}

// Load resolves the effective configuration: defaults, then the global file.
func Load() (*Config, *Sources, error) {
	cfg := Default()
	src := &Sources{Layers: []string{"built-in defaults"}}

	globalPath, err := GlobalPath()
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(globalPath)
	switch {
	case err == nil:
		if err := decode(data, &cfg, globalPath); err != nil {
			return nil, nil, err
		}
		src.GlobalPath = globalPath
		src.Layers = append(src.Layers, globalPath)
	case errors.Is(err, os.ErrNotExist):
		// A missing global file is a normal first-run state, not an error.
		src.Layers = append(src.Layers, "(no global config yet: "+globalPath+")")
	default:
		return nil, nil, fmt.Errorf("read %s: %w", globalPath, err)
	}

	if dir := strings.TrimSpace(os.Getenv(EnvStateDir)); dir != "" {
		expanded, err := ExpandPath(dir)
		if err != nil {
			return nil, nil, err
		}
		cfg.Server.StateDir = expanded
		src.Layers = append(src.Layers, "environment "+EnvStateDir)
	}

	if err := cfg.validateSemantics(); err != nil {
		return nil, nil, err
	}
	return &cfg, src, nil
}

// LoadFor resolves one repository's effective configuration by merging its
// committed .depmcp.yml over the global layer (§10: four layers, later wins).
func LoadFor(repoPath string) (*Config, *Sources, error) {
	cfg, src, err := Load()
	if err != nil {
		return nil, nil, err
	}
	repoFile := filepath.Join(repoPath, ".depmcp.yml")
	data, err := os.ReadFile(repoFile)
	switch {
	case err == nil:
		if err := decode(data, cfg, repoFile); err != nil {
			return nil, nil, err
		}
		src.RepoPath = repoFile
		src.Layers = append(src.Layers, repoFile)
	case errors.Is(err, os.ErrNotExist):
		// No per-repo override; global policy applies.
	default:
		return nil, nil, fmt.Errorf("read %s: %w", repoFile, err)
	}
	if err := cfg.validateSemantics(); err != nil {
		return nil, nil, err
	}
	return cfg, src, nil
}

// decode applies one YAML layer on top of cfg with strict field checking.
func decode(data []byte, cfg *Config, name string) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io_EOF) {
			return nil // empty file: nothing to merge
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// RepoID derives the stable identifier used for state rows, locks, worktrees,
// and resource URIs. It must not depend on the config file, because a rename or
// reordering of entries must not orphan a repository's history.
func RepoID(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return hex.EncodeToString(sum[:])[:12]
}

// Hash is a short, stable digest of a configuration fragment, used to notice
// drift between what configuration says and what derived state recorded.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// Find returns the repository entry matching path, if tracked.
func (c *Config) Find(path string) (Repository, bool) {
	clean := filepath.Clean(path)
	for _, r := range c.Repositories {
		if filepath.Clean(r.Path) == clean {
			return r, true
		}
	}
	return Repository{}, false
}

// StateDir returns the resolved state directory.
func (c *Config) StateDir() (string, error) {
	return ExpandPath(c.Server.StateDir)
}

// ForgeFor resolves a repository's forge, defaulting to the local forge, which
// stops at a verified branch (§11).
func (c *Config) ForgeFor(r Repository) string {
	if strings.TrimSpace(r.Forge) == "" {
		return "local"
	}
	return r.Forge
}

// RemoteFor resolves a repository's remote, falling back to the global default.
func (c *Config) RemoteFor(r Repository) string {
	if strings.TrimSpace(r.Remote) == "" {
		if strings.TrimSpace(c.Fetch.Remote) == "" {
			return "origin"
		}
		return c.Fetch.Remote
	}
	return r.Remote
}
