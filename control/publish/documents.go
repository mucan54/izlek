// Package publish turns the control database into the object-store snapshots
// the data plane reads (spec/control.md §9, spec/manifest.md §8/§9,
// spec/flags.md §1) and watches deletion requests until the engine has
// applied them.
package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/mucan54/izlek/control/db"
)

// Object keys (spec/manifest.md §1).
const (
	TokensKey   = "control/tokens.json"
	KeysKey     = "control/keys.json"
	ProjectsKey = "control/projects.json"
	flagsPrefix = "flags/"
	tombPrefix  = "tombstones/"
)

// FlagsKey is the object key of a token's ruleset.
func FlagsKey(token string) string { return flagsPrefix + token + ".json" }

// TombstoneKey is the object key of a deletion request's tombstone.
func TombstoneKey(projectID uint32, tsMillis int64, id string) string {
	return fmt.Sprintf("%sp=%d/%013d-%s.json", tombPrefix, projectID, tsMillis, id)
}

// flagsStateName is the publish_state row holding a project's ruleset version.
func flagsStateName(projectID uint32) string {
	return "flags/p=" + strconv.FormatUint(uint64(projectID), 10)
}

// TokenEntry is one token of control/tokens.json.
type TokenEntry struct {
	ProjectID      uint32   `json:"project_id"`
	Status         string   `json:"status"`
	RateLimit      int      `json:"rate_limit"`
	AllowedOrigins []string `json:"allowed_origins"`
	Replay         bool     `json:"replay"`
}

// TokensDoc is control/tokens.json.
type TokensDoc struct {
	Version     int64                 `json:"version"`
	GeneratedAt int64                 `json:"generated_at"`
	Tokens      map[string]TokenEntry `json:"tokens"`
}

// KeyEntry is one key of control/keys.json.
type KeyEntry struct {
	ProjectID uint32   `json:"project_id"`
	Scopes    []string `json:"scopes"`
}

// KeysDoc is control/keys.json. Map keys are hex SHA-256 digests of secrets.
type KeysDoc struct {
	Version int64               `json:"version"`
	Keys    map[string]KeyEntry `json:"keys"`
}

// ProjectEntry is one project of control/projects.json. Closed is an
// addition to the six fields of spec/manifest.md §8 (readers ignore unknown
// fields): it lets the data plane tell a closed project, whose retention is
// forced to 0, from a misconfigured one.
type ProjectEntry struct {
	Name                 string   `json:"name"`
	RetentionDays        int      `json:"retention_days"`
	ReplayRetentionDays  int      `json:"replay_retention_days"`
	DropKeys             []string `json:"drop_keys"`
	ScanLimitBytes       int64    `json:"scan_limit_bytes"`
	MaxConcurrentQueries int      `json:"max_concurrent_queries"`
	Closed               bool     `json:"closed,omitempty"`
}

// ProjectsDoc is control/projects.json. Map keys are decimal project ids.
type ProjectsDoc struct {
	Version  int64                   `json:"version"`
	Projects map[string]ProjectEntry `json:"projects"`
}

// FlagEntry is one flag of a published ruleset.
type FlagEntry struct {
	Key     string          `json:"key"`
	Enabled bool            `json:"enabled"`
	Salt    string          `json:"salt"`
	Rules   json.RawMessage `json:"rules"`
	Default json.RawMessage `json:"default"`
}

// RulesetDoc is flags/<token>.json.
type RulesetDoc struct {
	V     int64       `json:"v"`
	Flags []FlagEntry `json:"flags"`
}

// TombstoneDoc is tombstones/p=<pid>/<ts>-<id>.json.
type TombstoneDoc struct {
	ID         string   `json:"id"`
	TSMillis   int64    `json:"ts_ms"`
	PersonKeys []string `json:"person_keys"`
	AnonIDs    []string `json:"anon_ids"`
}

// Tombstone builds the tombstone of a deletion request.
func Tombstone(d db.Deletion) TombstoneDoc {
	t := TombstoneDoc{ID: d.ID, TSMillis: d.TSMillis, PersonKeys: make([]string, 0, len(d.UserIDs)), AnonIDs: d.AnonIDs}
	for _, u := range d.UserIDs {
		t.PersonKeys = append(t.PersonKeys, "u:"+u)
	}
	if t.AnonIDs == nil {
		t.AnonIDs = []string{}
	}
	return t
}

