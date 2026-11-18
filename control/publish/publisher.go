package publish

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/golib/objstore"
)

// Options configures a Publisher.
type Options struct {
	DB     *db.DB
	Store  objstore.Store
	Logger *slog.Logger
	Now    func() time.Time

	// Debounce is how long the publisher waits after a change before it
	// publishes, so a burst of changes becomes one publish. The contract
	// allows at most 2 s between change and publication.
	Debounce time.Duration
	// Interval is the period of the unconditional full republish (default
	// 5 min) that repairs objects deleted or edited by hand.
	Interval time.Duration
	// RetryMin/RetryMax bound the backoff after a failed publish.
	RetryMin, RetryMax time.Duration
	// OpTimeout bounds one store request.
	OpTimeout time.Duration
	// Parallel is the number of concurrent requests for per-token objects.
	Parallel int
}

// Publisher regenerates the published snapshots from the database. The
// database is the source of truth: every publish rebuilds every document
// from it; nothing is patched incrementally.
type Publisher struct {
	opt   Options
	start time.Time
	wake  chan struct{}

	runMu sync.Mutex // serializes publishes

	mu            sync.Mutex
	pendingSince  time.Time // oldest change not yet covered by a successful publish
	inflightSince time.Time // same, for the publish that is running
	fullDue       bool

	// Guarded by runMu.
	reconciled map[string]bool   // object keys whose store version was checked
	written    map[string]string // object key → hash of the bytes last written
	lastActive map[string]bool   // tokens that had a ruleset after the last publish

	okCount      atomic.Uint64
	failCount    atomic.Uint64
	objectsCount atomic.Uint64
	lastSuccess  atomic.Int64 // unix ms
}

// New returns a publisher; call Run to start it.
func New(opt Options) *Publisher {
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Debounce < 0 {
		opt.Debounce = 0
	}
	if opt.Interval <= 0 {
		opt.Interval = 5 * time.Minute
	}
	if opt.RetryMin <= 0 {
		opt.RetryMin = time.Second
	}
	if opt.RetryMax < opt.RetryMin {
		opt.RetryMax = max(30*time.Second, opt.RetryMin)
	}
	if opt.OpTimeout <= 0 {
		opt.OpTimeout = 30 * time.Second
	}
	if opt.Parallel <= 0 {
		opt.Parallel = 8
	}
	return &Publisher{
		opt:        opt,
		start:      opt.Now(),
		wake:       make(chan struct{}, 1),
		fullDue:    true,
		reconciled: map[string]bool{},
		written:    map[string]string{},
		lastActive: map[string]bool{},
	}
}

// Notify tells the publisher that the database changed. It never blocks.
func (p *Publisher) Notify() {
	p.mu.Lock()
	if p.pendingSince.IsZero() {
		p.pendingSince = p.opt.Now()
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// LagSeconds is the age of the oldest change (or failed publish) that no
// successful publish has covered yet; 0 when the store is up to date.
func (p *Publisher) LagSeconds() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	oldest := p.pendingSince
	if !p.inflightSince.IsZero() && (oldest.IsZero() || p.inflightSince.Before(oldest)) {
		oldest = p.inflightSince
	}
	if oldest.IsZero() {
		return 0
	}
	return max(0, p.opt.Now().Sub(oldest).Seconds())
}

// Stats are counters for metrics.
type Stats struct {
	Published, Failed, Objects uint64
	LastSuccessMillis          int64
}

// Stats returns the publisher's counters.
func (p *Publisher) Stats() Stats {
	return Stats{Published: p.okCount.Load(), Failed: p.failCount.Load(), Objects: p.objectsCount.Load(), LastSuccessMillis: p.lastSuccess.Load()}
}

func (p *Publisher) dirty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.pendingSince.IsZero() || p.fullDue
}

// Run publishes once at start, then after every change (debounced), every
// Interval in full, and with backoff after a failure, until ctx is done.
func (p *Publisher) Run(ctx context.Context) {
	ticker := time.NewTicker(p.opt.Interval)
	defer ticker.Stop()
	backoff := time.Duration(0)
	var retry <-chan time.Time
	for {
		if p.dirty() && retry == nil {
			if err := p.publish(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				if backoff == 0 {
					backoff = p.opt.RetryMin
				} else {
					backoff = min(backoff*2, p.opt.RetryMax)
				}
				p.opt.Logger.Warn("publish failed; the database stays authoritative and the publish is retried",
					"retry_in_ms", backoff.Milliseconds(), "lag_s", int(p.LagSeconds()), "err", err.Error())
				retry = time.After(backoff)
			} else {
				backoff = 0
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.mu.Lock()
			p.fullDue = true
			p.mu.Unlock()
		case <-retry:
			retry = nil
		case <-p.wake:
			// Debounce: let a burst of changes settle, then publish once.
			if p.opt.Debounce > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(p.opt.Debounce):
				}
			}
			select {
			case <-p.wake:
			default:
			}
			// A change is worth an immediate attempt even while backing off.
			retry = nil
		}
	}
}

