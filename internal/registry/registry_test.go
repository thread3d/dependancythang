package registry_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/thread3d/dependancythang/internal/registry"
)

// fakeProxy serves the module proxy protocol for one module.
func fakeProxy(t *testing.T, hits *int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/example.com/mod/@v/list", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hits, 1)
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, "v1.0.0\nv1.2.0\nv1.10.0\nv1.3.0\n")
	})
	mux.HandleFunc("/example.com/mod/@v/v1.10.0.info", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hits, 1)
		fmt.Fprint(w, `{"Version":"v1.10.0","Time":"2026-10-01T10:00:00Z"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestVersionsAreSortedNewestFirst(t *testing.T) {
	var hits int64
	srv := fakeProxy(t, &hits)
	c, err := registry.NewClient(srv.URL, t.TempDir(), false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := c.Versions(context.Background(), "example.com/mod")
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	want := []string{"v1.10.0", "v1.3.0", "v1.2.0", "v1.0.0"}
	if len(got) != len(want) {
		t.Fatalf("got %d versions (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i].Raw != want[i] {
			t.Errorf("version[%d] = %q, want %q", i, got[i].Raw, want[i])
		}
	}
}

func TestResponsesAreCached(t *testing.T) {
	var hits int64
	srv := fakeProxy(t, &hits)
	cacheDir := t.TempDir()

	c, err := registry.NewClient(srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.Versions(context.Background(), "example.com/mod"); err != nil {
		t.Fatalf("first Versions: %v", err)
	}
	afterFirst := atomic.LoadInt64(&hits)

	if _, err := c.Versions(context.Background(), "example.com/mod"); err != nil {
		t.Fatalf("second Versions: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != afterFirst {
		t.Errorf("cache miss: server hit %d extra times", got-afterFirst)
	}

	// A fresh client over the same cache directory must also be served locally.
	c2, err := registry.NewClient(srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c2.Versions(context.Background(), "example.com/mod"); err != nil {
		t.Fatalf("third Versions: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != afterFirst {
		t.Errorf("cache did not persist across clients: %d extra hits", got-afterFirst)
	}
}

func TestPublishTime(t *testing.T) {
	var hits int64
	srv := fakeProxy(t, &hits)
	c, err := registry.NewClient(srv.URL, t.TempDir(), false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := c.PublishTime(context.Background(), "example.com/mod", "v1.10.0")
	if err != nil {
		t.Fatalf("PublishTime: %v", err)
	}
	if got != "2026-10-01T10:00:00Z" {
		t.Errorf("publish time = %q, want the .info timestamp", got)
	}
}

func TestUnknownModuleIsAnError(t *testing.T) {
	var hits int64
	srv := fakeProxy(t, &hits)
	c, err := registry.NewClient(srv.URL, t.TempDir(), false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.Versions(context.Background(), "example.com/missing"); err == nil {
		t.Fatal("expected an error for a module the proxy does not know")
	}
}

func TestOfflineUsesCacheAndFailsWithoutIt(t *testing.T) {
	var hits int64
	srv := fakeProxy(t, &hits)
	cacheDir := t.TempDir()

	online, err := registry.NewClient(srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := online.Versions(context.Background(), "example.com/mod"); err != nil {
		t.Fatalf("warm cache: %v", err)
	}

	offline, err := registry.NewClient(srv.URL, cacheDir, true)
	if err != nil {
		t.Fatalf("NewClient offline: %v", err)
	}
	if _, err := offline.Versions(context.Background(), "example.com/mod"); err != nil {
		t.Errorf("offline read of a warm cache failed: %v", err)
	}
	if _, err := offline.Versions(context.Background(), "example.com/cold"); err == nil {
		t.Error("offline read of an uncached module should fail with a cache miss")
	}
}
