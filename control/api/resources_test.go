package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/publish"
	"github.com/mucan54/izlek/golib/flags"
	"github.com/mucan54/izlek/ingest/tokens"
)

func TestMembers(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	members := fmt.Sprintf("/api/orgs/%d/members", org)

	list := owner.get(members).want(200).list()
	if len(list) != 1 || list[0]["email"] != "owner@example.com" || list[0]["role"] != "owner" ||
		list[0]["user_id"] != float64(owner.userID) || list[0]["created_at"] != float64(e.clock.Now().UnixMilli()) {
		t.Fatalf("members: %v", list)
	}
	if len(list[0]) != 4 {
		t.Fatalf("member has unexpected fields (password hash?): %v", list[0])
	}

	add := func(email, role, password string) *response {
		return owner.post(members, map[string]any{"email": email, "role": role, "password": password})
	}
	add("bad", "member", testPassword).wantError(400, "invalid_email")
	add("new@example.com", "superuser", testPassword).wantError(400, "bad_request")
	add("new@example.com", "member", "short").wantError(400, "weak_password")
	add("new@example.com", "member", "").wantError(400, "weak_password")
	m := add(" New@Example.com", "member", testPassword).want(200).object()
	if m["email"] != "new@example.com" || m["role"] != "member" || m["user_id"] == nil {
		t.Fatalf("added member: %v", m)
	}
	uid := int64(m["user_id"].(float64))
	add("new@example.com", "viewer", testPassword).wantError(409, "conflict")
	// The password given by the owner works.
	nu := e.login("new@example.com", testPassword)
	if got := nu.me(); len(got.Orgs) != 1 || got.Orgs[0].Role != "member" {
		t.Fatalf("new member's orgs: %+v", got.Orgs)
	}

	// An existing user joins a second organisation without a password and
	// keeps the one they have.
	second := bootstrapOrg(t, e, "other-owner@example.com", "Other Inc", "p")
	other := e.login("other-owner@example.com", testPassword)
	other.post(fmt.Sprintf("/api/orgs/%d/members", second.OrgID), map[string]any{"email": "new@example.com", "role": "admin"}).want(200)
	other.post(fmt.Sprintf("/api/orgs/%d/members", second.OrgID), map[string]any{"email": "new@example.com", "role": "admin", "password": "x"}).wantError(409, "conflict")
	if got := nu.me(); len(got.Orgs) != 2 || got.Orgs[1].Role != "admin" {
		t.Fatalf("after joining a second org: %+v", got.Orgs)
	}
	e.login("new@example.com", testPassword)

	member := fmt.Sprintf("%s/%d", members, uid)
	owner.patch(member, map[string]any{"role": "nope"}).wantError(400, "bad_request")
	if got := owner.patch(member, map[string]any{"role": "admin"}).want(200).object(); got["role"] != "admin" || got["email"] != "new@example.com" {
		t.Fatalf("patched member: %v", got)
	}
	owner.patch(members+"/99999", map[string]any{"role": "admin"}).wantError(404, "not_found")
	owner.patch(members+"/abc", map[string]any{"role": "admin"}).wantError(404, "not_found")
	owner.del(members+"/99999").wantError(404, "not_found")

	// Last-owner protection.
	self := fmt.Sprintf("%s/%d", members, owner.userID)
	owner.patch(self, map[string]any{"role": "admin"}).wantError(409, "last_owner")
	owner.del(self).wantError(409, "last_owner")
	owner.patch(self, map[string]any{"role": "owner"}).want(200) // no change is fine
	// With a second owner, the first may step down — and then is no longer allowed to manage.
	owner.patch(member, map[string]any{"role": "owner"}).want(200)
	owner.patch(self, map[string]any{"role": "viewer"}).want(200)
	owner.patch(member, map[string]any{"role": "member"}).wantError(403, "forbidden")
	nu.patch(member, map[string]any{"role": "admin"}).wantError(409, "last_owner")
	nu.del(member).wantError(409, "last_owner")
	// The new owner removes the old one, whose session keeps working but sees nothing.
	nu.del(self).want(204)
	if got := owner.me(); len(got.Orgs) != 0 {
		t.Fatalf("removed member still has orgs: %+v", got.Orgs)
	}
	owner.get(members).wantError(404, "not_found")
}

