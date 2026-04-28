// Package hamdb is a thin client for the free HamDB.org JSON API with an
// on-disk cache. One cache file per callsign lives under the user cache dir
// at ft8m8/hamdb/{CALL}.json. Positive results are served from cache for
// positiveTTL; negative results (callsign not found / transient errors) for
// negativeTTL.
package hamdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kyleomalley/nocordhf/lib/logging"
)

const (
	apiBase     = "https://api.hamdb.org"
	agent       = "ft8m8"
	positiveTTL = 30 * 24 * time.Hour
	negativeTTL = 24 * time.Hour
	httpTimeout = 8 * time.Second
)

// Record is a normalised HamDB callsign record.
type Record struct {
	Call    string `json:"call"`
	FName   string `json:"fname,omitempty"`
	MI      string `json:"mi,omitempty"`
	Name    string `json:"name,omitempty"`
	Suffix  string `json:"suffix,omitempty"`
	Class   string `json:"class,omitempty"` // T/G/E/A/N/C
	Grid    string `json:"grid,omitempty"`
	Lat     string `json:"lat,omitempty"`
	Lon     string `json:"lon,omitempty"`
	Addr1   string `json:"addr1,omitempty"`
	Addr2   string `json:"addr2,omitempty"`
	State   string `json:"state,omitempty"`
	Zip     string `json:"zip,omitempty"`
	Country string `json:"country,omitempty"`
	Expires string `json:"expires,omitempty"`
	Status  string `json:"status,omitempty"` // "A"=active
}

// DisplayName returns a human-friendly name string ("First M. Last, Sfx") or "".
func (r Record) DisplayName() string {
	parts := make([]string, 0, 4)
	if r.FName != "" {
		parts = append(parts, r.FName)
	}
	if r.MI != "" {
		parts = append(parts, r.MI+".")
	}
	if r.Name != "" {
		parts = append(parts, r.Name)
	}
	out := strings.Join(parts, " ")
	if r.Suffix != "" {
		out = strings.TrimSpace(out + ", " + r.Suffix)
	}
	return out
}

// cacheEntry is what we persist per callsign. Found=false caches a negative
// lookup so we don't hammer the API for unknown calls.
type cacheEntry struct {
	Fetched time.Time `json:"fetched"`
	Found   bool      `json:"found"`
	Record  *Record   `json:"record,omitempty"`
}

// Client is an hamdb.org client with on-disk cache and in-flight deduplication.
type Client struct {
	cacheDir string
	http     *http.Client

	inflightMu sync.Mutex
	inflight   map[string]chan struct{}
}

// New creates a Client with its cache under os.UserCacheDir()/ft8m8/hamdb.
func New() (*Client, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("user cache dir: %w", err)
	}
	dir := filepath.Join(base, "ft8m8", "hamdb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache: %w", err)
	}
	return &Client{
		cacheDir: dir,
		http:     &http.Client{Timeout: httpTimeout},
		inflight: map[string]chan struct{}{},
	}, nil
}

// ErrNotFound is returned when HamDB has no record for the callsign.
var ErrNotFound = errors.New("hamdb: callsign not found")

// Lookup returns the HamDB record for call, using cache when fresh. Returns
// ErrNotFound if HamDB says the call is unknown (cached as a negative result).
// Safe to call concurrently; lookups for the same call coalesce.
func (c *Client) Lookup(ctx context.Context, call string) (*Record, error) {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" {
		return nil, errors.New("hamdb: empty callsign")
	}

	if rec, found, ok := c.readCacheFresh(call); ok {
		if !found {
			return nil, ErrNotFound
		}
		return rec, nil
	}

	// Coalesce in-flight requests for the same call.
	c.inflightMu.Lock()
	if ch, ok := c.inflight[call]; ok {
		c.inflightMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// After the first flight completes, the cache should be populated.
		if rec, found, ok := c.readCacheFresh(call); ok {
			if !found {
				return nil, ErrNotFound
			}
			return rec, nil
		}
		// Fall through to retry ourselves.
	}
	done := make(chan struct{})
	c.inflight[call] = done
	c.inflightMu.Unlock()

	defer func() {
		c.inflightMu.Lock()
		delete(c.inflight, call)
		close(done)
		c.inflightMu.Unlock()
	}()

	rec, err := c.fetch(ctx, call)
	if errors.Is(err, ErrNotFound) {
		c.writeCache(call, cacheEntry{Fetched: time.Now(), Found: false})
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.writeCache(call, cacheEntry{Fetched: time.Now(), Found: true, Record: rec})
	return rec, nil
}

// LookupCached returns the cached record (fresh or stale) without making a
// network request. The bool indicates whether an entry exists at all.
func (c *Client) LookupCached(call string) (rec *Record, found, hasEntry bool) {
	call = strings.ToUpper(strings.TrimSpace(call))
	entry, ok := c.readCache(call)
	if !ok {
		return nil, false, false
	}
	return entry.Record, entry.Found, true
}

// readCacheFresh returns the cached record if present AND within TTL.
func (c *Client) readCacheFresh(call string) (rec *Record, found, ok bool) {
	entry, hit := c.readCache(call)
	if !hit {
		return nil, false, false
	}
	ttl := positiveTTL
	if !entry.Found {
		ttl = negativeTTL
	}
	if time.Since(entry.Fetched) > ttl {
		return nil, false, false
	}
	return entry.Record, entry.Found, true
}

func (c *Client) readCache(call string) (cacheEntry, bool) {
	f, err := os.Open(c.cachePath(call))
	if err != nil {
		return cacheEntry{}, false
	}
	defer f.Close()
	var e cacheEntry
	if err := json.NewDecoder(f).Decode(&e); err != nil {
		return cacheEntry{}, false
	}
	return e, true
}

func (c *Client) writeCache(call string, e cacheEntry) {
	path := c.cachePath(call)
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		logging.L.Warnw("hamdb cache create failed", "call", call, "err", err)
		return
	}
	if err := json.NewEncoder(f).Encode(e); err != nil {
		f.Close()
		os.Remove(tmp)
		logging.L.Warnw("hamdb cache encode failed", "call", call, "err", err)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logging.L.Warnw("hamdb cache rename failed", "call", call, "err", err)
	}
}

func (c *Client) cachePath(call string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, call)
	return filepath.Join(c.cacheDir, safe+".json")
}

// hamdbResp is the raw HamDB JSON envelope.
type hamdbResp struct {
	Hamdb struct {
		Callsign json.RawMessage `json:"callsign"`
		Messages struct {
			Status string `json:"status"`
		} `json:"messages"`
	} `json:"hamdb"`
}

func (c *Client) fetch(ctx context.Context, call string) (*Record, error) {
	url := fmt.Sprintf("%s/%s/json/%s", apiBase, call, agent)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", agent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hamdb GET: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain up to 1 KB for logging and move on.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("hamdb HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var env hamdbResp
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("hamdb decode: %w", err)
	}

	status := strings.ToUpper(strings.TrimSpace(env.Hamdb.Messages.Status))
	if status != "OK" {
		// HamDB reports "NOT_FOUND" for unknown calls.
		return nil, ErrNotFound
	}

	var rec Record
	if err := json.Unmarshal(env.Hamdb.Callsign, &rec); err != nil {
		// Some error responses put an empty string where the object would be.
		return nil, ErrNotFound
	}
	if strings.TrimSpace(rec.Call) == "" {
		return nil, ErrNotFound
	}
	return &rec, nil
}
