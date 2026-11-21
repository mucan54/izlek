package api_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mucan54/izlek/control/api"
	"github.com/mucan54/izlek/control/auth"
)

type patBody struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	CreatedAt  int64    `json:"created_at"`
	LastUsedAt *int64   `json:"last_used_at"`
	ExpiresAt  *int64   `json:"expires_at"`
	Token      string   `json:"token"`
}

// pat creates a personal access token for c and returns it.
func (e *env) pat(c *client, name string, scopes ...string) patBody {
	e.t.Helper()
	var out patBody
	c.post("/api/me/tokens", map[string]any{"name": name, "scopes": scopes, "expires_in_days": nil}).want(201).json(&out)
	if !auth.LooksLikePersonalToken(out.Token) {
		e.t.Fatalf("token %q", out.Token)
	}
	return out
}

func (e *env) bearer(token string) *client {
	c := e.anon()
	c.bearer = token
	return c
}

func TestPersonalTokenLifecycle(t *testing.T) {
	e := newOAuthEnv(t)
	now := e.clock.Now().UnixMilli()

	if list := e.owner.get("/api/me/tokens").want(200).list(); len(list) != 0 {
		t.Fatalf("initial list: %v", list)
	}
	var first patBody
	r := e.owner.post("/api/me/tokens", map[string]any{"name": "  laptop  ", "scopes": []string{"izlek:read"}, "expires_in_days": 30}).want(201).json(&first)
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control %q", r.Header.Get("Cache-Control"))
	}
	if !strings.HasPrefix(first.Token, "pat_") || len(first.Token) != 36 || first.Name != "laptop" || first.Prefix != "pat_…"+first.Token[32:] ||
		fmt.Sprint(first.Scopes) != "[izlek:read]" || first.CreatedAt != now || first.LastUsedAt != nil ||
		first.ExpiresAt == nil || *first.ExpiresAt != now+30*24*3600*1000 || first.ID == "" {
		t.Fatalf("created: %+v", first)
	}
	// admin implies read; no expiry when null or absent.
	second := e.pat(e.owner, "ci", "izlek:admin")
	if fmt.Sprint(second.Scopes) != "[izlek:read izlek:admin]" || second.ExpiresAt != nil {
		t.Fatalf("second: %+v", second)
	}
	var third patBody
	e.owner.post("/api/me/tokens", map[string]any{"name": "x", "scopes": []string{"izlek:read", "izlek:admin"}}).want(201).json(&third)
	if third.ExpiresAt != nil {
		t.Fatalf("third: %+v", third)
	}

	// Listed newest first, without the secret.
	raw := e.owner.get("/api/me/tokens").want(200)
	list := raw.list()
	if len(list) != 3 || list[0]["id"] != third.ID || list[2]["id"] != first.ID || list[2]["prefix"] != first.Prefix || list[2]["last_used_at"] != nil || list[1]["expires_at"] != nil {
		t.Fatalf("list: %v", list)
	}
	for _, secret := range []string{first.Token, second.Token, third.Token} {
		if strings.Contains(string(raw.Body), secret) || strings.Contains(string(raw.Body), secret[4:]) {
			t.Fatal("the listing contains a secret")
		}
		// Stored as a hash only, absent from the audit log.
		if n := e.scalar(`SELECT COUNT(*) FROM personal_tokens WHERE instr(hash || name || prefix, ?) > 0`, secret[4:]); n != 0 {
			t.Fatal("the secret is stored")
		}
		if n := e.scalar(`SELECT COUNT(*) FROM audit_log WHERE instr(detail || target, ?) > 0`, secret[4:]); n != 0 {
			t.Fatal("the secret is in the audit log")
		}
		if n := e.scalar(`SELECT COUNT(*) FROM personal_tokens WHERE hash = ?`, auth.HashSecret(secret)); n != 1 {
			t.Fatal("the hash is not stored")
		}
	}
	if list[0]["token"] != nil || strings.Contains(string(raw.Body), "hash") {
		t.Fatalf("listing: %s", raw.Body)
	}

	// Validation.
	for name, body := range map[string]map[string]any{
		"no name":           {"scopes": []string{"izlek:read"}},
		"long name":         {"name": strings.Repeat("x", 81), "scopes": []string{"izlek:read"}},
		"no scopes":         {"name": "x"},
		"empty scopes":      {"name": "x", "scopes": []string{}},
		"unknown scope":     {"name": "x", "scopes": []string{"izlek:read", "root"}},
		"key scope":         {"name": "x", "scopes": []string{"admin"}},
		"bad expiry":        {"name": "x", "scopes": []string{"izlek:read"}, "expires_in_days": 7},
		"zero expiry":       {"name": "x", "scopes": []string{"izlek:read"}, "expires_in_days": 0},
		"expiry as string":  {"name": "x", "scopes": []string{"izlek:read"}, "expires_in_days": "30"},
		"scopes as string":  {"name": "x", "scopes": "izlek:read"},
		"fractional expiry": {"name": "x", "scopes": []string{"izlek:read"}, "expires_in_days": 30.5},
	} {
		if r := e.owner.post("/api/me/tokens", body); r.Status != 400 {
			t.Errorf("%s: status %d %s", name, r.Status, r.Body)
		}
	}
	e.owner.post("/api/me/tokens", map[string]any{"name": strings.Repeat("ş", 80), "scopes": []string{"izlek:read"}}).want(201)

	// Revoke: own token 204, then gone; another user's id is a 404.
	other := e.member(e.owner, e.org, "admin")
	theirs := e.pat(other, "theirs", "izlek:read")
	e.owner.del("/api/me/tokens/"+theirs.ID).wantError(404, "not_found")
	e.owner.del("/api/me/tokens/abc").wantError(404, "not_found")
	e.owner.del("/api/me/tokens/" + second.ID).want(204)
	e.owner.del("/api/me/tokens/"+second.ID).wantError(404, "not_found")
	e.bearer(second.Token).get("/api/projects").wantError(401, "unauthorized")
	e.mcp(second.Token, "tools/list", nil).want(401)
	e.bearer(theirs.Token).get("/api/projects").want(200)
	if list := e.owner.get("/api/me/tokens").want(200).list(); len(list) != 3 {
		t.Fatalf("after revoke: %v", list)
	}
	if list := other.get("/api/me/tokens").want(200).list(); len(list) != 1 || list[0]["id"] != theirs.ID {
		t.Fatalf("other user's list: %v", list)
	}

	// Audit: create and revoke, attributed to the user.
	audit := e.auditActions("pat.")
	user := fmt.Sprintf("user:%d", e.owner.userID)
	if len(audit) != 6 || !strings.HasPrefix(audit[0], "pat.create "+user+" "+first.ID+" ") || !strings.Contains(audit[0], `"name":"laptop"`) ||
		!strings.Contains(audit[0], `"scopes":["izlek:read"]`) || !strings.Contains(audit[0], `"expires_at"`) ||
		!strings.HasPrefix(audit[5], "pat.revoke "+user+" "+second.ID+" ") || !strings.Contains(audit[5], `"name":"ci"`) {
		t.Fatalf("audit: %q", audit)
	}
}