// PublishNow runs one full publish synchronously (startup of the CLI
// commands, shutdown flush, tests).
func (p *Publisher) PublishNow(ctx context.Context) error {
	p.mu.Lock()
	p.fullDue = true
	p.mu.Unlock()
	return p.publish(ctx)
}

// Flush publishes if there is anything unpublished.
func (p *Publisher) Flush(ctx context.Context) error {
	if !p.dirty() {
		return nil
	}
	return p.publish(ctx)
}

func (p *Publisher) publish(ctx context.Context) error {
	p.runMu.Lock()
	defer p.runMu.Unlock()

	begin := p.opt.Now()
	p.mu.Lock()
	full := p.fullDue
	p.fullDue = false
	p.inflightSince = p.pendingSince
	p.pendingSince = time.Time{}
	p.mu.Unlock()

	n, err := p.publishAll(ctx, full)
	p.objectsCount.Add(uint64(n))

	p.mu.Lock()
	covered := p.inflightSince
	p.inflightSince = time.Time{}
	if err != nil {
		// What this publish was meant to cover is still unpublished. A
		// failed periodic republish counts too: the store may be stale.
		if covered.IsZero() {
			covered = begin
		}
		if p.pendingSince.IsZero() || covered.Before(p.pendingSince) {
			p.pendingSince = covered
		}
		p.fullDue = p.fullDue || full
	}
	p.mu.Unlock()

	if err != nil {
		p.failCount.Add(1)
		return err
	}
	p.okCount.Add(1)
	p.lastSuccess.Store(p.opt.Now().UnixMilli())
	p.opt.Logger.Debug("published", "full", full, "objects", n, "ms", p.opt.Now().Sub(begin).Milliseconds())
	return nil
}

func (p *Publisher) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.opt.OpTimeout)
}

// publishAll regenerates every document. Families fail independently: an
// error in one does not stop the others; the joined error makes the caller
// retry everything. It returns the number of objects written or deleted.
func (p *Publisher) publishAll(ctx context.Context, full bool) (int, error) {
	snap, err := loadSnapshot(ctx, p.opt.DB, p.opt.Now())
	if err != nil {
		return 0, err
	}
	open := snap.openProjects()
	var errs []error
	written := 0
	record := func(n int, err error) {
		written += n
		if err != nil {
			errs = append(errs, err)
		}
	}

	// Tombstones first: they are the only family with a legal deadline.
	record(p.publishTombstones(ctx, snap, full))

	// projects.json before tokens.json: a new project's settings (drop_keys
	// in particular) should be visible before its first token is accepted.
	projects := snap.projectsDoc()
	record(p.publishControl(ctx, ProjectsKey, projects, full, func(v, _ int64) any { projects.Version = v; return projects }))
	keys := snap.keysDoc()
	record(p.publishControl(ctx, KeysKey, keys, full, func(v, _ int64) any { keys.Version = v; return keys }))
	tokens := snap.tokensDoc(open)
	record(p.publishControl(ctx, TokensKey, tokens, full, func(v, at int64) any { tokens.Version, tokens.GeneratedAt = v, at; return tokens }))

	record(p.publishFlags(ctx, snap, open, full))

	return written, errors.Join(errs...)
}

