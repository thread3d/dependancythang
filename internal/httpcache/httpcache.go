// Package httpcache is a small on-disk HTTP cache with ETag revalidation.
//
// It exists so registry and advisory lookups share one caching policy (§3 row
// 16): cache everything, revalidate when possible, and serve stale data in
// offline mode rather than failing a scan.
package httpcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// DefaultTTL is how long a response is trusted before revalidation.
const DefaultTTL = 6 * time.Hour

// Cache is a directory of cached responses keyed by a caller-chosen string.
type Cache struct {
	dir string
	ttl time.Duration
}

// Entry is one cached response.
type Entry struct {
	Key     string `json:"key"`
	ETag    string `json:"etag,omitempty"`
	Body    string `json:"body"`
	Fetched string `json:"fetched"`
}

// NewCache creates the cache directory.
func NewCache(dir string, ttl time.Duration) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Cache{dir: dir, ttl: ttl}, nil
}

func (c *Cache) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])[:32]+".json")
}

// Get returns a cached entry if present.
func (c *Cache) Get(key string) (Entry, bool) {
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		return Entry{}, false
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, false
	}
	return e, true
}

// Put stores an entry, ignoring failures: a cache write must never fail a scan.
func (c *Cache) Put(e Entry) {
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	target := c.path(e.Key)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, target)
}

// Fresh reports whether an entry is still within the TTL.
func (c *Cache) Fresh(e Entry) bool {
	t, err := time.Parse(time.RFC3339, e.Fetched)
	if err != nil {
		return false
	}
	return time.Since(t) < c.ttl
}

// UserAgent identifies depmcp to registries. crates.io rejects requests without
// one outright, and GitHub's API prefers it.
const UserAgent = "depmcp (+https://github.com/thread3d/dependancythang)"

// Client performs cached GET and POST requests.
type Client struct {
	HTTP    *http.Client
	Cache   *Cache
	Offline bool
	// MaxBody caps response size. Zero means 8 MiB.
	MaxBody int64
}

// NewClient builds a client, creating the cache directory.
func NewClient(cacheDir string, offline bool) (*Client, error) {
	cache, err := NewCache(cacheDir, DefaultTTL)
	if err != nil {
		return nil, err
	}
	return &Client{
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Cache:   cache,
		Offline: offline,
	}, nil
}

func (c *Client) limit() int64 {
	if c.MaxBody > 0 {
		return c.MaxBody
	}
	return 8 << 20
}

// Get fetches url using the cache and revalidates with If-None-Match.
func (c *Client) Get(ctx context.Context, url string) (string, error) {
	cached, have := c.Cache.Get(url)
	if have && c.Cache.Fresh(cached) {
		return cached.Body, nil
	}
	if c.Offline {
		if have {
			return cached.Body, nil
		}
		return "", fmt.Errorf("offline and no cached response for %s", url)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	if have && cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	body, etag, notModified, err := c.do(req)
	if err != nil {
		if have {
			return cached.Body, nil // stale beats nothing
		}
		return "", err
	}
	if notModified {
		cached.Fetched = time.Now().UTC().Format(time.RFC3339)
		c.Cache.Put(cached)
		return cached.Body, nil
	}
	c.Cache.Put(Entry{Key: url, ETag: etag, Body: body, Fetched: nowRFC3339()})
	return body, nil
}

// Post performs a cached POST. A POST body cannot be revalidated with ETags, so
// the cache key is the URL plus a digest of the request body (§9).
func (c *Client) Post(ctx context.Context, url string, payload []byte) (string, error) {
	sum := sha256.Sum256(payload)
	key := "POST " + url + " #" + hex.EncodeToString(sum[:])

	cached, have := c.Cache.Get(key)
	if have && c.Cache.Fresh(cached) {
		return cached.Body, nil
	}
	if c.Offline {
		if have {
			return cached.Body, nil
		}
		return "", fmt.Errorf("offline and no cached response for %s", url)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	body, etag, _, err := c.do(req)
	if err != nil {
		if have {
			return cached.Body, nil
		}
		return "", err
	}
	c.Cache.Put(Entry{Key: key, ETag: etag, Body: body, Fetched: nowRFC3339()})
	return body, nil
}

func (c *Client) do(req *http.Request) (body, etag string, notModified bool, err error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", false, fmt.Errorf("%s %s: %w", req.Method, req.URL, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return "", "", true, nil
	case resp.StatusCode == http.StatusNotFound:
		return "", "", false, fmt.Errorf("%w: %s %s", ErrNotFound, req.Method, req.URL)
	case resp.StatusCode != http.StatusOK:
		return "", "", false, fmt.Errorf("%s %s: unexpected status %s", req.Method, req.URL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.limit()))
	if err != nil {
		return "", "", false, fmt.Errorf("read %s: %w", req.URL, err)
	}
	return string(data), resp.Header.Get("ETag"), false, nil
}

// nowRFC3339 renders the current time for cache entries.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// ErrNotFound reports a 404 from the origin.
var ErrNotFound = fmt.Errorf("not found")