func TestAuditLog(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	base := fmt.Sprintf("/api/projects/%d", p.ID)
	key, keyID := e.secretKey(owner, p.ID, "admin")
	var tok struct {
		Token string `json:"token"`
	}
	owner.post(base+"/tokens", map[string]any{"name": "web"}).want(200).json(&tok)
	owner.patch(base+"/tokens/"+tok.Token, map[string]any{"status": "disabled"}).want(200)
	owner.del(base + "/tokens/" + tok.Token).want(204)
	owner.patch(base, map[string]any{"retention_days": 30, "drop_keys": []string{"email"}}).want(200)
	owner.post(base+"/flags", map[string]any{"key": "f"}).want(200)
	owner.put(base+"/flags/f", map[string]any{"enabled": true}).want(200)
	owner.del(base + "/flags/f").want(204)
	var ins struct {
		ID int64 `json:"id"`
	}
	owner.post(base+"/insights", map[string]any{"name": "i", "kind": "sql", "query": map[string]any{"sql": "SELECT 1"}}).want(200).json(&ins)
	owner.post(base+"/dashboards", map[string]any{"name": "d", "items": []any{map[string]any{"insight_id": ins.ID, "x": 0, "y": 0, "w": 6, "h": 3}}}).want(200)
	owner.post(base+"/deletions", map[string]any{"user_ids": []string{"secret-user-id-42"}, "anon_ids": []string{"secret-anon-id"}, "note": "ticket"}).want(200)
	key.post(base+"/tokens", map[string]any{"name": "from key"}).want(200)
	owner.del(fmt.Sprintf("%s/keys/%d", base, keyID)).want(204)
	owner.post("/api/me/password", map[string]any{"current": testPassword, "new": "a brand new password"}).want(200)
	member := e.member(owner, org, "member")
	_ = member

	audit := fmt.Sprintf("/api/orgs/%d/audit", org)
	all := owner.get(audit + "?limit=500").want(200).list()
	var actions []string
	for i, a := range all {
		for _, field := range []string{"id", "ts", "actor", "action", "target", "detail"} {
			if _, ok := a[field]; !ok {
				t.Fatalf("audit entry lacks %q: %v", field, a)
			}
		}
		if len(a) != 6 {
			t.Fatalf("audit entry has extra fields: %v", a)
		}
		if i > 0 && a["id"].(float64) >= all[i-1]["id"].(float64) {
			t.Fatal("audit log is not ordered newest → oldest")
		}
		if _, ok := a["detail"].(map[string]any); !ok {
			t.Fatalf("detail is not a JSON object: %v", a["detail"])
		}
		actions = append(actions, a["action"].(string))
	}
	want := []string{"auth.login", "member.add", "user.password", "key.delete", "token.create", "deletion.create", "dashboard.create", "insight.create",
		"flag.delete", "flag.update", "flag.create", "project.update", "token.delete", "token.update", "token.create", "key.create",
		"token.create", "project.create", "setup"}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("audit actions:\n got %v\nwant %v", actions, want)
	}
	// Actor: the user's id, or "key:<id>" for a secret key.
	if all[len(all)-1]["actor"] != float64(owner.userID) {
		t.Errorf("setup actor: %v", all[len(all)-1]["actor"])
	}
	if all[4]["actor"] != fmt.Sprintf("key:%d", keyID) {
		t.Errorf("key actor: %v", all[4]["actor"])
	}

	// Nothing secret in the log: no passwords, no sk_ secrets or hashes, no
	// session or CSRF values, no TOTP material, no deleted persons' ids.
	raw := string(owner.get(audit + "?limit=500").Body)
	for _, secret := range []string{testPassword, "a brand new password", key.bearer, key.bearer[3:], auth.HashSecret(key.bearer), owner.cookie, owner.csrf,
		"$argon2id", "secret-user-id-42", "secret-anon-id"} {
		if strings.Contains(raw, secret) {
			t.Errorf("audit log contains %q", secret)
		}
	}

	// Paging: limit and before (exclusive, by id).
	page := owner.get(audit + "?limit=3").want(200).list()
	if len(page) != 3 || page[0]["id"] != all[0]["id"] {
		t.Fatalf("first page: %v", page)
	}
	next := owner.get(fmt.Sprintf("%s?limit=3&before=%d", audit, int64(page[2]["id"].(float64)))).want(200).list()
	if len(next) != 3 || next[0]["id"] != all[3]["id"] {
		t.Fatalf("second page: %v", next)
	}
	if l := owner.get(audit + "?before=1").want(200).list(); len(l) != 0 {
		t.Fatalf("before the first entry: %v", l)
	}
	if l := owner.get(audit).want(200).list(); len(l) != len(all) {
		t.Fatalf("default limit returned %d of %d", len(l), len(all))
	}
	owner.get(audit+"?limit=0").wantError(400, "bad_request")
	owner.get(audit+"?limit=x").wantError(400, "bad_request")
	owner.get(audit+"?before=-1").wantError(400, "bad_request")

	// Another organisation's log shows none of this.
	second := bootstrapOrg(t, e, "other-owner@example.com", "Other Inc", "p")
	other := e.login("other-owner@example.com", testPassword)
	for _, a := range other.get(fmt.Sprintf("/api/orgs/%d/audit?limit=500", second.OrgID)).want(200).list() {
		if a["actor"] == float64(owner.userID) {
			t.Fatalf("another org's audit log shows %v", a)
		}
	}
	other.get(audit).wantError(404, "not_found")

	// Reads and refused requests leave no entries.
	before := len(owner.get(audit + "?limit=500").list())
	owner.get(base + "/flags").want(200)
	owner.post(base+"/flags", map[string]any{"key": "BAD KEY"}).wantError(400, "invalid_flag")
	member.post(base+"/flags", map[string]any{"key": "nope"}).wantError(403, "forbidden")
	if after := len(owner.get(audit + "?limit=500").list()); after != before {
		t.Fatalf("audit grew from %d to %d without a mutation", before, after)
	}
}

func TestProjects(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()

	owner.post("/api/projects", map[string]any{"name": "x"}).wantError(400, "bad_request")
	owner.post("/api/projects", map[string]any{"org_id": org, "name": " "}).wantError(400, "bad_request")
	owner.post("/api/projects", map[string]any{"org_id": org, "name": strings.Repeat("x", 101)}).wantError(400, "bad_request")
	owner.post("/api/projects", map[string]any{"org_id": 9999, "name": "x"}).wantError(404, "not_found")
	owner.post("/api/projects", `{"org_id": `).wantError(400, "bad_request")
	owner.post("/api/projects", `[1]`).wantError(400, "bad_request")

	p := e.project(owner, org, "  Shop  ")
	now := e.clock.Now().UnixMilli()
	want := projectBody{ID: 1, OrgID: org, Name: "Shop", RetentionDays: 365, ReplayRetentionDays: 30, DropKeys: []string{},
		ScanLimitBytes: 0, MaxConcurrentQueries: 4, ReplaySampleRate: 0.1, CreatedAt: now, Token: p.Token}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("new project:\n got %+v\nwant %+v", p, want)
	}
	// org_id as a string works too (ids are string-or-number for the UI).
	var p2 projectBody
	owner.post("/api/projects", map[string]any{"org_id": fmt.Sprint(org), "name": "second"}).want(200).json(&p2)
	if p2.ID != 2 || p2.OrgID != org {
		t.Fatalf("second project: %+v", p2)
	}
	owner.post("/api/projects", map[string]any{"org_id": "abc", "name": "x"}).wantError(400, "bad_request")
}

