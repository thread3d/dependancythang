// Package config holds the single source of truth for depmcp's intent:
// which repositories are tracked, what policy applies, and how tools are
// allowed to behave.
//
// Per the specification (§5), YAML is authoritative and the SQLite store is
// derived. Anything a human would want to review, diff, or commit lives here.
package config

// Config is the fully resolved, layered configuration.
type Config struct {
	Version      int            `yaml:"version" json:"version"`
	Server       Server         `yaml:"server" json:"server"`
	Fetch        Fetch          `yaml:"fetch" json:"fetch"`
	Repositories []Repository   `yaml:"repositories" json:"repositories"`
	Defaults     Defaults       `yaml:"defaults" json:"defaults"`
	Updates      []UpdateTarget `yaml:"updates" json:"updates"`
	Security     Security       `yaml:"security" json:"security"`
	Notify       Notify         `yaml:"notify" json:"notify"`
}

// Server holds process-level settings. There is no listen address: Rev 2 has
// no daemon and no socket (§2).
type Server struct {
	StateDir string `yaml:"state_dir" json:"state_dir"`
	Offline  bool   `yaml:"offline" json:"offline"`
	ReadOnly bool   `yaml:"read_only" json:"read_only"`
}

// Fetch controls base-ref freshness (§8). Branching from a stale local default
// branch produces pull requests full of unrelated diffs.
type Fetch struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Remote  string `yaml:"remote" json:"remote"`
}

// Repository is one tracked repository.
type Repository struct {
	Path     string    `yaml:"path" json:"path"`
	Forge    string    `yaml:"forge,omitempty" json:"forge,omitempty"`   // local | github | gitlab | gitea
	Remote   string    `yaml:"remote,omitempty" json:"remote,omitempty"` // empty means Fetch.Remote
	Schedule *Schedule `yaml:"schedule,omitempty" json:"schedule,omitempty"`
}

// Schedule is a cron-ish trigger, always evaluated in an explicit zone.
type Schedule struct {
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty"` // daily | weekly | monthly | cron
	Day      string `yaml:"day,omitempty" json:"day,omitempty"`
	Time     string `yaml:"time,omitempty" json:"time,omitempty"`
	Timezone string `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	Cron     string `yaml:"cron,omitempty" json:"cron,omitempty"`
}

// Defaults are applied to every repository unless overridden.
type Defaults struct {
	Schedule          Schedule `yaml:"schedule" json:"schedule"`
	Cooldown          Cooldown `yaml:"cooldown" json:"cooldown"`
	MaxOpenBranches   int      `yaml:"max_open_branches" json:"max_open_branches"`
	Verify            Verify   `yaml:"verify" json:"verify"`
	BisectMaxAttempts int      `yaml:"bisect_max_attempts" json:"bisect_max_attempts"`
	BranchPrefix      string   `yaml:"branch_prefix" json:"branch_prefix"`
	CommitStyle       string   `yaml:"commit_style" json:"commit_style"`
	AutoPR            bool     `yaml:"auto_pr" json:"auto_pr"`
}

// Cooldown is the minimum release age per update class (§9).
type Cooldown struct {
	PatchDays int `yaml:"patch_days" json:"patch_days"`
	MinorDays int `yaml:"minor_days" json:"minor_days"`
	MajorDays int `yaml:"major_days" json:"major_days"`
}

// Verify configures the Phase B sandbox (§4.1).
type Verify struct {
	Enabled             bool   `yaml:"enabled" json:"enabled"`
	Timeout             string `yaml:"timeout" json:"timeout"`
	Sandbox             string `yaml:"sandbox" json:"sandbox"` // bubblewrap | container | none
	AllowInstallScripts bool   `yaml:"allow_install_scripts" json:"allow_install_scripts"`
	StrictCache         bool   `yaml:"strict_cache" json:"strict_cache"`
}

// UpdateTarget mirrors dependabot.yml's `updates` entries.
type UpdateTarget struct {
	Ecosystem string           `yaml:"ecosystem" json:"ecosystem"`
	Directory string           `yaml:"directory" json:"directory"`
	Groups    map[string]Group `yaml:"groups,omitempty" json:"groups,omitempty"`
	Ignore    []IgnoreRule     `yaml:"ignore,omitempty" json:"ignore,omitempty"`
	Allow     []AllowRule      `yaml:"allow,omitempty" json:"allow,omitempty"`
	Cooldown  *Cooldown        `yaml:"cooldown,omitempty" json:"cooldown,omitempty"`
}

// Group is a named set of patterns updated together.
type Group struct {
	Patterns []string `yaml:"patterns" json:"patterns"`
}

// IgnoreRule suppresses an update. Reason is mandatory (§10).
type IgnoreRule struct {
	Dependency  string   `yaml:"dependency" json:"dependency"`
	Versions    []string `yaml:"versions,omitempty" json:"versions,omitempty"`
	UpdateTypes []string `yaml:"update_types,omitempty" json:"update_types,omitempty"`
	Reason      string   `yaml:"reason" json:"reason"`
	Until       string   `yaml:"until,omitempty" json:"until,omitempty"`
}

// AllowRule narrows what may be updated.
type AllowRule struct {
	DependencyType string `yaml:"dependency_type,omitempty" json:"dependency_type,omitempty"`
	DependencyName string `yaml:"dependency_name,omitempty" json:"dependency_name,omitempty"`
	UpdateType     string `yaml:"update_type,omitempty" json:"update_type,omitempty"`
}

// Security configures advisory handling (§9).
type Security struct {
	Source         string   `yaml:"source" json:"source"`
	BypassCooldown []string `yaml:"bypass_cooldown" json:"bypass_cooldown"`
	Govulncheck    bool     `yaml:"govulncheck" json:"govulncheck"`
}

// Notify configures unattended-run notifications.
type Notify struct {
	Desktop bool  `yaml:"desktop" json:"desktop"`
	Ntfy    *Ntfy `yaml:"ntfy,omitempty" json:"ntfy,omitempty"`
}

// Ntfy is an optional push target.
type Ntfy struct {
	Topic  string `yaml:"topic" json:"topic"`
	Server string `yaml:"server,omitempty" json:"server,omitempty"`
}
