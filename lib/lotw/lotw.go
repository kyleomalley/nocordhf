// Package lotw is a minimal client for ARRL's Logbook of the World QSL
// download service. It fetches the operator's confirmed-QSO ADIF report and
// exposes the subset of fields we need for map overlays (call, band, grid).
// Raw responses are cached to disk so incremental pulls can be made via
// qso_qslsince= without re-downloading the full history.
package lotw

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kyleomalley/nocordhf/lib/logging"
)

const (
	// reportURL is the LoTW ADIF-report endpoint. Authentication is by query
	// parameter (login=, password=) per the ARRL spec.
	reportURL = "https://lotw.arrl.org/lotwuser/lotwreport.adi"

	httpTimeout = 60 * time.Second
)

// QSL is one LoTW contact, reduced to the fields we use. Confirmed=true
// means QSL_RCVD=Y in the ADIF (we have a two-way LoTW match); Confirmed=false
// means we've logged the QSO but no QSL has come back yet.
type QSL struct {
	Call      string    // their callsign (uppercase)
	Band      string    // e.g. "20m"
	Grid      string    // their grid square (uppercase, up to 6 chars)
	QSLDate   time.Time // LoTW QSL received date, if parseable
	Confirmed bool      // true when QSL_RCVD=Y
}

// Client fetches and caches LoTW QSL reports.
type Client struct {
	Username string
	Password string

	cacheDir string
	http     *http.Client

	mu      sync.Mutex
	lastRun time.Time
}

// New creates a Client caching under os.UserCacheDir()/nocordhf/lotw.
func New(username, password string) (*Client, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("user cache dir: %w", err)
	}
	dir := filepath.Join(base, "nocordhf", "lotw")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache: %w", err)
	}
	// Cookie jar — ARRL's login-page error text insists on cookies even though
	// the documented adi query endpoint uses only login/password params. Cheap
	// to enable and matches browser behaviour.
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookiejar: %w", err)
	}
	// Force HTTP/1.1 — ARRL's Apache front-end serves the login page instead
	// of the ADIF when Go negotiates HTTP/2 via ALPN. Disable transparent
	// gzip handling so the raw body matches what curl sees.
	tr := &http.Transport{
		TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{},
		DisableCompression: true,
	}
	return &Client{
		Username: username,
		Password: password,
		cacheDir: dir,
		http: &http.Client{
			Timeout:   httpTimeout,
			Jar:       jar,
			Transport: tr,
			// Log each redirect so we can see ARRL's hop chain on rejection.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				logging.L.Infow("lotw redirect",
					"from", via[len(via)-1].URL.String(),
					"to", req.URL.String(),
					"depth", len(via))
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}, nil
}

// Configured reports whether the client has non-empty credentials.
func (c *Client) Configured() bool {
	return c != nil && strings.TrimSpace(c.Username) != "" && strings.TrimSpace(c.Password) != ""
}

// syncState persists the last-successful-sync timestamp so we can pull only
// new confirmations on subsequent runs.
type syncState struct {
	LastSync time.Time `json:"last_sync"`
}

func (c *Client) statePath() string   { return filepath.Join(c.cacheDir, "state.json") }
func (c *Client) historyPath() string { return filepath.Join(c.cacheDir, "qsls.json") }