func TestProjectLifecycle(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "Shop")
	base := fmt.Sprintf("/api/projects/%d", p.ID)

	// The automatic write token.
	toks := owner.get(base + "/tokens").want(200).list()
	if len(toks) != 1 || toks[0]["token"] != p.Token || toks[0]["status"] != "active" || !regexp.MustCompile(`^pk_[0-9A-Za-z]{32}$`).MatchString(p.Token) {
		t.Fatalf("automatic token: %v", toks)
	}

	// The exact field set of a Project.
	got := owner.get(base).want(200).object()
	fields := []string{"id", "org_id", "name", "retention_days", "replay_retention_days", "drop_keys", "scan_limit_bytes", "max_concurrent_queries", "replay_sample_rate", "created_at"}
	if len(got) != len(fields) {
		t.Fatalf("project fields: %v", got)
	}
	for _, f := range fields {
		if _, ok := got[f]; !ok {
			t.Errorf("project lacks %q", f)
		}
	}

	// PATCH: each setting, validation, and no partial application.
	bad := []map[string]any{
		{"retention_days": 0}, {"retention_days": 3651}, {"retention_days": 1.5}, {"retention_days": "30"},
		{"replay_retention_days": 0}, {"drop_keys": "email"}, {"drop_keys": []any{1}}, {"scan_limit_bytes": -1},
		{"max_concurrent_queries": 0}, {"max_concurrent_queries": 65}, {"replay_sample_rate": 1.01}, {"replay_sample_rate": -0.1},
		{"name": ""}, {"name": 5}, {"retention_days": 30, "max_concurrent_queries": 0},
	}
	for _, b := range bad {
		owner.patch(base, b).wantError(400, "bad_request")
	}
	if after := owner.get(base).object(); !reflect.DeepEqual(after, got) {
		t.Fatalf("a rejected PATCH changed the project:\n%v\n%v", got, after)
	}
	patched := owner.patch(base, map[string]any{"name": "Shop 2", "retention_days": 90, "replay_retention_days": 7, "drop_keys": []string{" email ", "phone", "email", ""},
		"scan_limit_bytes": 1 << 40, "max_concurrent_queries": 8, "replay_sample_rate": 0.25, "id": 77, "org_id": 99, "created_at": 1}).want(200).object()
	wantPatched := map[string]any{"id": float64(p.ID), "org_id": float64(org), "name": "Shop 2", "retention_days": 90.0, "replay_retention_days": 7.0,
		"drop_keys": []any{"email", "phone"}, "scan_limit_bytes": float64(1 << 40), "max_concurrent_queries": 8.0, "replay_sample_rate": 0.25, "created_at": got["created_at"]}
	if !reflect.DeepEqual(patched, wantPatched) {
		t.Fatalf("patched project:\n got %v\nwant %v", patched, wantPatched)
	}
	// A partial PATCH leaves the rest alone.
	if again := owner.patch(base, map[string]any{"retention_days": 91}).want(200).object(); again["name"] != "Shop 2" || again["retention_days"] != 91.0 || again["max_concurrent_queries"] != 8.0 {
		t.Fatalf("partial patch: %v", again)
	}
	e.publish()
	wantDoc := fmt.Sprintf(`{"version":1,"projects":{"%d":{"name":"Shop 2","retention_days":91,"replay_retention_days":7,"drop_keys":["email","phone"],"scan_limit_bytes":1099511627776,"max_concurrent_queries":8}}}`, p.ID)
	if doc := e.mustObject(publish.ProjectsKey); doc != wantDoc {
		t.Fatalf("projects.json\n got %s\nwant %s", doc, wantDoc)
	}

	// Ids grow by one and are not reused after a project is closed.
	second := e.project(owner, org, "second")
	if second.ID != p.ID+1 {
		t.Fatalf("second id %d", second.ID)
	}
	owner.del(fmt.Sprintf("/api/projects/%d", second.ID)).want(204)
	third := e.project(owner, org, "third")
	if third.ID != p.ID+2 {
		t.Fatalf("id after closing a project: %d (ids must never be reused)", third.ID)
	}
	ids := []float64{}
	for _, pr := range owner.get("/api/projects").want(200).list() {
		ids = append(ids, pr["id"].(float64))
	}
	if !reflect.DeepEqual(ids, []float64{float64(p.ID), float64(third.ID)}) {
		t.Fatalf("listed projects %v; the closed one must be gone", ids)
	}

	// Closing: tokens disabled in tokens.json, retention 0 in projects.json,
	// rulesets removed, the project gone from the API.
	e.publish()
	if _, ok := e.object(publish.FlagsKey(second.Token)); ok {
		t.Error("closed project still has a ruleset")
	}
	var td publish.TokensDoc
	if err := json.Unmarshal([]byte(e.mustObject(publish.TokensKey)), &td); err != nil {
		t.Fatal(err)
	}
	if td.Tokens[second.Token].Status != "disabled" || td.Tokens[p.Token].Status != "active" || td.Tokens[third.Token].Status != "active" {
		t.Fatalf("token statuses after close: %+v", td.Tokens)
	}
	var pd publish.ProjectsDoc
	if err := json.Unmarshal([]byte(e.mustObject(publish.ProjectsKey)), &pd); err != nil {
		t.Fatal(err)
	}
	if c := pd.Projects[fmt.Sprint(second.ID)]; c.RetentionDays != 0 || c.ReplayRetentionDays != 0 {
		t.Fatalf("closed project's retention: %+v", c)
	}
	// Ingestion's registry agrees: the token is known but disabled (not "unknown").
	reg := tokens.New(tokens.Options{Store: e.store})
	if err := reg.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tk, ok := reg.Lookup(second.Token); !ok || !tk.Disabled || tk.ProjectID != second.ID {
		t.Fatalf("closed project's token in ingestion: %+v, %v", tk, ok)
	}
}

