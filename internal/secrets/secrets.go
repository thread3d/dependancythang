// Package secrets stores forge credentials in the operating system's keyring.
//
// A token is never written to configuration, to the state database, or to a log
// (§11). The keyring is the OS facility: Secret Service on Linux, Keychain on
// macOS, Credential Manager on Windows.
//
// Availability is not assumed. Headless machines, containers, and SSH sessions
// frequently have no keyring at all, so every call reports a clear error rather
// than silently falling back to a plaintext file.
package secrets

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zalando/go-keyring"
)

// Service is the keyring namespace every entry lives under.
const Service = "depmcp"

// ErrUnavailable reports that no OS keyring can be reached.
var ErrUnavailable = errors.New("no OS keyring available")

// ErrNotFound reports a missing entry.
var ErrNotFound = errors.New("secret not found")

// Backend stores and retrieves secrets. It exists so tests can run without a
// session bus, and so an alternative backend can be added without touching
// callers.
type Backend interface {
	// Name identifies the backend in output.
	Name() string
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
}

// Store resolves secrets through a backend, applying the key naming scheme.
type Store struct {
	Backend Backend
}

// Key renders the keyring entry name for a forge.
//
// The account is the forge name so a user can see which token is which in their
// keyring UI, and the service is depmcp so all entries group together.
func Key(forge string) string {
	return strings.ToLower(strings.TrimSpace(forge)) + ":token"
}

// Get returns the token for a forge, or ErrNotFound.
func (s Store) Get(forge string) (string, error) {
	if s.Backend == nil {
		return "", ErrUnavailable
	}
	value, err := s.Backend.Get(Key(forge))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// Set stores a token for a forge.
func (s Store) Set(forge, token string) error {
	if s.Backend == nil {
		return ErrUnavailable
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("refusing to store an empty token for %q", forge)
	}
	return s.Backend.Set(Key(forge), token)
}

// Delete removes a token.
func (s Store) Delete(forge string) error {
	if s.Backend == nil {
		return ErrUnavailable
	}
	return s.Backend.Delete(Key(forge))
}

// Available reports whether the keyring can be used right now, returning a
// human-readable reason when it cannot.
//
// The only honest test is a round trip: a session bus can exist while the
// collection is locked or absent.
func (s Store) Available() error {
	if s.Backend == nil {
		return ErrUnavailable
	}
	if probe, ok := s.Backend.(interface{ Probe() error }); ok {
		return probe.Probe()
	}
	return nil
}

// Keyring is the OS-backed implementation.
type Keyring struct{}

// Name implements Backend.
func (Keyring) Name() string { return "os-keyring" }

// Get implements Backend.
func (Keyring) Get(key string) (string, error) {
	value, err := keyring.Get(Service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return value, nil
}

// Set implements Backend.
func (Keyring) Set(key, value string) error {
	if err := keyring.Set(Service, key, value); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// Delete implements Backend.
func (Keyring) Delete(key string) error {
	err := keyring.Delete(Service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// Probe writes and removes a throwaway entry, which is the only way to know the
// keyring is actually usable rather than merely present.
func (Keyring) Probe() error {
	const key = "probe"
	if err := keyring.Set(Service, key, "ok"); err != nil {
		return fmt.Errorf("%w: cannot write a test entry (%v)", ErrUnavailable, err)
	}
	_ = keyring.Delete(Service, key)
	return nil
}

// Memory is an in-process backend for tests.
type Memory struct {
	values map[string]string
	// Fail makes every operation fail, to exercise the unavailable path.
	Fail bool
}

// NewMemory returns an empty in-memory backend.
func NewMemory() *Memory { return &Memory{values: map[string]string{}} }

// Name implements Backend.
func (*Memory) Name() string { return "memory" }

// Get implements Backend.
func (m *Memory) Get(key string) (string, error) {
	if m.Fail {
		return "", ErrUnavailable
	}
	v, ok := m.values[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

// Set implements Backend.
func (m *Memory) Set(key, value string) error {
	if m.Fail {
		return ErrUnavailable
	}
	if m.values == nil {
		m.values = map[string]string{}
	}
	m.values[key] = value
	return nil
}

// Probe implements the availability check, so a failing test backend behaves like
// a keyring that cannot be reached.
func (m *Memory) Probe() error {
	if m.Fail {
		return ErrUnavailable
	}
	return nil
}

// Delete implements Backend.
func (m *Memory) Delete(key string) error {
	if m.Fail {
		return ErrUnavailable
	}
	if _, ok := m.values[key]; !ok {
		return ErrNotFound
	}
	delete(m.values, key)
	return nil
}
