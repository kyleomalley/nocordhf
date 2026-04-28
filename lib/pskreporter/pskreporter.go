// Package pskreporter is a minimal client for pskreporter.info that summarises
// recent FT8 activity per ham band. It polls the public XML query endpoint,
// counts reception reports + active receivers per band, and persists the last
// successful snapshot to disk so a restart renders useful data immediately.
package pskreporter

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kyleomalley/nocordhf/lib/logging"
)

const (
	queryURL    = "https://retrieve.pskreporter.info/query"
	httpTimeout = 20 * time.Second
	// flowWindow is the look-back window passed to pskreporter via
	// flowStartSeconds. 15 minutes is a good balance between signal (enough
	// reports to rank bands) and freshness (tracks propagation changes).
	flowWindow = 15 * time.Minute
)

// BandStats is the summarised activity for one band.
type BandStats struct {
	Reports   int       `json:"reports"`
	Monitors  int       `json:"monitors"`
	FetchedAt time.Time `json:"fetched_at"`
}

// BandSpec defines a band the client should fetch. Name is used as the map
// key; LowerHz/UpperHz bound the frequency query.
type BandSpec struct {
	Name    string
	LowerHz uint64
	UpperHz uint64
}

// Client polls pskreporter.info and caches per-band activity counts.
// Safe for concurrent use.
type Client struct {
	http       *http.Client
	cacheDir   string
	appContact string

	mu    sync.RWMutex
	stats map[string]BandStats
}

// New constructs a Client. appContact is sent to pskreporter as a courtesy
// identifier (typically an email). cacheDir may be empty to disable disk
// persistence.
func New(appContact, cacheDir string) *Client {
	c := &Client{
		http:       &http.Client{Timeout: httpTimeout},
		cacheDir:   cacheDir,
		appContact: strings.TrimSpace(appContact),
		stats:      map[string]BandStats{},
	}
	if cacheDir != "" {
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			logging.L.Warnw("pskreporter cache dir init failed", "err", err)
			c.cacheDir = ""
		} else if loaded, err := loadCache(c.cachePath()); err == nil {
			c.stats = loaded
		}
	}
	return c
}

// Stats returns the current counts for one band. ok=false if nothing cached.
func (c *Client) Stats(band string) (BandStats, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.stats[band]
	return s, ok
}

// All returns a shallow copy of all band stats.
func (c *Client) All() map[string]BandStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]BandStats, len(c.stats))
	for k, v := range c.stats {
		out[k] = v
	}
	return out
}

// Refresh issues a single HF-wide query to pskreporter and buckets results
// into per-band stats. One request per refresh stays well under the public
// "≤1 query per 5 minutes" guideline.
func (c *Client) Refresh(ctx context.Context, bands []BandSpec) error {
	if len(bands) == 0 {
		return nil
	}
	var lo, hi uint64 = bands[0].LowerHz, bands[0].UpperHz
	for _, b := range bands[1:] {
		if b.LowerHz < lo {
			lo = b.LowerHz
		}
		if b.UpperHz > hi {
			hi = b.UpperHz
		}
	}

	reports, monitors, err := c.fetchRange(ctx, lo, hi)
	if err != nil {
		return fmt.Errorf("pskreporter refresh: %w", err)
	}

	now := time.Now().UTC()
	next := make(map[string]BandStats, len(bands))
	for _, b := range bands {
		next[b.Name] = BandStats{FetchedAt: now}
	}
	for _, freq := range reports {
		if name, ok := bandForFreq(freq, bands); ok {
			s := next[name]
			s.Reports++
			next[name] = s
		}
	}
	for _, freq := range monitors {
		if name, ok := bandForFreq(freq, bands); ok {
			s := next[name]
			s.Monitors++
			next[name] = s
		}
	}

	c.mu.Lock()
	for k, v := range next {
		c.stats[k] = v
	}
	snap := make(map[string]BandStats, len(c.stats))
	for k, v := range c.stats {
		snap[k] = v
	}
	c.mu.Unlock()

	if c.cacheDir != "" {
		if err := saveCache(c.cachePath(), snap); err != nil {
			logging.L.Warnw("pskreporter cache write failed", "err", err)
		}
	}
	return nil
}

func bandForFreq(freq uint64, bands []BandSpec) (string, bool) {
	for _, b := range bands {
		if freq >= b.LowerHz && freq <= b.UpperHz {
			return b.Name, true
		}
	}
	return "", false
}

// fetchRange performs one XML query covering the full frequency range and
// returns the per-report and per-monitor frequency lists.
func (c *Client) fetchRange(ctx context.Context, lo, hi uint64) ([]uint64, []uint64, error) {
	q := url.Values{}
	q.Set("frequency", fmt.Sprintf("%d-%d", lo, hi))
	q.Set("mode", "FT8")
	q.Set("flowStartSeconds", fmt.Sprintf("-%d", int(flowWindow.Seconds())))
	q.Set("rronly", "0")
	q.Set("nolocator", "1")
	q.Set("statistics", "1")
	if c.appContact != "" {
		q.Set("appcontact", c.appContact)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, queryURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "nocordhf")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("GET: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return parseXML(resp.Body)
}

// pskResp matches the subset of the pskreporter XML we care about. Frequencies
// come back as attributes on each element; we keep them so the caller can
// bucket into bands client-side.
type pskResp struct {
	XMLName         xml.Name   `xml:"receptionReports"`
	ActiveReceivers []freqElem `xml:"activeReceiver"`
	Reports         []freqElem `xml:"receptionReport"`
}

type freqElem struct {
	Frequency uint64 `xml:"frequency,attr"`
}

func parseXML(r io.Reader) (reports []uint64, monitors []uint64, err error) {
	var env pskResp
	dec := xml.NewDecoder(r)
	dec.Strict = false
	if err := dec.Decode(&env); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("decode: %w", err)
	}
	reports = make([]uint64, 0, len(env.Reports))
	for _, e := range env.Reports {
		reports = append(reports, e.Frequency)
	}
	monitors = make([]uint64, 0, len(env.ActiveReceivers))
	for _, e := range env.ActiveReceivers {
		monitors = append(monitors, e.Frequency)
	}
	return reports, monitors, nil
}

func (c *Client) cachePath() string {
	return filepath.Join(c.cacheDir, "state.json")
}

func loadCache(path string) (map[string]BandStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out map[string]BandStats
	if err := json.NewDecoder(f).Decode(&out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]BandStats{}
	}
	return out, nil
}

func saveCache(path string, m map[string]BandStats) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(m); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ── Presentation helpers ─────────────────────────────────────────────────────

// Tier is a coarse activity bucket used by the UI.
type Tier int

const (
	TierQuiet  Tier = iota // 0 reports
	TierLow                // 1–20
	TierMedium             // 21–100
	TierHigh               // 101+
)

// TierFor maps a report count to a tier.
func TierFor(reports int) Tier {
	switch {
	case reports <= 0:
		return TierQuiet
	case reports <= 20:
		return TierLow
	case reports <= 100:
		return TierMedium
	default:
		return TierHigh
	}
}

// Dots returns a 0–3 bullet string for a tier, suitable as a tab-label suffix.
func (t Tier) Dots() string {
	switch t {
	case TierLow:
		return "\u2022"
	case TierMedium:
		return "\u2022\u2022"
	case TierHigh:
		return "\u2022\u2022\u2022"
	}
	return ""
}