func (c *Client) readState() syncState {
	var s syncState
	if data, err := os.ReadFile(c.statePath()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

func (c *Client) writeState(s syncState) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.WriteFile(c.statePath(), data, 0o644)
}

func (c *Client) readCachedQSLs() []QSL {
	data, err := os.ReadFile(c.historyPath())
	if err != nil {
		return nil
	}
	var out []QSL
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	// Legacy-cache migration: before the Confirmed field existed, the cache
	// only held QSL_RCVD=Y records, so entries without Confirmed=true can be
	// safely promoted rather than losing existing red squares until the next
	// full sync.
	anyConfirmed := false
	for i := range out {
		if out[i].Confirmed {
			anyConfirmed = true
			break
		}
	}
	if !anyConfirmed && len(out) > 0 {
		for i := range out {
			out[i].Confirmed = true
		}
	}
	return out
}

func (c *Client) writeCachedQSLs(qsls []QSL) {
	data, err := json.Marshal(qsls)
	if err != nil {
		return
	}
	_ = os.WriteFile(c.historyPath(), data, 0o644)
}

// SyncResult reports the outcome of a Sync/SyncFull call.
type SyncResult struct {
	QSLs     []QSL // merged set of LoTW records (confirmed + unconfirmed), backs map overlay
	QSOCount int   // total QSO records returned by LoTW this fetch (including unconfirmed)
	Fresh    int   // newly-confirmed QSLs pulled by this sync
}

// Sync does an incremental pull — asks LoTW for QSLs confirmed since the last
// successful sync. Use SyncFull to force a full history refetch.
func (c *Client) Sync(ctx context.Context) (SyncResult, error) {
	return c.sync(ctx, false)
}

// SyncFull does a full history pull — used by the "Sync Now" button so the
// user always gets a current count regardless of the stored state.
func (c *Client) SyncFull(ctx context.Context) (SyncResult, error) {
	return c.sync(ctx, true)
}

func (c *Client) sync(ctx context.Context, forceFull bool) (SyncResult, error) {
	if !c.Configured() {
		return SyncResult{QSLs: c.readCachedQSLs()}, nil
	}

	state := c.readState()
	since := state.LastSync
	if forceFull {
		since = time.Time{}
	}
	cached := c.readCachedQSLs()

	// LoTW's ADIF endpoint is bimodal: qso_qsl=yes returns *only* matched QSLs
	// and honours qso_qslsince; qso_qsl=no returns *only* uploaded QSOs
	// without a match and honours qso_qsorxsince. Fetch both and merge, so the
	// cache contains confirmed and unconfirmed records side-by-side.
	qslBody, err := c.fetchReport(ctx, since, true)
	if err != nil {
		logging.L.Warnw("lotw sync failed (qsl leg); returning cached",
			"err", err, "cached", len(cached))
		return SyncResult{QSLs: cached}, err
	}
	qsoBody, err := c.fetchReport(ctx, since, false)
	if err != nil {
		logging.L.Warnw("lotw sync failed (qso leg); falling back to qsl-only",
			"err", err)
		qsoBody = ""
	}

	qslCount, qslRecs := parseADIF(qslBody)
	qsoCount, qsoRecs := parseADIF(qsoBody)
	totalQSOs := qslCount + qsoCount
	// The qsl=yes leg already marks Confirmed via QSL_RCVD=Y; the qsl=no leg
	// is always unconfirmed.
	for i := range qsoRecs {
		qsoRecs[i].Confirmed = false
	}
	fresh := append(qslRecs, qsoRecs...)
	merged := mergeQSLs(cached, fresh)
	if forceFull {
		// On a full refetch we trust LoTW's set over the cache.
		merged = fresh
	}

	c.writeCachedQSLs(merged)
	c.writeState(syncState{LastSync: time.Now().UTC()})
	c.mu.Lock()
	c.lastRun = time.Now().UTC()
	c.mu.Unlock()

	confirmed := 0
	for _, q := range fresh {
		if q.Confirmed {
			confirmed++
		}
	}
	logging.L.Infow("lotw sync ok",
		"full", forceFull,
		"since", since.Format(time.RFC3339),
		"qsl_leg", qslCount,
		"qso_leg", qsoCount,
		"qsos_total", totalQSOs,
		"fresh_records", len(fresh),
		"fresh_confirmed", confirmed,
		"total_records", len(merged))
	return SyncResult{QSLs: merged, QSOCount: totalQSOs, Fresh: confirmed}, nil
}

// LoadCached returns whatever confirmed-QSL set is currently on disk.
func (c *Client) LoadCached() []QSL {
	return c.readCachedQSLs()
}

// LastRun returns the in-memory last-sync time for this process; zero if the
// process has not yet run Sync.
func (c *Client) LastRun() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRun
}