func TestPersonalTokensNeedASession(t *testing.T) {
	e := newOAuthEnv(t)
	p := e.env.project(e.owner, e.org, "Shop")
	admin := e.pat(e.owner, "admin", "izlek:read", "izlek:admin")
	body := map[string]any{"name": "more", "scopes": []string{"izlek:read"}}

	// Anonymous.
	e.anon().get("/api/me/tokens").wantError(401, "unauthorized")
	e.anon().post("/api/me/tokens", body).wantError(401, "unauthorized")
	// The cookie alone is not enough for changes.
	noCSRF := *e.owner
	noCSRF.noCSRF = true
	noCSRF.post("/api/me/tokens", body).wantError(403, "csrf")
	noCSRF.del("/api/me/tokens/"+admin.ID).wantError(403, "csrf")
	crossSite := *e.owner
	crossSite.headers = map[string]string{"Sec-Fetch-Site": "cross-site"}
	crossSite.post("/api/me/tokens", body).wantError(403, "csrf")

	// No token of any kind manages tokens: not a PAT (even with every
	// scope), not a secret key, not an OAuth access token.
	kc, _ := e.secretKey(e.owner, p.ID, "query", "admin")
	_, oauthTok := e.grant(e.owner, "izlek:read izlek:admin")
	for name, bearer := range map[string]string{"pat": admin.Token, "key": kc.bearer, "oauth": oauthTok.AccessToken} {
		c := e.bearer(bearer)
		// Even together with a valid session cookie the bearer decides.
		c.cookie, c.csrf = e.owner.cookie, e.owner.csrf
		for _, r := range []*response{c.get("/api/me/tokens"), c.post("/api/me/tokens", body), c.del("/api/me/tokens/" + admin.ID)} {
			if r.Status != 403 && r.Status != 401 {
				t.Errorf("%s: %s → %d", name, r.req, r.Status)
			}
		}
	}
	if n := e.scalar(`SELECT COUNT(*) FROM personal_tokens`); n != 1 {
		t.Fatalf("%d tokens", n)
	}
	// Nor through MCP: there is no such tool.
	for _, n := range e.toolNames(admin.Token) {
		if strings.Contains(n, "token") && !strings.Contains(n, "write_token") {
			t.Errorf("tool %s", n)
		}
	}
}

