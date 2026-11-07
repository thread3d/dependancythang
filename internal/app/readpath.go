package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thread3d/dependancythang/internal/advisory"
	"github.com/thread3d/dependancythang/internal/config"
	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/ecosystem/gomod"
	"github.com/thread3d/dependancythang/internal/planner"
	"github.com/thread3d/dependancythang/internal/policy"
	"github.com/thread3d/dependancythang/internal/registry"
	"github.com/thread3d/dependancythang/internal/store"
)

// Stable codes introduced by the read path (§7).
const (
	CodeRegistryUnavailable = "REGISTRY_UNAVAILABLE"
	CodeOfflineCacheMiss    = "OFFLINE_CACHE_MISS"
	CodePlanNotFound        = "PLAN_NOT_FOUND"
	CodeNoSuchRepository    = "NO_SUCH_REPOSITORY"
)

// ecosystemFor returns the ecosystem implementation for a name.
func ecosystemFor(name string) (ecosystem.Ecosystem, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "gomod":
		return gomod.New(), nil
	default:
		return nil, fmt.Errorf("ecosystem %q is not implemented yet (v1 supports gomod)", name)
	}
}

// registryFor builds a registry client bound to the state directory cache.
func (s *Service) registryFor() (ecosystem.Registry, error) {
	if s.Registry != nil {
		return s.Registry, nil
	}
	return registry.NewClient(registry.ProxyURL,
		filepath.Join(s.stateDir, "caches", "registry"), s.Cfg.Server.Offline)
}

// advisoryClient builds an OSV client bound to the state directory cache.
func (s *Service) advisoryClient() (*advisory.Client, error) {
	return advisory.NewClient(advisory.OSVURL,
		filepath.Join(s.stateDir, "caches", "osv"), s.Cfg.Server.Offline)
}

// RepoTarget is a repository with its resolved path and detected targets.
type RepoTarget struct {
	ID      string             `json:"id"`
	Path    string             `json:"path"`
	Targets []ecosystem.Target `json:"targets"`
}

// repoByID resolves a repository entry from its stable id.
func (s *Service) repoByID(id string) (config.Repository, string, error) {
	for _, r := range s.Cfg.Repositories {
		expanded, err := config.ExpandPath(r.Path)
		if err != nil {
			continue
		}
		if config.RepoID(expanded) == id {
			return r, expanded, nil
		}
	}
	// Also accept an absolute path for convenience.
	expanded, err := config.ExpandPath(id)
	if err == nil {
		if r, ok := s.Cfg.Find(expanded); ok {
			return r, expanded, nil
		}
	}
	return config.Repository{}, "", coded(CodeNoSuchRepository,
		fmt.Errorf("no tracked repository with id or path %q", id))
}

// ScanDependencies detects targets and returns their inventory, persisting a
// snapshot for each.
func (s *Service) ScanDependencies(ctx context.Context, repoRef string) (RepoTarget, []ecosystem.Dependency, error) {
	repo, path, err := s.repoByID(repoRef)
	if err != nil {
		return RepoTarget{}, nil, err
	}
	eco, err := ecosystemFor("gomod")
	if err != nil {
		return RepoTarget{}, nil, err
	}
	targets, err := eco.Detect(ctx, path)
	if err != nil {
		return RepoTarget{}, nil, err
	}
	out := RepoTarget{ID: config.RepoID(path), Path: path, Targets: targets}

	var all []ecosystem.Dependency
	now := time.Now()
	for _, t := range targets {
		deps, err := eco.Inventory(ctx, t)
		if err != nil {
			return out, all, err
		}
		rows := make([]store.DependencyRow, 0, len(deps))
		for _, d := range deps {
			rows = append(rows, store.DependencyRow{Name: d.Name, Current: d.Current, Direct: d.Direct})
		}
		if err := s.Store.SaveInventory(ctx, out.ID, t.Key(), rows, now); err != nil {
			return out, all, err
		}
		all = append(all, deps...)
	}
	_ = s.Store.MarkScanned(ctx, out.ID, now)
	_ = repo
	return out, all, nil
}

// Candidate is one dependency with the versions available to it.
type Candidate struct {
	Dependency  string                `json:"dependency"`
	From        string                `json:"from"`
	Latest      string                `json:"latest,omitempty"`
	Class       ecosystem.UpdateClass `json:"class"`
	PublishedAt string                `json:"published_at,omitempty"`
	Direct      bool                  `json:"direct"`
	Target      string                `json:"target"`
	Error       string                `json:"error,omitempty"`
}