// tombstoneRepublishWindow: tombstone objects are kept for 400 days by the
// compactor (spec/manifest.md §9). A missing one is only restored while it
// is safely younger than that, so an expired tombstone is not resurrected.
const tombstoneRepublishWindow = 395 * 24 * time.Hour

// snapshot is everything one publish needs, read in a single transaction.
type snapshot struct {
	projects  []db.Project
	tokens    []db.Token
	keys      []db.Key
	flags     map[uint32][]db.Flag
	deletions []db.Deletion
}

func loadSnapshot(ctx context.Context, d *db.DB, now time.Time) (*snapshot, error) {
	var s snapshot
	err := d.Read(ctx, func(q db.Q) error {
		var err error
		if s.projects, err = db.AllProjects(ctx, q); err != nil {
			return err
		}
		if s.tokens, err = db.AllTokens(ctx, q); err != nil {
			return err
		}
		if s.keys, err = db.ActiveKeys(ctx, q); err != nil {
			return err
		}
		if s.flags, err = db.AllFlags(ctx, q); err != nil {
			return err
		}
		s.deletions, err = db.DeletionsSince(ctx, q, now.Add(-tombstoneRepublishWindow).UnixMilli())
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("publish: read database: %w", err)
	}
	return &s, nil
}

func (s *snapshot) openProjects() map[uint32]bool {
	open := make(map[uint32]bool, len(s.projects))
	for i := range s.projects {
		if !s.projects[i].Closed() {
			open[s.projects[i].ID] = true
		}
	}
	return open
}

func (s *snapshot) tokensDoc(open map[uint32]bool) TokensDoc {
	doc := TokensDoc{Tokens: make(map[string]TokenEntry, len(s.tokens))}
	for _, t := range s.tokens {
		status := t.Status
		if !open[t.ProjectID] {
			status = "disabled"
		}
		origins := t.AllowedOrigins
		if origins == nil {
			origins = []string{}
		}
		doc.Tokens[t.Token] = TokenEntry{ProjectID: t.ProjectID, Status: status, RateLimit: t.RateLimit, AllowedOrigins: origins, Replay: t.Replay}
	}
	return doc
}

func (s *snapshot) keysDoc() KeysDoc {
	doc := KeysDoc{Keys: make(map[string]KeyEntry, len(s.keys))}
	for _, k := range s.keys {
		scopes := k.Scopes
		if scopes == nil {
			scopes = []string{}
		}
		doc.Keys[k.Hash] = KeyEntry{ProjectID: k.ProjectID, Scopes: scopes}
	}
	return doc
}

func (s *snapshot) projectsDoc() ProjectsDoc {
	doc := ProjectsDoc{Projects: make(map[string]ProjectEntry, len(s.projects))}
	for _, p := range s.projects {
		drop := p.DropKeys
		if drop == nil {
			drop = []string{}
		}
		doc.Projects[strconv.FormatUint(uint64(p.ID), 10)] = ProjectEntry{
			Name: p.Name, RetentionDays: p.RetentionDays, ReplayRetentionDays: p.ReplayRetentionDays, DropKeys: drop,
			ScanLimitBytes: p.ScanLimitBytes, MaxConcurrentQueries: p.MaxConcurrentQueries, Closed: p.Closed(),
		}
	}
	return doc
}

// Ruleset builds the published form of a project's flags (v left at 0).
func Ruleset(flags []db.Flag) RulesetDoc {
	doc := RulesetDoc{Flags: make([]FlagEntry, 0, len(flags))}
	for _, f := range flags {
		rules, def := f.Rules, f.Default
		if len(rules) == 0 {
			rules = json.RawMessage("[]")
		}
		if len(def) == 0 {
			def = json.RawMessage("null")
		}
		doc.Flags = append(doc.Flags, FlagEntry{Key: f.Key, Enabled: f.Enabled, Salt: f.Salt, Rules: rules, Default: def})
	}
	return doc
}

// contentHash hashes the JSON encoding of v. Maps encode with sorted keys,
// so equal content always hashes equally.
func contentHash(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
