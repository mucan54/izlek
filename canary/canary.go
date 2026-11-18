// Package canary is the end-to-end "no silent loss" probe (docs/design.md
// §16.3): it sends sequence-numbered events through the public ingestion
// endpoint and reads them back through the query API. A missing or duplicated
// sequence number is an alarm.
package canary

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/mucan54/izlek/canary/la1"
)

// Config is everything the worker needs. Zero values get defaults in New.
type Config struct {
	IngestURL string // e.g. http://ingest:8080
	Token     string // pk_… of the dedicated canary project
	QueryURL  string // e.g. http://query:8081
	QueryKey  string // sk_… with the "query" scope

	Interval time.Duration // between sends (default 60s)
	PerTick  int           // events per batch (default 5)
	Grace    time.Duration // age after which an acked event must be queryable (default 180s)
	// ResendEvery > 0 re-sends every n-th acked batch byte-for-byte to prove
	// idempotency end to end (0 = off).
	ResendEvery int
	StatePath   string // optional JSON file so a restart continues the same run

	HTTP *http.Client
	Now  func() time.Time
	Logf func(format string, args ...any)
}

// State is persisted between restarts.
type State struct {
	Run      string `json:"run"`
	Acked    int64  `json:"acked"`     // highest contiguous acked sequence number
	StartDay string `json:"start_day"` // UTC day of the first send
	Batches  int64  `json:"batches"`
}

// Stats is the externally visible health of the run.
type Stats struct {
	Run              string
	Sent             int64 // acked events
	SendErrors       int64
	VerifyErrors     int64
	Expected         int64 // events older than Grace at the last verification
	Visible          int64 // rows seen for those events
	Missing          int64
	Duplicates       int64
	LastFreshnessSec float64 // ack → first seen, for the newest verified batch
	LastVerifyUnix   int64
	Healthy          bool
}

type ackInfo struct {
	firstSeq int64
	lastSeq  int64
	at       time.Time
}

// Canary is safe for one Run loop plus concurrent Stats readers.
type Canary struct {
	cfg Config

	mu      sync.Mutex
	st      State
	stats   Stats
	pending []ackInfo // acked batches not yet seen by a verification
}

// New validates the config and loads (or starts) the run state.
func New(cfg Config) (*Canary, error) {
	if cfg.IngestURL == "" || cfg.Token == "" || cfg.QueryURL == "" || cfg.QueryKey == "" {
		return nil, errors.New("canary: IngestURL, Token, QueryURL and QueryKey are required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.PerTick <= 0 {
		cfg.PerTick = 5
	}
	if cfg.PerTick > 500 {
		return nil, errors.New("canary: PerTick must be <= 500 (batch record limit)")
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 180 * time.Second
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	c := &Canary{cfg: cfg}
	if cfg.StatePath != "" {
		if raw, err := os.ReadFile(cfg.StatePath); err == nil {
			if err := json.Unmarshal(raw, &c.st); err != nil {
				return nil, fmt.Errorf("canary: state file %s: %w", cfg.StatePath, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if c.st.Run == "" {
		now := cfg.Now().UTC()
		c.st = State{Run: fmt.Sprintf("%s-%06x", now.Format("20060102T150405"), rand.Uint32()&0xffffff),
			StartDay: now.Format("2006-01-02")}
	}
	c.stats.Run = c.st.Run
	c.stats.Sent = c.st.Acked
	c.stats.Healthy = true
	if c.st.Acked > 0 {
		// Ack times are not persisted: after a restart give everything sent so
		// far one grace period before it is required to be visible.
		c.pending = []ackInfo{{firstSeq: 1, lastSeq: c.st.Acked, at: cfg.Now()}}
	}
	return c, nil
}

// Stats returns a snapshot.
func (c *Canary) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// State returns the persisted run state.
func (c *Canary) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st
}

func (c *Canary) saveState() {
	if c.cfg.StatePath == "" {
		return
	}
	raw, _ := json.Marshal(c.st)
	tmp := c.cfg.StatePath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(c.cfg.StatePath), 0o755); err == nil {
		if err := os.WriteFile(tmp, raw, 0o644); err == nil {
			_ = os.Rename(tmp, c.cfg.StatePath)
		}
	}
}

// derive16 makes ids a pure function of (run, kind, n) so a restart that
// repeats a send produces byte-identical batches: the server's batch_id LRU
// or uuid dedupe then absorbs the repeat.
func derive16(run, kind string, n int64) (out [16]byte) {
	h := sha256.New()
	h.Write([]byte("la-canary\x00" + run + "\x00" + kind + "\x00"))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	h.Write(b[:])
	copy(out[:], h.Sum(nil))
	out[6] = (out[6] & 0x0f) | 0x80
	out[8] = (out[8] & 0x3f) | 0x80
	return
}

// buildBatch encodes sequence numbers first..first+n-1. ts/sent_at are fixed
// per batch (derived from `at`) so retries of the same batch are identical.
func (c *Canary) buildBatch(first int64, n int, at time.Time) []byte {
	ms := uint64(at.UnixMilli())
	h := la1.Header{
		BatchID: derive16(c.st.Run, "batch", first),
		SentAt:  ms,
		SDK:     "canary/1",
		AnonID:  "canary-" + c.st.Run,
		Context: []la1.Prop{la1.S("platform", "canary")},
	}
	recs := make([]la1.Record, 0, n)
	for i := 0; i < n; i++ {
		seq := first + int64(i)
		recs = append(recs, la1.Record{
			Kind: 0, UUID: derive16(c.st.Run, "event", seq), TS: ms, Name: "canary",
			Props: []la1.Prop{la1.S("run", c.st.Run), la1.I("seq", seq)},
		})
	}
	return la1.Encode(&h, recs)
}

var errPermanent = errors.New("permanent")

func (c *Canary) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.IngestURL+"/v1/e", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		return fmt.Errorf("ingest answered %d", resp.StatusCode)
	default:
		// 400/401/403/413: retrying cannot help; surface it loudly.
		return fmt.Errorf("%w: ingest answered %d", errPermanent, resp.StatusCode)
	}
}

// SendOnce sends the next batch, retrying the *same bytes* until it is
// acknowledged (or ctx ends / the error is permanent).
func (c *Canary) SendOnce(ctx context.Context) error {
	c.mu.Lock()
	first := c.st.Acked + 1
	n := c.cfg.PerTick
	c.mu.Unlock()
	body := c.buildBatch(first, n, c.cfg.Now())
	backoff := time.Second
	for {
		err := c.post(ctx, body)
		if err == nil {
			break
		}
		c.mu.Lock()
		c.stats.SendErrors++
		c.mu.Unlock()
		c.cfg.Logf("canary: send seq %d..%d failed: %v", first, first+int64(n)-1, err)
		if errors.Is(err, errPermanent) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)+1))):
		}
		if backoff < 5*time.Minute {
			backoff *= 2
		}
	}
	now := c.cfg.Now()
	c.mu.Lock()
	c.st.Acked = first + int64(n) - 1
	c.st.Batches++
	resend := c.cfg.ResendEvery > 0 && c.st.Batches%int64(c.cfg.ResendEvery) == 0
	c.stats.Sent = c.st.Acked
	c.pending = append(c.pending, ackInfo{firstSeq: first, lastSeq: c.st.Acked, at: now})
	c.saveState()
	c.mu.Unlock()
	if resend {
		// Deliberate duplicate delivery; any outcome other than a doubled row is fine.
		if err := c.post(ctx, body); err != nil {
			c.cfg.Logf("canary: idempotency resend failed (harmless): %v", err)
		}
	}
	return nil
}