func TestPersonalTokenLimit(t *testing.T) {
	e := newOAuthEnv(t)
	var first patBody
	for i := 0; i < 25; i++ {
		tok := e.pat(e.owner, fmt.Sprintf("t%d", i), "izlek:read")
		if i == 0 {
			first = tok
		}
	}
	e.owner.post("/api/me/tokens", map[string]any{"name": "one too many", "scopes": []string{"izlek:read"}}).wantError(409, "too_many_tokens")
	e.owner.del("/api/me/tokens/" + first.ID).want(204)
	e.pat(e.owner, "fits again", "izlek:read")
	// The limit is per user.
	e.pat(e.member(e.owner, e.org, "viewer"), "mine", "izlek:read")
}

func TestPersonalTokenOnTheAPI(t *testing.T) {
	e := newOAuthEnv(t)
	p := e.env.project(e.owner, e.org, "Shop")
	base := fmt.Sprintf("/api/projects/%d", p.ID)
	admin := e.bearer(e.pat(e.owner, "admin", "izlek:admin").Token)
	reader := e.bearer(e.pat(e.owner, "reader", "izlek:read").Token)

	// Reading, with either scope; no cookie, no CSRF header.
	for _, c := range []*client{admin, reader} {
		if list := c.get("/api/projects").want(200).list(); len(list) != 1 {
			t.Fatalf("projects: %v", list)
		}
		var me meBody
		c.get("/api/me").want(200).json(&me)
		if me.User.Email != "owner@example.com" || len(me.Orgs) != 1 || me.CSRF != "" {
			t.Fatalf("me: %+v", me)
		}
		c.get(base).want(200)
		c.get(base + "/tokens").want(200)
		c.get(base + "/keys").want(200)
		c.get(base + "/flags").want(200)
		c.get(base + "/insights").want(200)
		// POSTs that only read.
		c.post(base+"/q/schema", map[string]any{}).want(200)
	}
	if e.upstream.last().Header.Get("Authorization") != "" || e.upstream.last().Header.Get("X-LA-Project") != fmt.Sprint(p.ID) {
		t.Fatalf("upstream headers: %v", e.upstream.last().Header)
	}

	// Changing things needs izlek:admin.
	reader.post("/api/projects", map[string]any{"org_id": e.org, "name": "x"}).wantError(403, "insufficient_scope")
	reader.patch(base, map[string]any{"name": "x"}).wantError(403, "insufficient_scope")
	reader.post(base+"/flags", map[string]any{"key": "f"}).wantError(403, "insufficient_scope")
	reader.post(base+"/tokens", map[string]any{"name": "t"}).wantError(403, "insufficient_scope")
	reader.post(base+"/keys", map[string]any{"name": "k", "scopes": []string{"admin"}}).wantError(403, "insufficient_scope")
	reader.del(base+"/flags/f").wantError(403, "insufficient_scope")
	if n := e.scalar(`SELECT COUNT(*) FROM flags`) + e.scalar(`SELECT COUNT(*) FROM projects`) + e.scalar(`SELECT COUNT(*) FROM keys`); n != 1 {
		t.Fatalf("a read-only token changed something (%d)", n)
	}

	admin.post(base+"/flags", map[string]any{"key": "f", "enabled": true}).want(200)
	reader.post(base+"/flags/f/evaluate", map[string]any{"anon_id": "a"}).want(200)
	admin.put(base+"/flags/f", map[string]any{"enabled": false}).want(200)
	admin.patch(base, map[string]any{"retention_days": 90}).want(200)
	var second projectBody
	admin.post("/api/projects", map[string]any{"org_id": e.org, "name": "Second"}).want(200).json(&second)
	admin.get(fmt.Sprintf("/api/projects/%d/tokens", second.ID)).want(200)
	// Validation is the API's.
	admin.post(base+"/flags", map[string]any{"key": "Bad Key"}).wantError(400, "invalid_flag")

	// Out of reach whatever the scope: the account, its credentials, the
	// organisation's members and log, and what destroys data for good.
	denied := []*response{
		admin.post("/api/auth/logout", nil),
		admin.post("/api/me/password", map[string]any{"current": testPassword, "new": "another passphrase"}),
		admin.post("/api/me/totp/begin", nil),
		admin.post("/api/me/totp/enable", map[string]any{"code": "000000"}),
		admin.post("/api/me/totp/disable", map[string]any{"password": testPassword}),
		admin.get("/api/me/tokens"),
		admin.get("/api/me/connected-apps"),
		admin.del("/api/me/connected-apps/1"),
		admin.get(fmt.Sprintf("/api/orgs/%d/members", e.org)),
		admin.post(fmt.Sprintf("/api/orgs/%d/members", e.org), map[string]any{"email": "x@example.com", "role": "owner", "password": testPassword}),
		admin.patch(fmt.Sprintf("/api/orgs/%d/members/%d", e.org, e.owner.userID), map[string]any{"role": "viewer"}),
		admin.del(fmt.Sprintf("/api/orgs/%d/members/%d", e.org, e.owner.userID)),
		admin.get(fmt.Sprintf("/api/orgs/%d/audit", e.org)),
		admin.del(fmt.Sprintf("/api/orgs/%d", e.org)),
		admin.del(base),
		admin.post(base+"/deletions", map[string]any{"user_ids": []string{"u1"}}),
		admin.post("/api/setup", map[string]any{"email": "x@example.com", "password": testPassword, "org_name": "X"}),
		admin.post("/api/auth/login", map[string]any{"email": "owner@example.com", "password": testPassword}),
	}
	for _, r := range denied {
		r.wantError(403, "forbidden")
	}
	e.owner.get(base).want(200) // not closed
	if n := e.scalar(`SELECT COUNT(*) FROM memberships`) + e.scalar(`SELECT COUNT(*) FROM deletion_requests`); n != 1 {
		t.Fatalf("a denied request had an effect (%d)", n)
	}
	e.owner.me() // the session and password are untouched

	// Changes are audited as the user's, with the token noted.
	entries := e.owner.get(fmt.Sprintf("/api/orgs/%d/audit?limit=10", e.org)).want(200).list()
	var seen bool
	for _, entry := range entries {
		if entry["action"] == "flag.create" {
			via, _ := entry["detail"].(map[string]any)["via"].(map[string]any)
			seen = fmt.Sprint(entry["actor"]) == fmt.Sprint(e.owner.userID) && via["personal_token"] == "admin" && via["personal_token_id"] != nil && via["mcp_tool"] == nil
		}
	}
	if !seen {
		t.Fatalf("flag.create is not attributed to the token: %v", entries)
	}

	// Not a credential anywhere it should not be.
	for _, bad := range []string{"pat_" + strings.Repeat("a", 32), "pat_short", strings.Replace(admin.bearer, "pat_", "sk_", 1)} {
		e.bearer(bad).get("/api/projects").wantError(401, "unauthorized")
	}
	e.raw("GET", "/api/projects?access_token="+admin.bearer, nil, "").want(401)
}