// CheckUpdates resolves available versions for each dependency. Only direct
// dependencies are queried by default: transitive ones are reported by
// scan_dependencies and addressed through advisories (§3 row 9).
func (s *Service) CheckUpdates(ctx context.Context, repoRef string, includeIndirect bool, limit int) (RepoTarget, []Candidate, error) {
	repoTarget, deps, err := s.ScanDependencies(ctx, repoRef)
	if err != nil {
		return repoTarget, nil, err
	}
	eco, err := ecosystemFor("gomod")
	if err != nil {
		return repoTarget, nil, err
	}
	reg, err := s.registryFor()
	if err != nil {
		return repoTarget, nil, err
	}

	var out []Candidate
	for _, d := range deps {
		if !d.Direct && !includeIndirect {
			continue
		}
		c := Candidate{Dependency: d.Name, From: d.Current, Direct: d.Direct, Target: d.Target}
		versions, verr := reg.Versions(ctx, d.Name)
		if verr != nil {
			c.Error = verr.Error()
			c.Class = ecosystem.ClassUnknown
			out = append(out, c)
			continue
		}
		for _, v := range versions {
			if v.Yanked {
				continue
			}
			if strings.Contains(v.Raw, "-") && !strings.Contains(d.Current, "-") {
				continue
			}
			if eco.Compare(v.Raw, d.Current) <= 0 {
				continue
			}
			c.Latest = v.Raw
			c.Class = eco.Classify(d.Current, v.Raw)
			c.PublishedAt, _ = reg.PublishTime(ctx, d.Name, v.Raw)
			break
		}
		out = append(out, c)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Dependency < out[j].Dependency })
	return repoTarget, out, nil
}

// Vulnerability is one advisory affecting a current version.
type Vulnerability struct {
	Dependency string            `json:"dependency"`
	Version    string            `json:"version"`
	Direct     bool              `json:"direct"`
	Target     string            `json:"target,omitempty"`
	ID         string            `json:"id"`
	Aliases    []string          `json:"aliases,omitempty"`
	Severity   advisory.Severity `json:"severity"`
	Summary    string            `json:"summary,omitempty"`
	Fixed      []string          `json:"fixed_versions,omitempty"`
}

// CheckVulnerabilities queries OSV for every dependency currently in use.
// An explicit call bypasses the cache TTL (§9): a critical advisory published an
// hour ago being invisible to the person who just asked is unacceptable.
func (s *Service) CheckVulnerabilities(ctx context.Context, repoRef string) (RepoTarget, []Vulnerability, error) {
	repoTarget, deps, err := s.ScanDependencies(ctx, repoRef)
	if err != nil {
		return repoTarget, nil, err
	}
	osvc, err := s.advisoryClient()
	if err != nil {
		return repoTarget, nil, err
	}

	queries := make([]advisory.Query, 0, len(deps))
	for _, d := range deps {
		queries = append(queries, advisory.Query{Ecosystem: "Go", Name: d.Name, Version: d.Current})
	}
	results, err := osvc.QueryBatch(ctx, queries)
	if err != nil {
		return repoTarget, nil, coded(CodeRegistryUnavailable, err)
	}

	var out []Vulnerability
	now := time.Now()
	for i, advisories := range results {
		dep := deps[i]
		rows := make([]store.AdvisoryRow, 0, len(advisories))
		for _, a := range advisories {
			rows = append(rows, store.AdvisoryRow{ID: a.ID, Severity: string(a.Severity)})
			out = append(out, Vulnerability{
				Dependency: dep.Name, Version: dep.Current, Direct: dep.Direct, Target: dep.Target,
				ID: a.ID, Aliases: a.Aliases, Severity: a.Severity,
				Summary: a.Summary, Fixed: a.FixedVersions,
			})
		}
		if len(rows) > 0 {
			_ = s.Store.SaveAdvisories(ctx, repoTarget.ID, dep.Target, dep.Name, dep.Current, rows, now)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return severityOrder(out[i].Severity) > severityOrder(out[j].Severity)
		}
		return out[i].Dependency < out[j].Dependency
	})
	return repoTarget, out, nil
}