func TestTokens(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	base := fmt.Sprintf("/api/projects/%d/tokens", p.ID)

	for _, b := range []map[string]any{
		{}, {"name": ""}, {"name": "x", "rate_limit": -1}, {"name": "x", "rate_limit": 1000001}, {"name": "x", "rate_limit": 1.5},
		{"name": "x", "allowed_origins": "https://a.example"}, {"name": "x", "allowed_origins": []string{"app.example.com"}},
		{"name": "x", "allowed_origins": []string{"https://app.example.com/path"}}, {"name": "x", "allowed_origins": []string{"*"}},
		{"name": "x", "allowed_origins": []string{"https://*.example.com"}}, {"name": "x", "allowed_origins": []string{"https://user@app.example.com"}},
		{"name": "x", "replay": "yes"},
	} {
		owner.post(base, b).wantError(400, "bad_request")
	}
	tok := owner.post(base, map[string]any{"name": " Web ", "allowed_origins": []string{"HTTPS://App.Example.com/", "https://app.example.com", "http://localhost:5173"},
		"rate_limit": 500, "replay": true}).want(200).object()
	want := map[string]any{"token": tok["token"], "name": "Web", "status": "active", "allowed_origins": []any{"https://app.example.com", "http://localhost:5173"},
		"rate_limit": 500.0, "replay": true, "created_at": float64(e.clock.Now().UnixMilli())}
	if !reflect.DeepEqual(tok, want) {
		t.Fatalf("token:\n got %v\nwant %v", tok, want)
	}
	token := tok["token"].(string)
	if !regexp.MustCompile(`^pk_[0-9A-Za-z]{32}$`).MatchString(token) {
		t.Fatalf("token format %q", token)
	}
	minimal := owner.post(base, map[string]any{"name": "minimal"}).want(200).object()
	if minimal["rate_limit"] != 0.0 || minimal["replay"] != false || !reflect.DeepEqual(minimal["allowed_origins"], []any{}) {
		t.Fatalf("defaults: %v", minimal)
	}

	one := base + "/" + token
	owner.patch(one, map[string]any{"status": "paused"}).wantError(400, "bad_request")
	owner.patch(one, map[string]any{"allowed_origins": []string{"nope"}}).wantError(400, "bad_request")
	owner.patch(base+"/pk_"+strings.Repeat("0", 32), map[string]any{"name": "x"}).wantError(404, "not_found")
	// Another project's token is not reachable through this project.
	other := e.project(owner, org, "other")
	owner.patch(base+"/"+other.Token, map[string]any{"name": "x"}).wantError(404, "not_found")
	owner.del(base+"/"+other.Token).wantError(404, "not_found")

	if got := owner.patch(one, map[string]any{"name": "Renamed"}).want(200).object(); got["name"] != "Renamed" || got["rate_limit"] != 500.0 || got["replay"] != true {
		t.Fatalf("partial patch: %v", got)
	}
	got := owner.patch(one, map[string]any{"status": "disabled", "allowed_origins": []string{}, "rate_limit": 0, "replay": false}).want(200).object()
	if got["status"] != "disabled" || got["rate_limit"] != 0.0 || got["replay"] != false || len(got["allowed_origins"].([]any)) != 0 {
		t.Fatalf("patch: %v", got)
	}
	e.publish()
	entry := func() (publish.TokenEntry, bool) {
		var td publish.TokensDoc
		if err := json.Unmarshal([]byte(e.mustObject(publish.TokensKey)), &td); err != nil {
			t.Fatal(err)
		}
		en, ok := td.Tokens[token]
		return en, ok
	}
	if en, ok := entry(); !ok || en.Status != "disabled" || en.ProjectID != p.ID {
		t.Fatalf("published entry: %+v, %v", en, ok)
	}
	if _, ok := e.object(publish.FlagsKey(token)); ok {
		t.Error("a disabled token has a ruleset")
	}
	owner.patch(one, map[string]any{"status": "active"}).want(200)
	e.publish()
	if _, ok := e.object(publish.FlagsKey(token)); !ok {
		t.Error("an active token has no ruleset")
	}

	// DELETE: gone from tokens.json (ingestion answers 401) and its ruleset deleted.
	owner.del(one).want(204)
	owner.del(one).wantError(404, "not_found")
	e.publish()
	if _, ok := entry(); ok {
		t.Error("deleted token still published")
	}
	if _, ok := e.object(publish.FlagsKey(token)); ok {
		t.Error("deleted token's ruleset still published")
	}
	reg := tokens.New(tokens.Options{Store: e.store})
	if err := reg.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Lookup(token); ok {
		t.Error("ingestion still knows the deleted token")
	}
	if _, ok := reg.Lookup(p.Token); !ok {
		t.Error("ingestion lost the project's other token")
	}
}

func TestSecretKeys(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	base := fmt.Sprintf("/api/projects/%d/keys", p.ID)

	for _, b := range []map[string]any{
		{"name": "x"}, {"name": "x", "scopes": []string{}}, {"name": "x", "scopes": []string{"root"}}, {"name": "x", "scopes": "query"},
		{"scopes": []string{"query"}}, {"name": "", "scopes": []string{"query"}},
	} {
		owner.post(base, b).wantError(400, "bad_request")
	}
	created := owner.post(base, map[string]any{"name": "CI", "scopes": []string{"admin", "query", "admin"}}).want(200).object()
	secret, _ := created["secret"].(string)
	if !regexp.MustCompile(`^sk_[0-9A-Za-z]{32}$`).MatchString(secret) {
		t.Fatalf("secret format %q", secret)
	}
	want := map[string]any{"id": created["id"], "name": "CI", "scopes": []any{"query", "admin"}, "prefix": "sk_…" + secret[len(secret)-4:],
		"created_at": float64(e.clock.Now().UnixMilli()), "last_used_at": nil, "secret": secret}
	if !reflect.DeepEqual(created, want) {
		t.Fatalf("created key:\n got %v\nwant %v", created, want)
	}
	// The secret is shown once: the list has the key without it.
	listBody := owner.get(base).want(200)
	if strings.Contains(string(listBody.Body), secret) || strings.Contains(string(listBody.Body), "secret") || strings.Contains(string(listBody.Body), "hash") {
		t.Fatalf("key list leaks: %s", listBody.Body)
	}
	list := listBody.list()
	if len(list) != 1 || list[0]["prefix"] != want["prefix"] || len(list[0]) != 6 {
		t.Fatalf("key list: %v", list)
	}
	// Only the SHA-256 is in the database and in keys.json.
	var stored string
	if err := e.db.R().QueryRow(`SELECT hash FROM keys WHERE id = ?`, created["id"]).Scan(&stored); err != nil || stored != auth.HashSecret(secret) {
		t.Fatalf("stored hash %q, %v", stored, err)
	}
	var dump strings.Builder
	rows, err := e.db.R().Query(`SELECT id || '|' || name || '|' || scopes || '|' || hash || '|' || prefix FROM keys`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		dump.WriteString(s)
	}
	rows.Close()
	if strings.Contains(dump.String(), secret) || strings.Contains(dump.String(), secret[3:]) {
		t.Fatal("the secret itself is stored in the database")
	}
	e.publish()
	wantDoc := fmt.Sprintf(`{"version":1,"keys":{"%s":{"project_id":%d,"scopes":["query","admin"]}}}`, auth.HashSecret(secret), p.ID)
	if doc := e.mustObject(publish.KeysKey); doc != wantDoc {
		t.Fatalf("keys.json\n got %s\nwant %s", doc, wantDoc)
	}

	// The key works until it is deleted.
	kc := e.anon()
	kc.bearer = secret
	kc.get(fmt.Sprintf("/api/projects/%d", p.ID)).want(200)
	owner.del(fmt.Sprintf("%s/%v", base, created["id"])).want(204)
	owner.del(fmt.Sprintf("%s/%v", base, created["id"])).wantError(404, "not_found")
	owner.del(base+"/abc").wantError(404, "not_found")
	kc.get(fmt.Sprintf("/api/projects/%d", p.ID)).wantError(401, "unauthorized")
	e.publish()
	if doc := e.mustObject(publish.KeysKey); doc != `{"version":2,"keys":{}}` {
		t.Fatalf("keys.json after delete: %s", doc)
	}

	// Malformed Authorization headers are 401, never a fall-through to "no auth".
	for _, bearer := range []string{"", "sk_short", p.Token, "sk_" + strings.Repeat("é", 16)} {
		c := e.anon()
		c.headers = map[string]string{"Authorization": "Bearer " + bearer}
		c.get(fmt.Sprintf("/api/projects/%d", p.ID)).wantError(401, "unauthorized")
	}
}

