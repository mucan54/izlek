package db

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Roles, weakest to strongest (spec/control.md §1).
const (
	RoleViewer = "viewer"
	RoleMember = "member"
	RoleAdmin  = "admin"
	RoleOwner  = "owner"
)

// RoleRank orders roles; unknown roles rank below viewer.
func RoleRank(role string) int {
	switch role {
	case RoleViewer:
		return 1
	case RoleMember:
		return 2
	case RoleAdmin:
		return 3
	case RoleOwner:
		return 4
	}
	return 0
}

// ValidRole reports whether role is one of the four roles.
func ValidRole(role string) bool { return RoleRank(role) > 0 }

// Actor identifies who performed an action: a user, a secret key or the
// system itself (CLI bootstrap, background jobs).
type Actor struct {
	Kind string // "user" | "key" | "system"
	ID   int64
}

// Actor constructors.
func UserActor(id int64) Actor { return Actor{Kind: "user", ID: id} }
func KeyActor(id int64) Actor  { return Actor{Kind: "key", ID: id} }
func SystemActor() Actor       { return Actor{Kind: "system"} }

// String is the storage form: "user:5", "key:3", "system".
func (a Actor) String() string {
	if a.Kind == "user" || a.Kind == "key" {
		return a.Kind + ":" + strconv.FormatInt(a.ID, 10)
	}
	return "system"
}

// ParseActor is the inverse of String; anything unrecognised is the system.
func ParseActor(s string) Actor {
	kind, id, ok := strings.Cut(s, ":")
	if ok && (kind == "user" || kind == "key") {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			return Actor{Kind: kind, ID: n}
		}
	}
	return SystemActor()
}

// MarshalJSON renders the API form (spec/control.md §10, "actor (user id |
// key id)"): a user is its numeric id, a key is the string "key:<id>", the
// system is "system".
func (a Actor) MarshalJSON() ([]byte, error) {
	switch a.Kind {
	case "user":
		return strconv.AppendInt(nil, a.ID, 10), nil
	case "key":
		return json.Marshal(a.String())
	}
	return []byte(`"system"`), nil
}

// UnmarshalJSON accepts what MarshalJSON produces.
func (a *Actor) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*a = UserActor(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("db: invalid actor %s", data)
	}
	*a = ParseActor(s)
	return nil
}

// User is a row of users. Secrets never leave the server.
type User struct {
	ID           int64
	Email        string
	PasswordHash string
	TOTPSecret   string
	TOTPPending  string
	TOTPEnabled  bool
	TOTPLastStep int64
	CreatedAt    int64
}

// OrgRole is one organisation a user belongs to.
type OrgRole struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// Member is one membership of an organisation.
type Member struct {
	UserID    int64  `json:"user_id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
}

// Session is a login session joined with its user.
type Session struct {
	IDHash    string
	CSRF      string
	CreatedAt int64
	ExpiresAt int64
	User      User
}

// Project is the API shape of a project (spec/control.md §3).
type Project struct {
	ID                   uint32   `json:"id"`
	OrgID                int64    `json:"org_id"`
	Name                 string   `json:"name"`
	RetentionDays        int      `json:"retention_days"`
	ReplayRetentionDays  int      `json:"replay_retention_days"`
	DropKeys             []string `json:"drop_keys"`
	ScanLimitBytes       int64    `json:"scan_limit_bytes"`
	MaxConcurrentQueries int      `json:"max_concurrent_queries"`
	ReplaySampleRate     float64  `json:"replay_sample_rate"`
	CreatedAt            int64    `json:"created_at"`
	ClosedAt             int64    `json:"-"` // 0 = open
}

// Closed reports whether the project has been closed (DELETE).
func (p *Project) Closed() bool { return p.ClosedAt != 0 }

// Token is a write token (spec/control.md §4).
type Token struct {
	Token          string   `json:"token"`
	Name           string   `json:"name"`
	Status         string   `json:"status"`
	AllowedOrigins []string `json:"allowed_origins"`
	RateLimit      int      `json:"rate_limit"`
	Replay         bool     `json:"replay"`
	CreatedAt      int64    `json:"created_at"`
	ProjectID      uint32   `json:"-"`
}

// Key is a secret key; the secret itself is not stored.
type Key struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	Prefix     string   `json:"prefix"`
	CreatedAt  int64    `json:"created_at"`
	LastUsedAt *int64   `json:"last_used_at"`
	ProjectID  uint32   `json:"-"`
	Hash       string   `json:"-"`
}

// HasScope reports whether the key carries scope.
func (k *Key) HasScope(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Flag is the API shape of a feature flag (spec/control.md §5). Rules and
// Default hold validated, canonical JSON.
type Flag struct {
	Key         string          `json:"key"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	Salt        string          `json:"salt"`
	Rules       json.RawMessage `json:"rules"`
	Default     json.RawMessage `json:"default"`
	Version     int64           `json:"version"`
	UpdatedAt   int64           `json:"updated_at"`
	UpdatedBy   Actor           `json:"updated_by"`
}

// FlagVersion is one entry of a flag's history.
type FlagVersion struct {
	Version int64           `json:"version"`
	TS      int64           `json:"ts"`
	Actor   Actor           `json:"actor"`
	Flag    json.RawMessage `json:"flag"`
}

// Insight is a saved query (spec/control.md §6).
type Insight struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Query     json.RawMessage `json:"query"`
	CreatedBy Actor           `json:"created_by"`
	UpdatedAt int64           `json:"updated_at"`
}

// DashboardItem places an insight on a dashboard grid.
type DashboardItem struct {
	InsightID int64 `json:"insight_id"`
	X         int   `json:"x"`
	Y         int   `json:"y"`
	W         int   `json:"w"`
	H         int   `json:"h"`
}

// Dashboard is a grid of insights.
type Dashboard struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Items     []DashboardItem `json:"items"`
	UpdatedAt int64           `json:"updated_at"`
}

// Deletion is a person-deletion request (spec/control.md §7).
type Deletion struct {
	ID        string   `json:"id"`
	UserIDs   []string `json:"user_ids"`
	AnonIDs   []string `json:"anon_ids"`
	Status    string   `json:"status"`
	CreatedAt int64    `json:"created_at"`
	CreatedBy Actor    `json:"created_by"`
	Note      string   `json:"note"`

	ProjectID   uint32 `json:"-"`
	TSMillis    int64  `json:"-"` // tombstone ts_ms
	PublishedAt int64  `json:"-"` // 0 = tombstone not yet in the store
}

// AuditEntry is one row of the audit log.
type AuditEntry struct {
	ID     int64           `json:"id"`
	TS     int64           `json:"ts"`
	Actor  Actor           `json:"actor"`
	Action string          `json:"action"`
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail"`
}

// PublishState is the bookkeeping of one published object family.
type PublishState struct {
	Version     int64
	Hash        string
	GeneratedAt int64
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Only reachable with values that cannot be JSON (channels, cycles);
		// every caller passes plain data.
		panic(fmt.Sprintf("db: cannot encode %T: %v", v, err))
	}
	return string(b)
}

func stringsFromJSON(s string) []string {
	out := []string{}
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		out = []string{}
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