func severityOrder(s advisory.Severity) int {
	switch s {
	case advisory.SeverityCritical:
		return 4
	case advisory.SeverityHigh:
		return 3
	case advisory.SeverityMedium:
		return 2
	case advisory.SeverityLow:
		return 1
	default:
		return 0
	}
}

// policyRules resolves configuration into pure policy rules.
func (s *Service) policyRules(now time.Time) policy.Rules {
	rules := policy.Rules{
		Now: now,
		Cooldown: policy.Cooldown{
			PatchDays: s.Cfg.Defaults.Cooldown.PatchDays,
			MinorDays: s.Cfg.Defaults.Cooldown.MinorDays,
			MajorDays: s.Cfg.Defaults.Cooldown.MajorDays,
		},
	}
	for _, sev := range s.Cfg.Security.BypassCooldown {
		rules.BypassCooldown = append(rules.BypassCooldown, advisory.Severity(strings.ToLower(sev)))
	}
	for _, target := range s.Cfg.Updates {
		for _, ig := range target.Ignore {
			rules.Ignore = append(rules.Ignore, policy.IgnoreRule{
				Dependency: ig.Dependency, Versions: ig.Versions,
				UpdateTypes: ig.UpdateTypes, Reason: ig.Reason, Until: ig.Until,
			})
		}
		for name, g := range target.Groups {
			rules.Groups = append(rules.Groups, policy.Group{Name: name, Patterns: g.Patterns})
		}
	}
	sort.SliceStable(rules.Groups, func(i, j int) bool { return rules.Groups[i].Name < rules.Groups[j].Name })
	return rules
}

// PlanUpdates builds and stores an UpdatePlan for a repository.
func (s *Service) PlanUpdates(ctx context.Context, repoRef string) (planner.Plan, error) {
	repoTarget, deps, err := s.ScanDependencies(ctx, repoRef)
	if err != nil {
		return planner.Plan{}, err
	}
	eco, err := ecosystemFor("gomod")
	if err != nil {
		return planner.Plan{}, err
	}
	reg, err := s.registryFor()
	if err != nil {
		return planner.Plan{}, err
	}
	osvc, err := s.advisoryClient()
	if err != nil {
		return planner.Plan{}, err
	}
	rules := s.policyRules(time.Now())

	var decisions []policy.Decision
	for _, d := range deps {
		in := policy.Input{Dep: d, Classify: eco.Classify, Compare: eco.Compare}
		if versions, verr := reg.Versions(ctx, d.Name); verr == nil {
			in.Candidates = versions
		}
		if advisories, aerr := osvc.QueryBatch(ctx, []advisory.Query{{
			Ecosystem: "Go", Name: d.Name, Version: d.Current,
		}}); aerr == nil && len(advisories) == 1 {
			in.Advisories = advisories[0]
		}
		decisions = append(decisions, policy.Decide(in, rules))
	}

	plan := planner.Build(decisions, "gomod", repoTarget.ID, time.Now())
	if len(repoTarget.Targets) > 0 {
		plan.SetTarget(repoTarget.Targets[0].Key())
	}
	data, err := json.Marshal(plan)
	if err != nil {
		return plan, fmt.Errorf("encode plan: %w", err)
	}
	created := time.Now()
	if err := s.Store.SavePlan(ctx, plan.PlanID, repoTarget.ID, plan.Groups[0].Target, "gomod", created, data); err != nil {
		return plan, err
	}
	return plan, nil
}

// GetPlan loads a stored plan by id.
func (s *Service) GetPlan(ctx context.Context, planID string) (planner.Plan, error) {
	rec, err := s.Store.LoadPlan(ctx, planID)
	if errors.Is(err, store.ErrNoPlan) {
		return planner.Plan{}, coded(CodePlanNotFound, err)
	}
	if err != nil {
		return planner.Plan{}, err
	}
	var plan planner.Plan
	if err := json.Unmarshal([]byte(rec.JSON), &plan); err != nil {
		return planner.Plan{}, fmt.Errorf("decode stored plan %s: %w", planID, err)
	}
	return plan, nil
}

// statePath joins paths under the state directory.
func (s *Service) statePath(parts ...string) string {
	return filepath.Join(append([]string{s.stateDir}, parts...)...)
}

// ensure os is referenced even when offline cache handling is compiled out.
var _ = os.Getenv
