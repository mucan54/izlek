package publish

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/golib/objstore"
)

// DeletionGrace is the age after which a deletion request counts as done
// even if some day manifest has not caught up (spec/control.md §7).
const DeletionGrace = 8 * 24 * time.Hour

// Checker flips deletion requests from "pending" to "done" once the engine
// has applied their tombstones: every day manifest of the project reports
// tombstone_ts >= ts_ms (spec/manifest.md §2), or the request is 8 days old.
// A request whose tombstone has not reached the store yet never completes.
type Checker struct {
	DB     *db.DB
	Store  objstore.Store
	Logger *slog.Logger
	Now    func() time.Time

	mu sync.Mutex
	// Day manifests are immutable per version, so tombstone_ts is cached by
	// object key; each project's cache is replaced by every listing.
	cache map[uint32]map[string]int64
}

// dayManifest parses "data/p=<pid>/d=<day>/_m/<v:08>.json".
func dayManifest(key, prefix string) (day string, version int64, ok bool) {
	rest, found := strings.CutPrefix(key, prefix)
	if !found {
		return "", 0, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || !strings.HasPrefix(parts[0], "d=") || parts[1] != "_m" {
		return "", 0, false
	}
	name, found := strings.CutSuffix(parts[2], ".json")
	if !found || name == "" {
		return "", 0, false
	}
	v, err := strconv.ParseInt(name, 10, 64)
	if err != nil || v < 1 {
		return "", 0, false
	}
	return parts[0][2:], v, true
}

// minTombstoneTS returns the smallest tombstone_ts over the newest manifest
// of every day of the project (MaxInt64 when the project has no data).
func (c *Checker) minTombstoneTS(ctx context.Context, projectID uint32) (int64, error) {
	prefix := fmt.Sprintf("data/p=%d/", projectID)
	objs, err := c.Store.List(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("deletions: list %s: %w", prefix, err)
	}
	type latest struct {
		key     string
		version int64
	}
	days := map[string]latest{}
	for _, o := range objs {
		day, v, ok := dayManifest(o.Key, prefix)
		if ok && v > days[day].version {
			days[day] = latest{key: o.Key, version: v}
		}
	}
	old := c.cache[projectID]
	fresh := make(map[string]int64, len(days))
	minTS := int64(math.MaxInt64)
	for _, l := range days {
		ts, ok := old[l.key]
		if !ok {
			raw, _, err := c.Store.Get(ctx, l.key, nil)
			if errors.Is(err, objstore.ErrNotFound) {
				// Garbage-collected between LIST and GET: a newer version
				// exists; look again on the next round.
				return 0, fmt.Errorf("deletions: %s vanished", l.key)
			}
			if err != nil {
				return 0, fmt.Errorf("deletions: read %s: %w", l.key, err)
			}
			var m struct {
				TombstoneTS int64 `json:"tombstone_ts"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				return 0, fmt.Errorf("deletions: %s: %w", l.key, err)
			}
			ts = m.TombstoneTS
		}
		fresh[l.key] = ts
		minTS = min(minTS, ts)
	}
	if c.cache == nil {
		c.cache = map[uint32]map[string]int64{}
	}
	c.cache[projectID] = fresh
	return minTS, nil
}

// CheckOnce examines every pending request and returns how many it flipped
// to "done" and how many are still pending.
func (c *Checker) CheckOnce(ctx context.Context) (done, pending int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	reqs, err := db.PendingDeletions(ctx, c.DB.R())
	if err != nil {
		return 0, 0, fmt.Errorf("deletions: %w", err)
	}
	var errs []error
	minByProject := map[uint32]int64{}
	failed := map[uint32]bool{}
	seen := map[uint32]bool{}
	for _, d := range reqs {
		seen[d.ProjectID] = true
		if d.PublishedAt == 0 {
			pending++
			continue
		}
		complete := now().UnixMilli()-d.CreatedAt >= DeletionGrace.Milliseconds()
		if !complete && !failed[d.ProjectID] {
			minTS, ok := minByProject[d.ProjectID]
			if !ok {
				minTS, err = c.minTombstoneTS(ctx, d.ProjectID)
				if err != nil {
					errs = append(errs, err)
					failed[d.ProjectID] = true
				} else {
					minByProject[d.ProjectID] = minTS
				}
			}
			complete = !failed[d.ProjectID] && minTS >= d.TSMillis
		}
		if !complete {
			pending++
			continue
		}
		ts := now().UnixMilli()
		if err := c.DB.Write(ctx, func(tx *sql.Tx) error { return db.MarkDeletionDone(ctx, tx, d.ID, ts) }); err != nil {
			errs = append(errs, fmt.Errorf("deletions: mark %s done: %w", d.ID, err))
			pending++
			continue
		}
		done++
		if c.Logger != nil {
			c.Logger.Info("deletion request completed", "id", d.ID, "project", d.ProjectID)
		}
	}
	for pid := range c.cache {
		if !seen[pid] {
			delete(c.cache, pid)
		}
	}
	return done, pending, errors.Join(errs...)
}

// Run calls CheckOnce every interval until ctx is done. onResult (optional)
// receives the number of requests still pending after each round.
func (c *Checker) Run(ctx context.Context, interval time.Duration, onResult func(pending int)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		cctx, cancel := context.WithTimeout(ctx, max(interval, 30*time.Second))
		_, pending, err := c.CheckOnce(cctx)
		cancel()
		if err != nil && ctx.Err() == nil && c.Logger != nil {
			c.Logger.Warn("deletion check incomplete", "err", err.Error())
		}
		if onResult != nil {
			onResult(pending)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
