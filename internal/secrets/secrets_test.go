package secrets_test

import (
	"errors"
	"testing"

	"github.com/thread3d/dependancythang/internal/secrets"
)

func TestKeyNaming(t *testing.T) {
	if got := secrets.Key("GitHub"); got != "github:token" {
		t.Errorf("Key(GitHub) = %q, want github:token", got)
	}
	if got := secrets.Key(" gitea "); got != "gitea:token" {
		t.Errorf("Key with spaces = %q, want gitea:token", got)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	backend := secrets.NewMemory()
	store := secrets.Store{Backend: backend}

	if _, err := store.Get("github"); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("Get on empty store = %v, want ErrNotFound", err)
	}
	if err := store.Set("github", "ghp_example"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := store.Get("github")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "ghp_example" {
		t.Errorf("Get = %q", got)
	}
	// Tokens are per forge: storing one must not make another appear.
	if _, err := store.Get("gitlab"); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("Get(gitlab) = %v, want ErrNotFound", err)
	}
	if err := store.Delete("github"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get("github"); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("after delete Get = %v, want ErrNotFound", err)
	}
}

func TestStoreTrimsAndRejectsEmpty(t *testing.T) {
	store := secrets.Store{Backend: secrets.NewMemory()}
	if err := store.Set("github", "   "); err == nil {
		t.Error("an empty token was stored")
	}
	if err := store.Set("github", "  token\n"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, _ := store.Get("github")
	if got != "token" {
		t.Errorf("Get = %q, want the token with whitespace trimmed", got)
	}
}

func TestUnavailableBackendIsReported(t *testing.T) {
	store := secrets.Store{Backend: &secrets.Memory{Fail: true}}
	if _, err := store.Get("github"); !errors.Is(err, secrets.ErrUnavailable) {
		t.Errorf("Get = %v, want ErrUnavailable", err)
	}
	if err := store.Set("github", "x"); !errors.Is(err, secrets.ErrUnavailable) {
		t.Errorf("Set = %v, want ErrUnavailable", err)
	}
	if err := store.Available(); !errors.Is(err, secrets.ErrUnavailable) {
		t.Errorf("Available = %v, want ErrUnavailable", err)
	}
}

func TestNilBackendIsUnavailableNotAPanic(t *testing.T) {
	store := secrets.Store{}
	if err := store.Available(); !errors.Is(err, secrets.ErrUnavailable) {
		t.Errorf("Available with no backend = %v, want ErrUnavailable", err)
	}
	if _, err := store.Get("github"); !errors.Is(err, secrets.ErrUnavailable) {
		t.Errorf("Get with no backend = %v, want ErrUnavailable", err)
	}
}