func TestPersonalTokenFollowsRolesAcrossOrganisations(t *testing.T) {
	m := newMCPEnv(t)
	// Organisation A: m.owner owns it (project m.project).
	// Organisation B: owned by someone else; m.owner is a viewer there.
	b := bootstrapOrg(t, m.env, "boss@example.com", "Beta", "Beta app")
	boss := m.login("boss@example.com", testPassword)
	boss.post(fmt.Sprintf("/api/orgs/%d/members", b.OrgID), map[string]any{"email": "owner@example.com", "role": "viewer", "password": testPassword}).want(200)
	// Organisation C: no membership.
	c := bootstrapOrg(t, m.env, "stranger@example.com", "Gamma", "Gamma app")

	tok := m.pat(m.owner, "all accounts", "izlek:read", "izlek:admin")
	api := m.bearer(tok.Token)
	pidA, pidB, pidC := m.project.ID, b.ProjectID, c.ProjectID

	// One token sees both organisations, with the role in each.
	res := m.callTool(tok.Token, "list_projects", nil).ok(t)
	if len(res["projects"].([]any)) != 2 || len(res["organisations"].([]any)) != 2 {
		t.Fatalf("list_projects: %v", res)
	}
	if list := api.get("/api/projects").want(200).list(); len(list) != 2 {
		t.Fatalf("GET /api/projects: %v", list)
	}
	names := m.toolNames(tok.Token)
	if !has(names, "create_project") || len(names) != 22 {
		t.Fatalf("tools: %v", names)
	}
	// With several projects the id has to be given.
	m.callTool(tok.Token, "get_project", nil).fails(t, "project_id is required")

	// A (owner): read and write, through MCP and through the API.
	m.callTool(tok.Token, "create_flag", map[string]any{"project_id": pidA, "key": "in_a"}).ok(t)
	api.post(fmt.Sprintf("/api/projects/%d/flags", pidA), map[string]any{"key": "in_a_api"}).want(200)
	// B (viewer): read only.
	m.callTool(tok.Token, "get_project", map[string]any{"project_id": pidB}).ok(t)
	m.callTool(tok.Token, "list_flags", map[string]any{"project_id": pidB}).ok(t)
	m.callTool(tok.Token, "create_flag", map[string]any{"project_id": pidB, "key": "in_b"}).fails(t, "forbidden")
	m.callTool(tok.Token, "update_project_settings", map[string]any{"project_id": pidB, "name": "mine"}).fails(t, "forbidden")
	api.get(fmt.Sprintf("/api/projects/%d/flags", pidB)).want(200)
	api.post(fmt.Sprintf("/api/projects/%d/flags", pidB), map[string]any{"key": "in_b_api"}).wantError(403, "forbidden")
	api.post("/api/projects", map[string]any{"org_id": b.OrgID, "name": "x"}).wantError(403, "forbidden")
	// C (no membership): does not exist.
	m.callTool(tok.Token, "get_project", map[string]any{"project_id": pidC}).fails(t, "not_found")
	m.callTool(tok.Token, "create_flag", map[string]any{"project_id": pidC, "key": "in_c"}).fails(t, "not_found")
	api.get(fmt.Sprintf("/api/projects/%d", pidC)).wantError(404, "not_found")
	api.post(fmt.Sprintf("/api/projects/%d/flags", pidC), map[string]any{"key": "in_c_api"}).wantError(404, "not_found")
	api.post("/api/projects", map[string]any{"org_id": c.OrgID, "name": "x"}).wantError(404, "not_found")
	if n := m.scalar(`SELECT COUNT(*) FROM flags WHERE project_id <> ?`, pidA); n != 0 {
		t.Fatalf("%d flags outside organisation A", n)
	}
	if n := m.scalar(`SELECT COUNT(*) FROM flags WHERE project_id = ?`, pidA); n != 2 {
		t.Fatalf("%d flags in organisation A", n)
	}

	// Roles are read on every call: promoted in B, the same token can write there.
	boss.patch(fmt.Sprintf("/api/orgs/%d/members/%d", b.OrgID, m.owner.userID), map[string]any{"role": "admin"}).want(200)
	m.callTool(tok.Token, "create_flag", map[string]any{"project_id": pidB, "key": "in_b"}).ok(t)
	// Removed from B, it reaches nothing there.
	boss.del(fmt.Sprintf("/api/orgs/%d/members/%d", b.OrgID, m.owner.userID)).want(204)
	m.callTool(tok.Token, "get_project", map[string]any{"project_id": pidB}).fails(t, "not_found")
	api.get(fmt.Sprintf("/api/projects/%d", pidB)).wantError(404, "not_found")

	// The MCP change is audited as the user's, naming the token.
	last := m.owner.get(fmt.Sprintf("/api/orgs/%d/audit?limit=5", m.org)).want(200).list()
	var viaMCP bool
	for _, entry := range last {
		if entry["action"] == "flag.create" && entry["target"] == "in_a" {
			via, _ := entry["detail"].(map[string]any)["via"].(map[string]any)
			viaMCP = via["mcp_tool"] == "create_flag" && via["personal_token"] == "all accounts" && via["personal_token_id"] == tok.ID && via["oauth_client"] == nil &&
				fmt.Sprint(entry["actor"]) == fmt.Sprint(m.owner.userID)
		}
	}
	if !viaMCP {
		t.Fatalf("audit: %v", last)
	}
}