// storeVersion reads the "version" (or "v") of an object already in the
// store; 0 when the object does not exist or is not a JSON document.
func (p *Publisher) storeVersion(ctx context.Context, key string) (int64, error) {
	cctx, cancel := p.opCtx(ctx)
	defer cancel()
	raw, _, err := p.opt.Store.Get(cctx, key, nil)
	if errors.Is(err, objstore.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("publish: read %s: %w", key, err)
	}
	var doc struct {
		Version int64 `json:"version"`
		V       int64 `json:"v"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return 0, nil
	}
	return max(doc.Version, doc.V), nil
}

// versionFor returns the version to publish content with hash under. The
// stored version is kept while the content is unchanged and bumped by one
// when it changes; floor is the highest version seen in the store, which the
// result always exceeds if the store is ahead of the database (a database
// restored from a backup must not reuse version numbers).
func (p *Publisher) versionFor(ctx context.Context, name, hash string, floor int64) (db.PublishState, error) {
	unchanged := func(st db.PublishState) bool { return st.Version > 0 && st.Hash == hash && st.Version >= floor }
	// The common case (nothing changed) needs no write transaction.
	if st, err := db.GetPublishState(ctx, p.opt.DB.R(), name); err == nil && unchanged(st) {
		return st, nil
	}
	var out db.PublishState
	err := p.opt.DB.Write(ctx, func(tx *sql.Tx) error {
		st, err := db.GetPublishState(ctx, tx, name)
		if err != nil {
			return err
		}
		if unchanged(st) {
			out = st
			return nil
		}
		out = db.PublishState{Version: max(st.Version, floor) + 1, Hash: hash, GeneratedAt: p.opt.Now().UnixMilli()}
		return db.SetPublishState(ctx, tx, name, out)
	})
	if err != nil {
		return out, fmt.Errorf("publish: version of %s: %w", name, err)
	}
	return out, nil
}

// put writes an object unless (in an incremental publish) the same bytes
// were already written by this process.
func (p *Publisher) put(ctx context.Context, key string, body []byte, full bool) (bool, error) {
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	if !full && p.written[key] == hash {
		return false, nil
	}
	cctx, cancel := p.opCtx(ctx)
	defer cancel()
	if _, err := p.opt.Store.Put(cctx, key, body); err != nil {
		delete(p.written, key)
		return false, fmt.Errorf("publish: write %s: %w", key, err)
	}
	p.written[key] = hash
	return true, nil
}

func (p *Publisher) publishControl(ctx context.Context, key string, content any, full bool, finish func(version, generatedAt int64) any) (int, error) {
	hash, err := contentHash(content)
	if err != nil {
		return 0, fmt.Errorf("publish: encode %s: %w", key, err)
	}
	floor := int64(0)
	if !p.reconciled[key] {
		if floor, err = p.storeVersion(ctx, key); err != nil {
			return 0, err
		}
	}
	st, err := p.versionFor(ctx, key, hash, floor)
	if err != nil {
		return 0, err
	}
	p.reconciled[key] = true
	body, err := json.Marshal(finish(st.Version, st.GeneratedAt))
	if err != nil {
		return 0, fmt.Errorf("publish: encode %s: %w", key, err)
	}
	wrote, err := p.put(ctx, key, body, full)
	if err != nil || !wrote {
		return 0, err
	}
	return 1, nil
}

// forEach runs fn over items with bounded concurrency and joins the errors.
func forEach[T any](ctx context.Context, parallel int, items []T, fn func(context.Context, T) error) error {
	if len(items) == 0 {
		return nil
	}
	parallel = min(parallel, len(items))
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		next atomic.Int64
	)
	for w := 0; w < parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(items) || ctx.Err() != nil {
					return
				}
				if err := fn(ctx, items[i]); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// publishFlags writes flags/<token>.json for every active token of every
// open project and removes the rulesets of tokens that are gone or disabled.
func (p *Publisher) publishFlags(ctx context.Context, snap *snapshot, open map[uint32]bool, full bool) (int, error) {
	active := map[uint32][]db.Token{}
	activeSet := map[string]bool{}
	for _, t := range snap.tokens {
		if t.Status == "active" && open[t.ProjectID] {
			active[t.ProjectID] = append(active[t.ProjectID], t)
			activeSet[t.Token] = true
		}
	}
	projectIDs := make([]uint32, 0, len(active))
	for pid := range active {
		projectIDs = append(projectIDs, pid)
	}
	sort.Slice(projectIDs, func(i, j int) bool { return projectIDs[i] < projectIDs[j] })

	type job struct {
		key  string
		body []byte
	}
	var jobs []job
	var errs []error
	for _, pid := range projectIDs {
		doc := Ruleset(snap.flags[pid])
		hash, err := contentHash(doc)
		if err != nil {
			errs = append(errs, fmt.Errorf("publish: encode flags of project %d: %w", pid, err))
			continue
		}
		// Rulesets written by an earlier process may carry a higher v than
		// the database remembers (restored backup); check each object once.
		// Tokens created after this process started cannot have one.
		var unchecked []string
		for _, t := range active[pid] {
			key := FlagsKey(t.Token)
			if !p.reconciled[key] && t.CreatedAt < p.start.UnixMilli() {
				unchecked = append(unchecked, key)
			}
		}
		var floor atomic.Int64
		if err := forEach(ctx, p.opt.Parallel, unchecked, func(ctx context.Context, key string) error {
			v, err := p.storeVersion(ctx, key)
			if err != nil {
				return err
			}
			for {
				cur := floor.Load()
				if v <= cur || floor.CompareAndSwap(cur, v) {
					return nil
				}
			}
		}); err != nil {
			errs = append(errs, err)
			continue
		}
		st, err := p.versionFor(ctx, flagsStateName(pid), hash, floor.Load())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, t := range active[pid] {
			p.reconciled[FlagsKey(t.Token)] = true
		}
		doc.V = st.Version
		body, err := json.Marshal(doc)
		if err != nil {
			errs = append(errs, fmt.Errorf("publish: encode flags of project %d: %w", pid, err))
			continue
		}
		for _, t := range active[pid] {
			jobs = append(jobs, job{key: FlagsKey(t.Token), body: body})
		}
	}

	var putMu sync.Mutex
	written := 0
	if err := forEach(ctx, p.opt.Parallel, jobs, func(ctx context.Context, j job) error {
		sum := sha256.Sum256(j.body)
		hash := hex.EncodeToString(sum[:])
		putMu.Lock()
		same := !full && p.written[j.key] == hash
		putMu.Unlock()
		if same {
			return nil
		}
		cctx, cancel := p.opCtx(ctx)
		defer cancel()
		_, err := p.opt.Store.Put(cctx, j.key, j.body)
		putMu.Lock()
		defer putMu.Unlock()
		if err != nil {
			delete(p.written, j.key)
			return fmt.Errorf("publish: write %s: %w", j.key, err)
		}
		p.written[j.key] = hash
		written++
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	// Remove rulesets that no longer belong to an active token. A full
	// publish lists the prefix (which also catches leftovers of an earlier
	// process); an incremental one only looks at what changed since the last.
	stale := map[string]bool{}
	for token := range p.lastActive {
		if !activeSet[token] {
			stale[token] = true
		}
	}
	if full {
		cctx, cancel := p.opCtx(ctx)
		objs, err := p.opt.Store.List(cctx, flagsPrefix)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("publish: list %s: %w", flagsPrefix, err))
		}
		for _, o := range objs {
			token, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, flagsPrefix), ".json")
			// Only objects that look like ours are touched.
			if ok && auth.LooksLikeWriteToken(token) && !activeSet[token] {
				stale[token] = true
			}
		}
	}
	staleList := make([]string, 0, len(stale))
	for token := range stale {
		staleList = append(staleList, token)
	}
	sort.Strings(staleList)
	removed := map[string]bool{}
	if err := forEach(ctx, p.opt.Parallel, staleList, func(ctx context.Context, token string) error {
		cctx, cancel := p.opCtx(ctx)
		defer cancel()
		key := FlagsKey(token)
		if err := p.opt.Store.Delete(cctx, key); err != nil {
			return fmt.Errorf("publish: delete %s: %w", key, err)
		}
		putMu.Lock()
		removed[token] = true
		delete(p.written, key)
		delete(p.reconciled, key)
		written++
		putMu.Unlock()
		return nil
	}); err != nil {
		errs = append(errs, err)
	}
	// Remember which tokens may have a ruleset in the store: the active
	// ones plus stale ones whose removal failed (retried next time).
	next := make(map[string]bool, len(activeSet))
	for token := range activeSet {
		next[token] = true
	}
	for token := range stale {
		if !removed[token] {
			next[token] = true
		}
	}
	p.lastActive = next
	return written, errors.Join(errs...)
}

// publishTombstones writes the tombstone of every deletion request that has
// none yet, per project in ts_ms order (a later tombstone must never become
// visible before an earlier one: the engine's watermark would skip it). A
// full publish also restores tombstones that disappeared from the store.
func (p *Publisher) publishTombstones(ctx context.Context, snap *snapshot, full bool) (int, error) {
	existing := map[string]bool{}
	haveListing := false
	if full && len(snap.deletions) > 0 {
		cctx, cancel := p.opCtx(ctx)
		objs, err := p.opt.Store.List(cctx, tombPrefix)
		cancel()
		if err != nil {
			return 0, fmt.Errorf("publish: list %s: %w", tombPrefix, err)
		}
		haveListing = true
		for _, o := range objs {
			existing[o.Key] = true
		}
	}
	written := 0
	var errs []error
	failed := map[uint32]bool{}
	for _, d := range snap.deletions { // ordered by project, ts_ms
		if failed[d.ProjectID] {
			continue
		}
		key := TombstoneKey(d.ProjectID, d.TSMillis, d.ID)
		need := d.PublishedAt == 0 || (haveListing && !existing[key])
		if !need {
			continue
		}
		body, err := json.Marshal(Tombstone(d))
		if err != nil {
			errs = append(errs, fmt.Errorf("publish: encode %s: %w", key, err))
			failed[d.ProjectID] = true
			continue
		}
		cctx, cancel := p.opCtx(ctx)
		_, err = p.opt.Store.Put(cctx, key, body)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("publish: write %s: %w", key, err))
			failed[d.ProjectID] = true
			continue
		}
		written++
		if d.PublishedAt == 0 {
			now := p.opt.Now().UnixMilli()
			if err := p.opt.DB.Write(ctx, func(tx *sql.Tx) error { return db.MarkDeletionPublished(ctx, tx, d.ID, now) }); err != nil {
				errs = append(errs, fmt.Errorf("publish: mark %s published: %w", d.ID, err))
				failed[d.ProjectID] = true
			}
		}
	}
	return written, errors.Join(errs...)
}