func TestFlagValidationAndHistory(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	base := fmt.Sprintf("/api/projects/%d/flags", p.ID)

	bad := []map[string]any{
		{}, {"key": ""}, {"key": "Has-Upper"}, {"key": "_leading"}, {"key": "sp ace"}, {"key": strings.Repeat("a", 65)}, {"key": 5},
		{"key": "f", "rules": map[string]any{}},
		{"key": "f", "rules": []any{map[string]any{"rollout": -1}}},
		{"key": "f", "rules": []any{map[string]any{"rollout": 10001}}},
		{"key": "f", "rules": []any{map[string]any{"rollout": 12.5}}},
		{"key": "f", "rules": []any{map[string]any{"rolout": 100}}},
		{"key": "f", "rules": []any{map[string]any{"variants": []any{map[string]any{"key": "a", "w": 5000}, map[string]any{"key": "b", "w": 4999}}}}},
		{"key": "f", "rules": []any{map[string]any{"variants": []any{map[string]any{"key": "a", "w": 5000}, map[string]any{"key": "a", "w": 5000}}}}},
		{"key": "f", "rules": []any{map[string]any{"when": map[string]any{"prop": "p", "op": "like", "value": "x"}}}},
		{"key": "f", "rules": []any{map[string]any{"when": map[string]any{"prop": "p", "op": "regex", "value": "("}}}},
		{"key": "f", "rules": []any{map[string]any{"when": map[string]any{"prop": "p", "op": "in", "value": "x"}}}},
		{"key": "f", "rules": []any{map[string]any{"when": map[string]any{"prop": "", "op": "eq", "value": "x"}}}},
		{"key": "f", "rules": []any{map[string]any{"when": map[string]any{"and": []any{}, "or": []any{}}}}},
		{"key": "f", "rules": []any{map[string]any{"stable_on": "user_id"}}},
		{"key": "f", "default": 5}, {"key": "f", "default": map[string]any{}}, {"key": "f", "default": ""},
		{"key": "f", "enabled": "yes"}, {"key": "f", "name": strings.Repeat("n", 201)},
	}
	for _, b := range bad {
		res := owner.post(base, b)
		if res.Status != 400 {
			t.Errorf("POST %v: status %d, want 400 (%s)", b, res.Status, res.Body)
		}
	}
	if l := owner.get(base).want(200).list(); len(l) != 0 {
		t.Fatalf("rejected definitions created flags: %v", l)
	}
	// The error says where the problem is.
	res := owner.post(base, map[string]any{"key": "f", "rules": []any{map[string]any{}, map[string]any{"variants": []any{map[string]any{"key": "a", "w": 1}}}}}).wantError(400, "invalid_flag")
	if !strings.Contains(string(res.Body), "rules[1].variants") {
		t.Errorf("validation error without a path: %s", res.Body)
	}

	rules := []any{map[string]any{
		"when":     map[string]any{"and": []any{map[string]any{"prop": "platform", "op": "eq", "value": "ios"}, map[string]any{"prop": "plan", "op": "is_set", "value": true}, map[string]any{"prop": "age", "op": "gte", "value": 18}}},
		"rollout":  2500,
		"variants": []any{map[string]any{"key": "control", "w": 5000}, map[string]any{"key": "b", "w": 5000}},
	}}
	var f1 map[string]any
	owner.post(base, map[string]any{"key": "new_checkout", "name": "New checkout", "description": "d", "enabled": true, "rules": rules, "default": false,
		"salt": "chosen-by-client", "version": 99}).want(200).json(&f1)
	salt, _ := f1["salt"].(string)
	if len(salt) < 4 || salt == "chosen-by-client" {
		t.Fatalf("salt %q must be generated by the server", salt)
	}
	for _, field := range []string{"key", "name", "description", "enabled", "salt", "rules", "default", "version", "updated_at", "updated_by"} {
		if _, ok := f1[field]; !ok {
			t.Errorf("flag lacks %q", field)
		}
	}
	if len(f1) != 10 || f1["version"] != 1.0 || f1["updated_by"] != float64(owner.userID) || f1["key"] != "new_checkout" || f1["enabled"] != true {
		t.Fatalf("created flag: %v", f1)
	}
	owner.post(base, map[string]any{"key": "new_checkout"}).wantError(409, "conflict")

	one := base + "/new_checkout"
	// PUT replaces the whole definition; key and salt cannot change.
	owner.put(one, map[string]any{"key": "renamed"}).wantError(400, "invalid_flag")
	owner.put(one, map[string]any{"rules": []any{map[string]any{"rollout": 99999}}}).wantError(400, "invalid_flag")
	owner.put(base+"/missing", map[string]any{}).wantError(404, "not_found")
	e.clock.Advance(time.Minute)
	editor := e.member(owner, org, "admin")
	var f2 map[string]any
	// The UI echoes the full object back, including read-only fields.
	echo := map[string]any{"key": "new_checkout", "name": "Renamed", "description": "", "enabled": false, "rules": []any{map[string]any{"rollout": 10000}}, "default": "legacy",
		"salt": "tampered", "version": 1, "updated_at": 1, "updated_by": 1}
	editor.put(one, echo).want(200).json(&f2)
	if f2["salt"] != salt || f2["version"] != 2.0 || f2["name"] != "Renamed" || f2["enabled"] != false || f2["default"] != "legacy" ||
		f2["updated_by"] != float64(editor.userID) || f2["updated_at"] != float64(e.clock.Now().UnixMilli()) {
		t.Fatalf("after PUT: %v", f2)
	}
	// Absent fields take their defaults: PUT is not a merge.
	var f3 map[string]any
	owner.put(one, map[string]any{"enabled": true}).want(200).json(&f3)
	if f3["name"] != "" || f3["default"] != nil || len(f3["rules"].([]any)) != 0 || f3["version"] != 3.0 || f3["salt"] != salt {
		t.Fatalf("PUT with only enabled: %v", f3)
	}
	if got := owner.get(one).want(200).object(); !reflect.DeepEqual(got, f3) {
		t.Fatalf("GET differs from the PUT response:\n%v\n%v", got, f3)
	}

	versions := owner.get(one + "/versions").want(200).list()
	if len(versions) != 3 {
		t.Fatalf("history has %d entries", len(versions))
	}
	for i, wantVersion := range []float64{3, 2, 1} {
		v := versions[i]
		snap, _ := v["flag"].(map[string]any)
		if len(v) != 4 || v["version"] != wantVersion || snap == nil || snap["version"] != wantVersion || snap["salt"] != salt {
			t.Fatalf("history entry %d: %v", i, v)
		}
	}
	if versions[1]["actor"] != float64(editor.userID) || versions[1]["ts"] != f2["updated_at"] || versions[0]["actor"] != float64(owner.userID) {
		t.Fatalf("history actors: %v", versions)
	}
	if !reflect.DeepEqual(versions[2]["flag"], f1) || !reflect.DeepEqual(versions[1]["flag"], f2) {
		t.Fatal("history does not hold the definitions as they were")
	}

	owner.del(one).want(204)
	owner.get(one).wantError(404, "not_found")
	owner.get(one+"/versions").wantError(404, "not_found")
	owner.del(one).wantError(404, "not_found")
	// Created again: a new salt, and the version numbering carries on.
	var f4 map[string]any
	owner.post(base, map[string]any{"key": "new_checkout"}).want(200).json(&f4)
	if f4["version"] != 4.0 || f4["salt"] == salt {
		t.Fatalf("recreated flag: %v", f4)
	}
}