func TestPersonalTokenScopesOnMCP(t *testing.T) {
	m := newMCPEnv(t)
	reader := m.pat(m.owner, "reader", "izlek:read").Token
	names := m.toolNames(reader)
	if len(names) != 15 || has(names, "create_project") || !has(names, "run_sql") {
		t.Fatalf("tools for a read-only token: %v", names)
	}
	m.callTool(reader, "list_flags", nil).ok(t)
	r := m.mcp(reader, "tools/call", map[string]any{"name": "create_flag", "arguments": map[string]any{"key": "x"}}).want(403)
	if !strings.Contains(r.Header.Get("WWW-Authenticate"), `error="insufficient_scope"`) {
		t.Fatalf("challenge: %s", r.Header.Get("WWW-Authenticate"))
	}
	if n := m.scalar(`SELECT COUNT(*) FROM flags`); n != 0 {
		t.Fatal("a read-only token created a flag")
	}
	// A viewer's token with the admin scope still cannot change anything.
	viewer := m.pat(m.member(m.owner, m.org, "viewer"), "v", "izlek:admin").Token
	m.callTool(viewer, "create_flag", map[string]any{"key": "x"}).fails(t, "forbidden")
	m.callTool(viewer, "get_project", nil).ok(t)

	// It works without the OAuth server (no LA_PUBLIC_URL)…
	bare := newEnv(t)
	owner, org := bare.setup()
	bare.project(owner, org, "Shop")
	tok := bare.pat(owner, "t", "izlek:admin").Token
	bare.callTool(tok, "list_projects", nil).ok(t)
	// …and has its own rate-limit budget.
	for i := 0; i < 300; i++ {
		bare.mcp(tok, "tools/call", map[string]any{"name": "list_projects"}).want(200)
	}
	bare.callTool(tok, "list_projects", nil).fails(t, "Rate limit")
	bare.callTool(bare.pat(owner, "t2", "izlek:read").Token, "list_projects", nil).ok(t)
	// The instructions tell a model how to work with an account-wide token.
	init := string(bare.mcp(tok, "initialize", map[string]any{"protocolVersion": "2025-06-18"}).want(200).Body)
	if !strings.Contains(init, "personal access token") || !strings.Contains(init, "pass project_id explicitly") {
		t.Fatalf("instructions: %s", init)
	}
}