type sqlResp struct {
	Rows  [][]json.RawMessage `json:"rows"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Canary) sql(ctx context.Context, q string) ([][]json.RawMessage, error) {
	today := c.cfg.Now().UTC().Format("2006-01-02")
	c.mu.Lock()
	from := c.st.StartDay
	c.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"from": from, "to": today, "sql": q})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.QueryURL+"/q/sql", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.QueryKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var out sqlResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("query answered %d with non-JSON body", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		if out.Error != nil {
			return nil, fmt.Errorf("query answered %d: %s", resp.StatusCode, out.Error.Message)
		}
		return nil, fmt.Errorf("query answered %d", resp.StatusCode)
	}
	return out.Rows, nil
}

func num(raw json.RawMessage) (int64, bool) {
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

func sqlQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, s[i])
	}
	return string(append(out, '\''))
}

// Verify checks every event of the run that was acked more than Grace ago:
// each sequence number must be visible exactly once. It re-checks the whole
// run every time, so data lost later (e.g. by a bad compaction) is caught too.
func (c *Canary) Verify(ctx context.Context) error {
	now := c.cfg.Now()
	c.mu.Lock()
	run := c.st.Run
	acked := c.st.Acked
	// Batches still inside the grace period (and everything after them) are
	// not required yet.
	expected := acked
	for _, p := range c.pending {
		if now.Sub(p.at) < c.cfg.Grace {
			expected = p.firstSeq - 1
			break
		}
	}
	c.mu.Unlock()
	if expected < 0 {
		expected = 0
	}

	fail := func(err error) error {
		c.mu.Lock()
		c.stats.VerifyErrors++
		c.mu.Unlock()
		return err
	}
	where := fmt.Sprintf("event = 'canary' AND prop(props, 'run') = %s", sqlQuote(run))
	rows, err := c.sql(ctx, "SELECT count(*), count(DISTINCT prop_num(props, 'seq')), "+
		"coalesce(max(prop_num(props, 'seq')), 0) FROM events WHERE "+where+
		fmt.Sprintf(" AND prop_num(props, 'seq') <= %d", expected))
	if err != nil {
		return fail(err)
	}
	if len(rows) != 1 || len(rows[0]) != 3 {
		return fail(fmt.Errorf("unexpected result shape from /q/sql"))
	}
	total, ok1 := num(rows[0][0])
	distinct, ok2 := num(rows[0][1])
	if !ok1 || !ok2 {
		return fail(fmt.Errorf("non-numeric result from /q/sql"))
	}
	missing := expected - distinct
	dups := total - distinct

	// Freshness: how far has visibility progressed into the not-yet-required tail?
	var freshness float64 = -1
	if tail, err := c.sql(ctx, "SELECT coalesce(max(prop_num(props, 'seq')), 0) FROM events WHERE "+where); err == nil &&
		len(tail) == 1 && len(tail[0]) == 1 {
		if maxSeen, ok := num(tail[0][0]); ok {
			c.mu.Lock()
			keep := c.pending[:0]
			for _, p := range c.pending {
				if p.lastSeq <= maxSeen {
					freshness = now.Sub(p.at).Seconds() // newest batch that became visible
				} else {
					keep = append(keep, p)
				}
			}
			// Visible batches no longer need a grace period.
			c.pending = keep
			c.mu.Unlock()
		}
	}

	if missing != 0 || dups != 0 {
		c.cfg.Logf("canary: run %s MISMATCH expected=%d distinct=%d total=%d (missing=%d duplicates=%d)%s",
			run, expected, distinct, total, missing, dups, c.detail(ctx, where, expected))
	}
	c.mu.Lock()
	c.stats.Expected = expected
	c.stats.Visible = total
	c.stats.Missing = missing
	c.stats.Duplicates = dups
	if freshness >= 0 {
		c.stats.LastFreshnessSec = freshness
	}
	c.stats.LastVerifyUnix = now.Unix()
	c.stats.Healthy = missing == 0 && dups == 0
	c.mu.Unlock()
	return nil
}

// detail lists up to 20 offending sequence numbers for the log line.
func (c *Canary) detail(ctx context.Context, where string, expected int64) string {
	rows, err := c.sql(ctx, "SELECT prop_num(props, 'seq') AS s, count(*) AS c FROM events WHERE "+where+
		fmt.Sprintf(" AND prop_num(props, 'seq') <= %d GROUP BY 1 ORDER BY 1 LIMIT 10000", expected))
	if err != nil || expected > 10000 {
		return ""
	}
	seen := map[int64]int64{}
	for _, r := range rows {
		if len(r) == 2 {
			s, _ := num(r[0])
			n, _ := num(r[1])
			seen[s] = n
		}
	}
	var missing, dup []int64
	for s := int64(1); s <= expected; s++ {
		switch n := seen[s]; {
		case n == 0:
			missing = append(missing, s)
		case n > 1:
			dup = append(dup, s)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	trim := func(v []int64) []int64 {
		if len(v) > 20 {
			return v[:20]
		}
		return v
	}
	return fmt.Sprintf(" missing_seqs=%v duplicate_seqs=%v", trim(missing), trim(dup))
}

// Run loops until ctx is cancelled: send every Interval, verify after each send.
func (c *Canary) Run(ctx context.Context) error {
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		if err := c.SendOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.cfg.Logf("canary: giving up on this tick: %v", err)
		}
		if err := c.Verify(ctx); err != nil && ctx.Err() == nil {
			c.cfg.Logf("canary: verification could not run: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Probe sends `batches` batches back to back and waits until every event is
// visible exactly once, or `timeout` passes. It is the one-shot mode used by
// deploy smoke tests and the end-to-end test. Returns the time from the last
// ack to full visibility.
func (c *Canary) Probe(ctx context.Context, batches int, timeout, poll time.Duration) (time.Duration, error) {
	for i := 0; i < batches; i++ {
		if err := c.SendOnce(ctx); err != nil {
			return 0, err
		}
	}
	lastAck := c.cfg.Now()
	saved := c.cfg.Grace
	c.cfg.Grace = time.Nanosecond // everything acked is required now
	defer func() { c.cfg.Grace = saved }()
	deadline := lastAck.Add(timeout)
	var lastErr error
	for {
		if err := c.Verify(ctx); err != nil {
			lastErr = err
		} else {
			lastErr = nil
			s := c.Stats()
			if s.Duplicates != 0 {
				return 0, fmt.Errorf("canary: %d duplicate rows", s.Duplicates)
			}
			if s.Missing == 0 && s.Expected == c.State().Acked {
				return c.cfg.Now().Sub(lastAck), nil
			}
		}
		if c.cfg.Now().After(deadline) {
			s := c.Stats()
			if lastErr != nil {
				return 0, fmt.Errorf("canary: timed out; last verification error: %w", lastErr)
			}
			return 0, fmt.Errorf("canary: timed out with %d of %d events missing", s.Missing, s.Expected)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Metrics renders the Prometheus text exposition for /metrics.
func (c *Canary) Metrics() string {
	s := c.Stats()
	healthy := 0
	if s.Healthy {
		healthy = 1
	}
	return fmt.Sprintf(`la_canary_sent_total %d
la_canary_send_errors_total %d
la_canary_verify_errors_total %d
la_canary_expected %d
la_canary_visible %d
la_canary_missing %d
la_canary_duplicates %d
la_canary_freshness_seconds %g
la_canary_last_verify_timestamp_seconds %d
la_canary_healthy %d
`, s.Sent, s.SendErrors, s.VerifyErrors, s.Expected, s.Visible, s.Missing, s.Duplicates,
		s.LastFreshnessSec, s.LastVerifyUnix, healthy)
}
