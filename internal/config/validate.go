package config

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// io_EOF avoids importing io purely for one sentinel in load.go.
var io_EOF = io.EOF

// Issue is one validation problem, located in the YAML source when possible.
type Issue struct {
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

func (i Issue) String() string {
	if i.Line > 0 {
		return fmt.Sprintf("line %d, column %d: %s", i.Line, i.Column, i.Message)
	}
	return i.Message
}

// ValidationError collects every issue found in one pass.
type ValidationError struct{ Issues []Issue }

func (e *ValidationError) Error() string {
	if len(e.Issues) == 1 {
		return "invalid configuration: " + e.Issues[0].String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d issues):", len(e.Issues))
	for _, i := range e.Issues {
		b.WriteString("\n  - ")
		b.WriteString(i.String())
	}
	return b.String()
}

// Validate checks a candidate document without saving it (§7 validate_config).
// Unknown keys are reported with line numbers: strict decoding alone gives the
// field name but not where it came from.
func Validate(data []byte) []Issue {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return []Issue{{Message: "yaml: " + err.Error()}}
	}
	if len(doc.Content) == 0 {
		return nil // empty document means "all defaults"
	}

	var issues []Issue
	issues = append(issues, unknownKeys(doc.Content[0], reflect.TypeOf(Config{}), "")...)

	// Strict decode on top of defaults catches type errors the key walk cannot.
	cfg := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			for _, msg := range te.Errors {
				// Unknown keys were already reported, with a line number, by the
				// node walk above; the decoder only repeats them without one.
				if strings.Contains(msg, "not found in type") {
					continue
				}
				issues = append(issues, Issue{Message: msg, Line: lineFromYAMLError(msg)})
			}
		} else if len(issues) == 0 {
			issues = append(issues, Issue{Message: err.Error(), Line: lineFromYAMLError(err.Error())})
		}
	}
	if len(issues) == 0 {
		if err := cfg.validateSemantics(); err != nil {
			var ve *ValidationError
			if errors.As(err, &ve) {
				issues = append(issues, ve.Issues...)
			} else {
				issues = append(issues, Issue{Message: err.Error()})
			}
		}
	}
	return issues
}

var lineRE = regexp.MustCompile(`line (\d+)`)

func lineFromYAMLError(msg string) int {
	if m := lineRE.FindStringSubmatch(msg); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	return 0
}

// fieldIndex caches, per struct type, a map of yaml key to field type.
var fieldIndex sync.Map // reflect.Type -> map[string]reflect.Type

func fieldsOf(t reflect.Type) map[string]reflect.Type {
	if cached, ok := fieldIndex.Load(t); ok {
		return cached.(map[string]reflect.Type)
	}
	fields := map[string]reflect.Type{}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		fieldIndex.Store(t, fields)
		return fields
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		tag := f.Tag.Get("yaml")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			name = strings.ToLower(f.Name)
		}
		fields[name] = f.Type
	}
	fieldIndex.Store(t, fields)
	return fields
}

// unknownKeys walks a YAML node tree alongside its Go type and reports keys the
// type does not accept, with their line numbers.
func unknownKeys(n *yaml.Node, t reflect.Type, path string) []Issue {
	if n == nil {
		return nil
	}
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	var issues []Issue

	switch n.Kind {
	case yaml.MappingNode:
		if t.Kind() != reflect.Struct {
			return nil
		}
		fields := fieldsOf(t)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			ft, ok := fields[k.Value]
			if !ok {
				issues = append(issues, Issue{
					Line: k.Line, Column: k.Column,
					Path:    path,
					Message: fmt.Sprintf("unknown key %q (not a field of %s)", k.Value, t.Name()),
				})
				continue
			}
			issues = append(issues, unknownKeys(v, ft, path+"."+k.Value)...)
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			issues = append(issues, unknownKeys(item, t, path)...)
		}
	}
	return issues
}

var (
	allowedForges     = []string{"local", "github", "gitlab", "gitea", "forgejo"}
	allowedIntervals  = []string{"daily", "weekly", "monthly", "cron"}
	allowedSandboxes  = []string{"bubblewrap", "container", "none", ""}
	allowedUpdateType = []string{"patch", "minor", "major", "digest", "pin", "security"}
)