// Every case of spec/vectors/flags.json through the API: the vector
// ruleset's flags are created with their fixed salts, published, and both
// the evaluate endpoint and the shared evaluator reading the published file
// must give the expected value.
func TestFlagVectorsThroughEvaluateAndPublishedRuleset(t *testing.T) {
	raw, err := os.ReadFile("../../spec/vectors/flags.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf struct {
		Ruleset struct {
			Flags []map[string]any `json:"flags"`
		} `json:"ruleset"`
		Cases []struct {
			Flag   string         `json:"flag"`
			Props  map[string]any `json:"props"`
			UserID string         `json:"user_id"`
			AnonID string         `json:"anon_id"`
			Expect any            `json:"expect"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "vectors")
	base := fmt.Sprintf("/api/projects/%d/flags", p.ID)
	created := map[string]bool{}
	for _, fl := range vf.Ruleset.Flags {
		key := fl["key"].(string)
		body := map[string]any{"key": key, "enabled": fl["enabled"], "rules": fl["rules"], "default": fl["default"]}
		res := owner.post(base, body)
		if key == "short_weights" {
			// Deliberately invalid for the control plane (weights sum to 1);
			// SDKs must cope with it, the API must refuse it.
			res.wantError(400, "invalid_flag")
			continue
		}
		res.want(200)
		// The vectors depend on the salt, which the API (rightly) does not
		// let a client choose: set it the way a restored backup would.
		setSalt(t, e, p.ID, key, fl["salt"].(string))
		created[key] = true
	}
	e.app.Publisher.Notify()
	e.publish()
	rs, err := flags.ParseRuleset([]byte(e.mustObject(publish.FlagsKey(p.Token))))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Flags) != len(created) {
		t.Fatalf("published %d flags, created %d", len(rs.Flags), len(created))
	}
	checked := 0
	for i, c := range vf.Cases {
		if !created[c.Flag] {
			continue
		}
		var out struct {
			Value any `json:"value"`
		}
		res := owner.post(base+"/"+c.Flag+"/evaluate", map[string]any{"props": c.Props, "user_id": c.UserID, "anon_id": c.AnonID}).want(200)
		if !strings.Contains(string(res.Body), `"value"`) {
			t.Fatalf("evaluate response without a value field: %s", res.Body)
		}
		res.json(&out)
		if !reflect.DeepEqual(out.Value, c.Expect) {
			t.Errorf("case %d (%s %v %q %q): evaluate = %#v, want %#v", i, c.Flag, c.Props, c.UserID, c.AnonID, out.Value, c.Expect)
		}
		if got := rs.Find(c.Flag).Eval(c.Props, c.UserID, c.AnonID); !reflect.DeepEqual(got, c.Expect) {
			t.Errorf("case %d (%s): published ruleset evaluates to %#v, want %#v", i, c.Flag, got, c.Expect)
		}
		checked++
	}
	if checked < 80 {
		t.Fatalf("only %d vector cases were checked", checked)
	}

	// Evaluate input handling.
	one := base + "/everyone/evaluate"
	owner.post(one, map[string]any{"props": []any{}}).wantError(400, "bad_request")
	owner.post(one, map[string]any{"props": "x"}).wantError(400, "bad_request")
	if got := owner.post(one, map[string]any{}).want(200).object(); got["value"] != true {
		t.Fatalf("evaluate with an empty body: %v", got)
	}
	if got := owner.post(one, map[string]any{"user_id": 42, "anon_id": "a"}).want(200).object(); got["value"] != true {
		t.Fatalf("evaluate with a numeric user_id: %v", got)
	}
	owner.post(base+"/missing/evaluate", map[string]any{}).wantError(404, "not_found")
	// Evaluating changes nothing.
	if v := owner.get(base + "/everyone").object()["version"]; v != 1.0 {
		t.Fatalf("evaluate bumped the version to %v", v)
	}
}

func TestFlagPublishing(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	other := e.project(owner, org, "other")
	base := fmt.Sprintf("/api/projects/%d", p.ID)
	var second struct {
		Token string `json:"token"`
	}
	owner.post(base+"/tokens", map[string]any{"name": "second"}).want(200).json(&second)
	e.publish()
	for _, tok := range []string{p.Token, second.Token, other.Token} {
		if got := e.mustObject(publish.FlagsKey(tok)); got != `{"v":1,"flags":[]}` {
			t.Fatalf("empty ruleset for %s: %s", tok, got)
		}
	}

	var f map[string]any
	owner.post(base+"/flags", map[string]any{"key": "beta", "enabled": true, "default": false,
		"rules": []any{map[string]any{"when": map[string]any{"prop": "country", "op": "in", "value": []any{"TR", "DE", 7}}, "rollout": 5000, "stable_on": "anon_id"}}}).want(200).json(&f)
	e.publish()
	want := fmt.Sprintf(`{"v":2,"flags":[{"key":"beta","enabled":true,"salt":"%s","rules":[{"when":{"prop":"country","op":"in","value":["TR","DE",7]},"rollout":5000,"stable_on":"anon_id"}],"default":false}]}`, f["salt"])
	for _, tok := range []string{p.Token, second.Token} {
		if got := e.mustObject(publish.FlagsKey(tok)); got != want {
			t.Fatalf("ruleset of %s\n got %s\nwant %s", tok, got, want)
		}
	}
	// The ruleset is public: no names, descriptions, authors or timestamps in it.
	if strings.Contains(want, "updated") || strings.Contains(want, "name") || strings.Contains(want, "description") {
		t.Fatal("management metadata in the public ruleset")
	}
	if got := e.mustObject(publish.FlagsKey(other.Token)); got != `{"v":1,"flags":[]}` {
		t.Fatalf("another project's ruleset changed: %s", got)
	}
	// Each change is one step of v, for every active token of the project.
	owner.put(base+"/flags/beta", map[string]any{"enabled": false}).want(200)
	e.publish()
	owner.del(base + "/flags/beta").want(204)
	e.publish()
	for _, tok := range []string{p.Token, second.Token} {
		if got := e.mustObject(publish.FlagsKey(tok)); got != `{"v":4,"flags":[]}` {
			t.Fatalf("after update + delete: %s", got)
		}
	}
}

func setSalt(t *testing.T, e *env, pid uint32, key, salt string) {
	t.Helper()
	if err := e.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE flags SET salt = ? WHERE project_id = ? AND key = ?`, salt, pid, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInsightsAndDashboards(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	other := e.project(owner, org, "other")
	base := fmt.Sprintf("/api/projects/%d", p.ID)

	query := map[string]any{"range": "30d", "interval": "day", "series": []any{map[string]any{"event": nil, "agg": "unique"}}, "ui": map[string]any{"chart": "bar"}, "anything": []any{1.0, "x"}}
	for _, b := range []map[string]any{
		{"kind": "trends", "query": query}, {"name": "x", "query": query}, {"name": "x", "kind": "pie", "query": query},
		{"name": "x", "kind": "trends"}, {"name": "x", "kind": "trends", "query": "SELECT"}, {"name": "x", "kind": "trends", "query": []any{}},
	} {
		owner.post(base+"/insights", b).wantError(400, "bad_request")
	}
	member := e.member(owner, org, "member")
	ins := member.post(base+"/insights", map[string]any{"name": " Daily actives ", "kind": "trends", "query": query}).want(200).object()
	want := map[string]any{"id": ins["id"], "name": "Daily actives", "kind": "trends", "query": query, "created_by": float64(member.userID), "updated_at": float64(e.clock.Now().UnixMilli())}
	if !reflect.DeepEqual(ins, want) {
		t.Fatalf("insight (the query must be stored verbatim, unknown fields included):\n got %v\nwant %v", ins, want)
	}
	id := int64(ins["id"].(float64))
	one := fmt.Sprintf("%s/insights/%d", base, id)
	if got := owner.get(one).want(200).object(); !reflect.DeepEqual(got, want) {
		t.Fatalf("GET insight: %v", got)
	}
	// Not reachable through another project.
	owner.get(fmt.Sprintf("/api/projects/%d/insights/%d", other.ID, id)).wantError(404, "not_found")
	owner.put(fmt.Sprintf("/api/projects/%d/insights/%d", other.ID, id), map[string]any{"name": "x", "query": query}).wantError(404, "not_found")
	owner.get(base+"/insights/0").wantError(404, "not_found")
	owner.get(base+"/insights/abc").wantError(404, "not_found")

	e.clock.Advance(time.Minute)
	upd := owner.put(one, map[string]any{"name": "Renamed", "query": map[string]any{"sql": "SELECT 1"}}).want(200).object()
	if upd["name"] != "Renamed" || upd["kind"] != "trends" || upd["created_by"] != float64(member.userID) || upd["updated_at"] != float64(e.clock.Now().UnixMilli()) ||
		!reflect.DeepEqual(upd["query"], map[string]any{"sql": "SELECT 1"}) {
		t.Fatalf("updated insight (kind and created_by are kept): %v", upd)
	}
	if got := owner.put(one, map[string]any{"name": "Renamed", "kind": "sql", "query": map[string]any{"sql": "SELECT 2"}}).want(200).object(); got["kind"] != "sql" {
		t.Fatalf("kind change: %v", got)
	}
	owner.put(one, map[string]any{"name": "", "query": query}).wantError(400, "bad_request")

	// Dashboards.
	otherInsight := owner.post(fmt.Sprintf("/api/projects/%d/insights", other.ID), map[string]any{"name": "foreign", "kind": "sql", "query": map[string]any{}}).want(200).object()
	item := func(insight any, x, y, w, h any) map[string]any {
		return map[string]any{"insight_id": insight, "x": x, "y": y, "w": w, "h": h}
	}
	for _, b := range []map[string]any{
		{"items": []any{}}, {"name": "d", "items": "x"},
		{"name": "d", "items": []any{item(99999, 0, 0, 6, 3)}},
		{"name": "d", "items": []any{item(otherInsight["id"], 0, 0, 6, 3)}}, // an insight of another project
		{"name": "d", "items": []any{item(id, -1, 0, 6, 3)}},
		{"name": "d", "items": []any{item(id, 0, 0, 1.5, 3)}},
		{"name": "d", "items": []any{map[string]any{"insight_id": id, "x": 0, "y": 0, "w": 6}}},
	} {
		owner.post(base+"/dashboards", b).wantError(400, "bad_request")
	}
	second := owner.post(base+"/insights", map[string]any{"name": "second", "kind": "funnel", "query": map[string]any{}}).want(200).object()
	d := member.post(base+"/dashboards", map[string]any{"name": "Overview", "items": []any{item(id, 0, 0, 8, 3), item(fmt.Sprint(second["id"]), 8, 0, 4, 3), item(id, 0, 3, 12, 2)}}).want(200).object()
	wantItems := []any{
		map[string]any{"insight_id": float64(id), "x": 0.0, "y": 0.0, "w": 8.0, "h": 3.0},
		map[string]any{"insight_id": second["id"], "x": 8.0, "y": 0.0, "w": 4.0, "h": 3.0},
		map[string]any{"insight_id": float64(id), "x": 0.0, "y": 3.0, "w": 12.0, "h": 2.0},
	}
	if len(d) != 4 || d["name"] != "Overview" || !reflect.DeepEqual(d["items"], wantItems) || d["updated_at"] != float64(e.clock.Now().UnixMilli()) {
		t.Fatalf("dashboard: %v", d)
	}
	dash := fmt.Sprintf("%s/dashboards/%v", base, d["id"])
	if got := owner.put(dash, map[string]any{"name": "Empty"}).want(200).object(); len(got["items"].([]any)) != 0 || got["name"] != "Empty" {
		t.Fatalf("PUT without items: %v", got)
	}
	owner.put(dash, map[string]any{"name": "Overview", "items": []any{item(id, 0, 0, 8, 3), item(second["id"], 8, 0, 4, 3)}}).want(200)
	owner.get(fmt.Sprintf("/api/projects/%d/dashboards/%v", other.ID, d["id"])).wantError(404, "not_found")

	// Deleting an insight removes its tiles; the dashboard survives.
	owner.del(one).want(204)
	owner.get(one).wantError(404, "not_found")
	owner.del(one).wantError(404, "not_found")
	left := owner.get(dash).want(200).object()["items"].([]any)
	if len(left) != 1 || left[0].(map[string]any)["insight_id"] != second["id"] {
		t.Fatalf("tiles after deleting an insight: %v", left)
	}
	if l := owner.get(base + "/insights").want(200).list(); len(l) != 1 {
		t.Fatalf("insights: %v", l)
	}
	owner.del(dash).want(204)
	owner.get(dash).wantError(404, "not_found")
	if l := owner.get(base + "/dashboards").want(200).list(); len(l) != 0 {
		t.Fatalf("dashboards: %v", l)
	}
	// None of this publishes anything: saved queries stay in the database.
	e.publish()
	for _, k := range e.s3.Keys(testBucket) {
		if strings.Contains(e.mustObject(k), "Overview") || strings.Contains(e.mustObject(k), "Daily actives") {
			t.Fatalf("saved content leaked into %s", k)
		}
	}
}

func TestDeletionRequests(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	base := fmt.Sprintf("/api/projects/%d/deletions", p.ID)

	for _, b := range []map[string]any{
		{}, {"user_ids": []string{}, "anon_ids": []string{" "}}, {"user_ids": "42"}, {"user_ids": []any{map[string]any{}}},
		{"user_ids": []string{strings.Repeat("x", 513)}}, {"user_ids": []string{"42"}, "note": strings.Repeat("n", 1001)},
	} {
		owner.post(base, b).wantError(400, "bad_request")
	}
	if l := owner.get(base).want(200).list(); len(l) != 0 {
		t.Fatalf("rejected requests were stored: %v", l)
	}

	d := owner.post(base, map[string]any{"user_ids": []any{"42", 1042, " 42 "}, "anon_ids": []string{"0190f1a2-b3c4"}, "note": " Ticket #1841 "}).want(200).object()
	id, _ := d["id"].(string)
	now := e.clock.Now().UnixMilli()
	want := map[string]any{"id": id, "user_ids": []any{"42", "1042"}, "anon_ids": []any{"0190f1a2-b3c4"}, "status": "pending",
		"created_at": float64(now), "created_by": float64(owner.userID), "note": "Ticket #1841"}
	if !reflect.DeepEqual(d, want) || !regexp.MustCompile(`^del_[0-9A-HJKMNP-TV-Z]{26}$`).MatchString(id) {
		t.Fatalf("deletion:\n got %v\nwant %v", d, want)
	}

	// The API call succeeded although nothing is in the store yet; the
	// publisher writes the tombstone.
	key := fmt.Sprintf("tombstones/p=%d/%013d-%s.json", p.ID, now, id)
	if _, ok := e.object(key); ok {
		t.Fatal("tombstone exists before the publisher ran")
	}
	e.publish()
	wantTomb := fmt.Sprintf(`{"id":"%s","ts_ms":%d,"person_keys":["u:42","u:1042"],"anon_ids":["0190f1a2-b3c4"]}`, id, now)
	if got := e.mustObject(key); got != wantTomb {
		t.Fatalf("tombstone\n got %s\nwant %s", got, wantTomb)
	}

	// A second request in the same millisecond gets a later ts_ms.
	d2 := owner.post(base, map[string]any{"anon_ids": []string{"only-anon"}}).want(200).object()
	e.publish()
	key2 := fmt.Sprintf("tombstones/p=%d/%013d-%s.json", p.ID, now+1, d2["id"])
	if got := e.mustObject(key2); got != fmt.Sprintf(`{"id":"%s","ts_ms":%d,"person_keys":[],"anon_ids":["only-anon"]}`, d2["id"], now+1) {
		t.Fatalf("second tombstone: %s", got)
	}
	list := owner.get(base).want(200).list()
	if len(list) != 2 || list[0]["id"] != d2["id"] || list[1]["id"] != id {
		t.Fatalf("list must be newest first: %v", list)
	}

	// Status follows the day manifests (spec/control.md §7).
	status := func(which any) string {
		for _, x := range owner.get(base).want(200).list() {
			if x["id"] == which {
				return x["status"].(string)
			}
		}
		t.Fatalf("deletion %v not listed", which)
		return ""
	}
	manifest := func(day string, v int, ts int64) {
		t.Helper()
		k := fmt.Sprintf("data/p=%d/d=%s/_m/%08d.json", p.ID, day, v)
		body := fmt.Sprintf(`{"version":%d,"project":%d,"day":%q,"tombstone_ts":%d,"files":[]}`, v, p.ID, day, ts)
		if _, err := e.store.Put(context.Background(), k, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		t.Helper()
		if _, _, err := e.app.Checker.CheckOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	manifest("2026-11-17", 1, 0)
	manifest("2026-11-18", 1, 0)
	check()
	if status(id) != "pending" || status(d2["id"]) != "pending" {
		t.Fatal("done before the compactor did anything")
	}
	manifest("2026-11-17", 2, now)
	check()
	if status(id) != "pending" {
		t.Fatal("done although one day is behind")
	}
	manifest("2026-11-18", 2, now)
	check()
	if status(id) != "done" || status(d2["id"]) != "pending" {
		t.Fatalf("statuses: %s, %s", status(id), status(d2["id"]))
	}
	manifest("2026-11-17", 3, now+1)
	manifest("2026-11-18", 3, now+1)
	check()
	if status(d2["id"]) != "done" {
		t.Fatal("second request not done after both days advanced")
	}
}

func TestOrgDelete(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	admin := e.member(owner, org, "admin")
	key, _ := e.secretKey(owner, p.ID, "admin")
	e.publish()

	owner.del(fmt.Sprintf("/api/orgs/%d", org)).want(204)
	for _, c := range []*client{owner, admin} {
		if m := c.me(); len(m.Orgs) != 0 {
			t.Fatalf("org still listed: %+v", m.Orgs)
		}
		if l := c.get("/api/projects").want(200).list(); len(l) != 0 {
			t.Fatalf("projects still listed: %v", l)
		}
		c.get(fmt.Sprintf("/api/projects/%d", p.ID)).wantError(404, "not_found")
		c.get(fmt.Sprintf("/api/orgs/%d/members", org)).wantError(404, "not_found")
	}
	key.get(fmt.Sprintf("/api/projects/%d", p.ID)).wantError(401, "unauthorized")
	e.publish()
	var td publish.TokensDoc
	_ = json.Unmarshal([]byte(e.mustObject(publish.TokensKey)), &td)
	if td.Tokens[p.Token].Status != "disabled" {
		t.Fatalf("token after org delete: %+v", td.Tokens)
	}
	if doc := e.mustObject(publish.KeysKey); !strings.Contains(doc, `"keys":{}`) {
		t.Fatalf("keys after org delete: %s", doc)
	}
	// Project ids of a deleted organisation are not handed out again.
	second := bootstrapOrg(t, e, "other-owner@example.com", "Other Inc", "p")
	if second.ProjectID <= p.ID {
		t.Fatalf("project id %d reused (deleted org had %d)", second.ProjectID, p.ID)
	}
}
