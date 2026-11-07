package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Edit applies a mutation to the configuration document at path and writes it
// back atomically, preserving comments and key order for everything it does not
// touch (§5 mutation protocol).
//
// The sequence is: read → hash → mutate → re-read and compare hash → validate →
// temp file in the same directory → fsync → rename. The hash comparison is what
// makes a concurrent hand-edit detectable instead of silently discarded.
func Edit(path string, mutate func(root *yaml.Node) (bool, error)) (*Config, error) {
	abs, err := ExpandPath(path)
	if err != nil {
		return nil, err
	}
	original, existed, err := readOrSkeleton(abs)
	if err != nil {
		return nil, err
	}
	before := sha256.Sum256(original)

	var root yaml.Node
	if err := yaml.Unmarshal(original, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	if len(root.Content) == 0 {
		root.Kind = yaml.DocumentNode
		empty := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = []*yaml.Node{empty}
	}

	// Callers edit the mapping that is the document's content; handing them the
	// DocumentNode would append keys outside the map and lose them on encode.
	changed, err := mutate(root.Content[0])
	if err != nil {
		return nil, err
	}
	if !changed {
		cfg := Default()
		if err := decode(original, &cfg, abs); err != nil {
			return nil, err
		}
		return &cfg, nil
	}

	// Detect a concurrent hand-edit between our read and our write.
	current, err := os.ReadFile(abs)
	switch {
	case err == nil:
		if sha256.Sum256(current) != before {
			return nil, fmt.Errorf("%s: %w", abs, ErrChangedExternally)
		}
	case errors.Is(err, os.ErrNotExist):
		if existed {
			return nil, fmt.Errorf("%s: %w", abs, ErrChangedExternally)
		}
	default:
		return nil, fmt.Errorf("re-read %s: %w", abs, err)
	}

	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root.Content[0]); err != nil {
		return nil, fmt.Errorf("encode %s: %w", abs, err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode %s: %w", abs, err)
	}
	rendered := []byte(buf.String())

	// Never write a document we cannot load back.
	if issues := Validate(rendered); len(issues) > 0 {
		return nil, &ValidationError{Issues: issues}
	}
	if err := writeAtomic(abs, rendered); err != nil {
		return nil, err
	}

	cfg := Default()
	if err := decode(rendered, &cfg, abs); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// readOrSkeleton returns the file contents, or a minimal skeleton when the file
// does not exist yet. First run must not require hand-creating a directory.
func readOrSkeleton(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		return data, true, nil
	case errors.Is(err, os.ErrNotExist):
		return []byte("version: 1\nrepositories: []\n"), false, nil
	case errors.Is(err, os.ErrPermission):
		return nil, false, fmt.Errorf("%s: %w", path, ErrReadOnly)
	default:
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
}

// writeAtomic writes data to a temp file beside path, fsyncs it, and renames.
// A read-only directory or file surfaces as ErrReadOnly, which tools map onto
// the CONFIG_READONLY code with a copy-pasteable snippet.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, wrapReadOnly(err))
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o200 == 0 {
		return fmt.Errorf("%s: %w", path, ErrReadOnly)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, wrapReadOnly(err))
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, wrapReadOnly(err))
	}
	return nil
}

// wrapReadOnly translates permission and read-only-filesystem errors into
// ErrReadOnly so callers need only one check.
func wrapReadOnly(err error) error {
	if errors.Is(err, os.ErrPermission) || errors.Is(err, errors.ErrUnsupported) ||
		strings.Contains(err.Error(), "read-only file system") {
		return ErrReadOnly
	}
	return err
}

// ---- node helpers -------------------------------------------------------

// MappingValue returns the value node for key in a mapping node, or nil.
func MappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// SetMappingValue sets key to val, appending or replacing as needed.
func SetMappingValue(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

// ensureSequence returns the sequence node stored at key, creating it if asked.
func ensureSequence(root *yaml.Node, key string, create bool) *yaml.Node {
	if v := MappingValue(root, key); v != nil {
		return v
	}
	if !create {
		return nil
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	SetMappingValue(root, key, seq)
	return seq
}

// EncodeNode renders v as a YAML node.
func EncodeNode(v any) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return &n, nil
}

// repositoryPath returns the expanded path recorded in a repository sequence
// item, or "" when the item is not a mapping with a path.
func repositoryPath(item *yaml.Node) string {
	v := MappingValue(item, "path")
	if v == nil {
		return ""
	}
	expanded, err := ExpandPath(v.Value)
	if err != nil {
		return filepath.Clean(v.Value)
	}
	return expanded
}

// UpsertRepository adds or replaces a repository entry in the document root.
// It reports whether anything changed, so a no-op edit can skip the write.
func UpsertRepository(root *yaml.Node, repo Repository) (bool, error) {
	expanded, err := ExpandPath(repo.Path)
	if err != nil {
		return false, err
	}
	repo.Path = expanded
	seq := ensureSequence(root, "repositories", true)
	// Render as a block sequence even when the document started as `repositories: []`.
	// A committed config should diff line by line, not reflow onto one line.
	seq.Style = 0

	item, err := EncodeNode(repo)
	if err != nil {
		return false, err
	}
	for i, existing := range seq.Content {
		if repositoryPath(existing) == expanded {
			if sameRepository(existing, item) {
				return false, nil
			}
			seq.Content[i] = item
			return true, nil
		}
	}
	seq.Content = append(seq.Content, item)
	return true, nil
}

// RemoveRepository deletes a repository entry. Removing an untracked path is not
// an error: the caller asked for a state that already holds.
func RemoveRepository(root *yaml.Node, path string) (bool, error) {
	expanded, err := ExpandPath(path)
	if err != nil {
		return false, err
	}
	seq := ensureSequence(root, "repositories", false)
	if seq == nil {
		return false, nil
	}
	seq.Style = 0
	kept := seq.Content[:0]
	removed := false
	for _, item := range seq.Content {
		if repositoryPath(item) == expanded {
			removed = true
			continue
		}
		kept = append(kept, item)
	}
	if !removed {
		return false, nil
	}
	seq.Content = kept
	return true, nil
}

// sameRepository compares an existing node against a rendered one without
// depending on key order, so a re-add of an identical entry is a no-op.
func sameRepository(existing, want *yaml.Node) bool {
	var a, b Repository
	if err := existing.Decode(&a); err != nil {
		return false
	}
	if err := want.Decode(&b); err != nil {
		return false
	}
	return a.Path == b.Path && a.Forge == b.Forge && a.Remote == b.Remote && a.Schedule == nil && b.Schedule == nil
}