// validateSemantics enforces the rules from §10 that a schema cannot express.
func (c *Config) validateSemantics() error {
	var issues []Issue

	if c.Version != 1 {
		issues = append(issues, Issue{Message: fmt.Sprintf("version must be 1, got %d", c.Version)})
	}

	seen := map[string]bool{}
	for idx, r := range c.Repositories {
		where := fmt.Sprintf("repositories[%d]", idx)
		if strings.TrimSpace(r.Path) == "" {
			issues = append(issues, Issue{Path: where, Message: "path is required"})
			continue
		}
		expanded, err := ExpandPath(r.Path)
		if err != nil {
			issues = append(issues, Issue{Path: where, Message: err.Error()})
			continue
		}
		if !filepath.IsAbs(expanded) {
			issues = append(issues, Issue{Path: where, Message: "path must be absolute after expansion"})
		}
		if seen[expanded] {
			issues = append(issues, Issue{Path: where, Message: "duplicate repository path " + expanded})
		}
		seen[expanded] = true
		if r.Forge != "" && !contains(allowedForges, r.Forge) {
			issues = append(issues, Issue{Path: where + ".forge",
				Message: fmt.Sprintf("must be one of %s, got %q", strings.Join(allowedForges, ", "), r.Forge)})
		}
		if r.Schedule != nil {
			issues = append(issues, c.validateSchedule(*r.Schedule, where+".schedule")...)
		}
	}

	issues = append(issues, c.validateSchedule(c.Defaults.Schedule, "defaults.schedule")...)

	cd := c.Defaults.Cooldown
	for name, days := range map[string]int{
		"patch_days": cd.PatchDays, "minor_days": cd.MinorDays, "major_days": cd.MajorDays,
	} {
		if days < 0 {
			issues = append(issues, Issue{Path: "defaults.cooldown." + name, Message: "must not be negative"})
		}
	}
	if c.Defaults.MaxOpenBranches < 0 {
		issues = append(issues, Issue{Path: "defaults.max_open_branches", Message: "must not be negative"})
	}
	if c.Defaults.BisectMaxAttempts < 0 {
		issues = append(issues, Issue{Path: "defaults.bisect_max_attempts", Message: "must not be negative"})
	}
	if !contains(allowedSandboxes, c.Defaults.Verify.Sandbox) {
		issues = append(issues, Issue{Path: "defaults.verify.sandbox",
			Message: fmt.Sprintf("must be one of bubblewrap, container, none; got %q", c.Defaults.Verify.Sandbox)})
	}
	if c.Defaults.Verify.Timeout != "" {
		if _, err := time.ParseDuration(c.Defaults.Verify.Timeout); err != nil {
			issues = append(issues, Issue{Path: "defaults.verify.timeout", Message: "not a duration: " + err.Error()})
		}
	}

	for idx, u := range c.Updates {
		where := fmt.Sprintf("updates[%d]", idx)
		if strings.TrimSpace(u.Ecosystem) == "" {
			issues = append(issues, Issue{Path: where, Message: "ecosystem is required"})
		}
		for j, rule := range u.Ignore {
			ruleWhere := fmt.Sprintf("%s.ignore[%d]", where, j)
			if strings.TrimSpace(rule.Dependency) == "" {
				issues = append(issues, Issue{Path: ruleWhere, Message: "dependency is required"})
			}
			if strings.TrimSpace(rule.Reason) == "" {
				issues = append(issues, Issue{Path: ruleWhere, Message: "reason is mandatory for every ignore rule"})
			}
			for _, ut := range rule.UpdateTypes {
				if !contains(allowedUpdateType, ut) {
					issues = append(issues, Issue{Path: ruleWhere + ".update_types",
						Message: fmt.Sprintf("unknown update type %q", ut)})
				}
			}
		}
		for _, al := range u.Allow {
			if al.DependencyType != "" && !contains([]string{"production", "development"}, al.DependencyType) {
				issues = append(issues, Issue{Path: where + ".allow",
					Message: fmt.Sprintf("dependency_type must be production or development, got %q", al.DependencyType)})
			}
			if al.UpdateType != "" && !contains(allowedUpdateType, al.UpdateType) {
				issues = append(issues, Issue{Path: where + ".allow",
					Message: fmt.Sprintf("unknown update type %q", al.UpdateType)})
			}
		}
	}

	if len(issues) == 0 {
		return nil
	}
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Line != issues[j].Line {
			return issues[i].Line < issues[j].Line
		}
		return issues[i].Path < issues[j].Path
	})
	return &ValidationError{Issues: issues}
}

func (c *Config) validateSchedule(s Schedule, where string) []Issue {
	var issues []Issue
	if s.Interval == "" {
		return issues
	}
	if !contains(allowedIntervals, s.Interval) {
		issues = append(issues, Issue{Path: where + ".interval",
			Message: fmt.Sprintf("must be one of %s, got %q", strings.Join(allowedIntervals, ", "), s.Interval)})
	}
	if s.Time != "" {
		if _, err := time.Parse("15:04", s.Time); err != nil {
			issues = append(issues, Issue{Path: where + ".time", Message: "must be HH:MM: " + err.Error()})
		}
	}
	if s.Timezone != "" {
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			issues = append(issues, Issue{Path: where + ".timezone", Message: "unknown time zone " + s.Timezone})
		}
	}
	if s.Interval == "weekly" && s.Day == "" {
		issues = append(issues, Issue{Path: where + ".day", Message: "weekly schedules need a day"})
	}
	if s.Interval == "cron" && strings.TrimSpace(s.Cron) == "" {
		issues = append(issues, Issue{Path: where + ".cron", Message: "cron schedules need an expression"})
	}
	return issues
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