func TestPersonalTokenExpiryAndUse(t *testing.T) {
	m := newMCPEnv(t)
	var tok patBody
	m.owner.post("/api/me/tokens", map[string]any{"name": "short", "scopes": []string{"izlek:read"}, "expires_in_days": 30}).want(201).json(&tok)
	forever := m.pat(m.owner, "forever", "izlek:read")
	api := m.bearer(tok.Token)

	// last_used_at: set on first use, then at most once a minute.
	used := func() int64 {
		for _, item := range m.owner.get("/api/me/tokens").want(200).list() {
			if item["id"] == tok.ID {
				if item["last_used_at"] == nil {
					return 0
				}
				return int64(item["last_used_at"].(float64))
			}
		}
		t.Fatal("token not listed")
		return 0
	}
	if used() != 0 {
		t.Fatal("used before use")
	}
	t0 := m.clock.Now().UnixMilli()
	api.get("/api/projects").want(200)
	m.clock.Advance(30 * time.Second)
	m.mcp(tok.Token, "tools/list", nil).want(200)
	if got := used(); got != t0 {
		t.Fatalf("last_used_at = %d, want %d (one write a minute)", got, t0)
	}
	m.clock.Advance(31 * time.Second)
	m.mcp(tok.Token, "tools/list", nil).want(200)
	if got := used(); got != t0+61000 {
		t.Fatalf("last_used_at = %d, want %d", got, t0+61000)
	}

	// Expiry: 401 on both surfaces, with the usual challenge on /mcp.
	m.clock.Advance(30*24*time.Hour - 2*time.Minute)
	api.get("/api/projects").want(200)
	m.clock.Advance(2 * time.Minute)
	api.get("/api/projects").wantError(401, "unauthorized")
	r := m.mcp(tok.Token, "tools/list", nil).want(401)
	if got := r.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, `Bearer error="invalid_token"`) || !strings.Contains(got, "resource_metadata=") {
		t.Fatalf("challenge: %s", got)
	}
	m.mcp(forever.Token, "tools/list", nil).want(200)
	// The session expired with the clock; sign in again to look.
	owner := m.login("owner@example.com", testPassword)
	if list := owner.get("/api/me/tokens").want(200).list(); len(list) != 1 || list[0]["id"] != forever.ID {
		t.Fatalf("an expired token is listed: %v", list)
	}
	// The clean-up loop removes it.
	if err := m.db.Write(context.Background(), func(tx *sql.Tx) error { return api2Prune(tx, m.clock.Now()) }); err != nil {
		t.Fatal(err)
	}
	if n := m.scalar(`SELECT COUNT(*) FROM personal_tokens`); n != 1 {
		t.Fatalf("%d rows after pruning", n)
	}
	m.mcp(forever.Token, "tools/list", nil).want(200)
}