// fetchReport pulls one flavour of LoTW's ADIF report. qslsOnly=true maps to
// qso_qsl=yes (matched confirmations, filterable by qso_qslsince); false maps
// to qso_qsl=no (uploaded-but-unmatched QSOs, filterable by qso_qsorxsince).
func (c *Client) fetchReport(ctx context.Context, since time.Time, qslsOnly bool) (string, error) {
	// Preserve the login's exact case — LoTW is case-sensitive on the login
	// parameter and will reject a callsign submitted in the wrong case, even
	// though the website login form is case-insensitive.
	username := strings.TrimSpace(c.Username)
	password := strings.TrimSpace(c.Password)

	// Count non-ASCII chars in the password; LoTW's web form and the adi
	// endpoint may url-decode differently for multi-byte characters.
	nonASCII := 0
	for _, r := range password {
		if r > 127 {
			nonASCII++
		}
	}
	logging.L.Infow("lotw fetchReport",
		"login", username,
		"pw_len", len(password),
		"pw_nonascii", nonASCII,
		"since", since.Format(time.RFC3339))

	q := url.Values{}
	q.Set("login", username)
	q.Set("password", password)
	q.Set("qso_query", "1")
	q.Set("qso_qsldetail", "yes") // include QSL-detail fields
	q.Set("qso_mydetail", "no")
	q.Set("qso_withown", "yes") // include our own callsign field
	// Anchor "since" at a date well before any amateur LoTW history when we
	// don't have a prior sync to key off of — ARRL defaults to "now".
	qsoSince := since
	if qsoSince.IsZero() {
		qsoSince = time.Date(1945, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	stamp := qsoSince.UTC().Format("2006-01-02")
	if qslsOnly {
		q.Set("qso_qsl", "yes")
		q.Set("qso_qslsince", stamp) // ignored unless qso_qsl=yes
	} else {
		q.Set("qso_qsl", "no")
		q.Set("qso_qsorxsince", stamp) // ignored unless qso_qsl=no
	}

	// GET with query params — documented by ARRL. Password is url-encoded.
	fullURL := reportURL + "?" + q.Encode()
	redacted := url.Values{}
	for k, v := range q {
		if k == "password" {
			redacted.Set(k, "***")
			continue
		}
		redacted[k] = v
	}
	logging.L.Infow("lotw GET", "url", reportURL+"?"+redacted.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return "", err
	}
	// Use a browser-like UA; the ARRL error page explicitly mentions
	// browsers/cookies, and some Apache mod_security rules block the
	// default Go UA.
	req.Header.Set("User-Agent", "Mozilla/5.0 (nocordhf LoTW client)")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("lotw GET: %w", err)
	}
	defer resp.Body.Close()
	logging.L.Infow("lotw response",
		"status", resp.Status,
		"final_host", resp.Request.URL.Host,
		"final_path", resp.Request.URL.Path,
		"proto", resp.Proto,
		"content_type", resp.Header.Get("Content-Type"),
		"content_length", resp.Header.Get("Content-Length"),
		"has_cookie", len(resp.Header.Values("Set-Cookie")) > 0)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("lotw HTTP %d: %s", resp.StatusCode,
			strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("lotw read: %w", err)
	}
	bodyStr := string(body)
	// LoTW returns an ARRL error page for bad credentials with HTTP 200.
	if idx := strings.Index(bodyStr, "Username/password incorrect"); idx >= 0 {
		// Log a window around the error so we can see any nearby hint
		// (e.g. "user not found", "account locked", etc).
		start := idx - 200
		if start < 0 {
			start = 0
		}
		end := idx + 400
		if end > len(bodyStr) {
			end = len(bodyStr)
		}
		logging.L.Warnw("lotw credentials rejected",
			"login", username,
			"body_len", len(bodyStr),
			"error_ctx", bodyStr[start:end])
		return "", fmt.Errorf("lotw: credentials rejected")
	}
	// If the body doesn't look like ADIF at all, surface the first bit of it
	// so we can diagnose other auth/handler errors.
	if !strings.Contains(strings.ToUpper(bodyStr), "<EOH>") &&
		!strings.Contains(strings.ToUpper(bodyStr), "<CALL:") {
		snippet := strings.TrimSpace(bodyStr)
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		logging.L.Warnw("lotw non-ADIF response",
			"login", username, "body_len", len(bodyStr), "body_head", snippet)
		return "", fmt.Errorf("lotw: unexpected response: %s", snippet)
	}
	return bodyStr, nil
}

// parseADIF walks a LoTW ADIF body. qsoCount is the total record count (any
// record with CALL set). records is every QSO that has a grid square —
// Confirmed=true when QSL_RCVD=Y, false for logged-but-unconfirmed contacts.
func parseADIF(body string) (qsoCount int, records []QSL) {
	// Skip the header.
	upper := strings.ToUpper(body)
	if i := strings.Index(upper, "<EOH>"); i >= 0 {
		body = body[i+5:]
		upper = upper[i+5:]
	}

	for {
		eor := strings.Index(upper, "<EOR>")
		if eor < 0 {
			break
		}
		chunk := body[:eor]
		chunkUpper := upper[:eor]
		body = body[eor+5:]
		upper = upper[eor+5:]

		fields := parseADIFFields(chunk, chunkUpper)
		call := strings.ToUpper(strings.TrimSpace(fields["CALL"]))
		if call == "" {
			continue
		}
		qsoCount++
		band := strings.ToLower(strings.TrimSpace(fields["BAND"]))
		grid := strings.ToUpper(strings.TrimSpace(fields["GRIDSQUARE"]))
		if grid == "" {
			continue
		}
		q := QSL{
			Call:      call,
			Band:      band,
			Grid:      grid,
			Confirmed: strings.EqualFold(fields["QSL_RCVD"], "Y"),
		}
		if ts := parseQSLDate(fields["QSLRDATE"]); !ts.IsZero() {
			q.QSLDate = ts
		}
		records = append(records, q)
	}
	return qsoCount, records
}

func parseQSLDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if len(s) != 8 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("20060102", s, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

// mergeQSLs returns the union of prior and fresh, de-duplicated on
// (call, band, grid). Fresh wins on field values, but Confirmed is sticky —
// once a pair has been confirmed we don't regress it to unconfirmed.
func mergeQSLs(prior, fresh []QSL) []QSL {
	type key struct{ call, band, grid string }
	idx := make(map[key]QSL, len(prior)+len(fresh))
	for _, q := range prior {
		idx[key{q.Call, q.Band, q.Grid}] = q
	}
	for _, q := range fresh {
		k := key{q.Call, q.Band, q.Grid}
		if prev, ok := idx[k]; ok && prev.Confirmed {
			q.Confirmed = true
			if q.QSLDate.IsZero() {
				q.QSLDate = prev.QSLDate
			}
		}
		idx[k] = q
	}
	out := make([]QSL, 0, len(idx))
	for _, q := range idx {
		out = append(out, q)
	}
	return out
}

// parseADIFFields is a lightweight clone of the adif package's parser — kept
// local so the lotw client does not depend on ui-visible ADIF fields.
func parseADIFFields(chunk, chunkUpper string) map[string]string {
	fields := map[string]string{}
	pos := 0
	for pos < len(chunkUpper) {
		lt := strings.IndexByte(chunkUpper[pos:], '<')
		if lt < 0 {
			break
		}
		lt += pos
		gt := strings.IndexByte(chunkUpper[lt:], '>')
		if gt < 0 {
			break
		}
		gt += lt + 1

		tag := chunkUpper[lt+1 : gt-1]
		pos = gt

		colon := strings.IndexByte(tag, ':')
		if colon < 0 {
			continue
		}
		name := tag[:colon]
		lenStr := tag[colon+1:]
		if c := strings.IndexByte(lenStr, ':'); c >= 0 {
			lenStr = lenStr[:c]
		}
		var length int
		if _, err := fmt.Sscanf(lenStr, "%d", &length); err != nil || length < 0 || pos+length > len(chunk) {
			continue
		}
		fields[name] = chunk[pos : pos+length]
		pos += length
	}
	return fields
}