func api2Prune(tx *sql.Tx, now time.Time) error { return api.PruneOAuth(context.Background(), tx, now) }

func TestCredentialChangesRevokePersonalTokens(t *testing.T) {
	e := newOAuthEnv(t)
	other := e.member(e.owner, e.org, "admin")
	theirs := e.pat(other, "theirs", "izlek:read")
	a, b := e.pat(e.owner, "a", "izlek:read"), e.pat(e.owner, "b", "izlek:admin")

	e.owner.post("/api/me/password", map[string]any{"current": testPassword, "new": "a different passphrase"}).want(200)
	for _, tok := range []patBody{a, b} {
		e.bearer(tok.Token).get("/api/projects").wantError(401, "unauthorized")
		e.mcp(tok.Token, "tools/list", nil).want(401)
	}
	if list := e.owner.get("/api/me/tokens").want(200).list(); len(list) != 0 {
		t.Fatalf("tokens survive a password change: %v", list)
	}
	e.bearer(theirs.Token).get("/api/projects").want(200) // another user's tokens are untouched
	if audit := e.auditActions("user.password"); len(audit) != 1 || !strings.Contains(audit[0], `"personal_tokens_revoked":2`) {
		t.Fatalf("audit: %q", audit)
	}

	// Switching on the second factor does the same.
	c := e.pat(e.owner, "c", "izlek:read")
	var begin struct {
		Secret string `json:"secret"`
	}
	e.owner.post("/api/me/totp/begin", nil).want(200).json(&begin)
	code, err := auth.TOTPCode(begin.Secret, auth.TOTPStep(e.clock.Now()))
	if err != nil {
		t.Fatal(err)
	}
	e.owner.post("/api/me/totp/enable", map[string]any{"code": code}).want(200)
	e.bearer(c.Token).get("/api/projects").wantError(401, "unauthorized")
	if audit := e.auditActions("user.totp_enable"); len(audit) != 1 || !strings.Contains(audit[0], `"personal_tokens_revoked":1`) {
		t.Fatalf("audit: %q", audit)
	}
}
