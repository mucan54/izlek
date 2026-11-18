package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mucan54/izlek/control/db"
)

// fixtureCtx is what a matrix case may use to prepare the resource it acts on.
type fixtureCtx struct {
	e     *env
	owner *client
	org   int64
	pid   uint32
	base  string // /api/projects/<pid>
	n     int
}

func (f *fixtureCtx) seq() int { f.n++; return f.n }

type matrixCase struct {
	name    string
	method  string
	minRole string
	// prep creates what the request needs (as the owner) and returns the
	// path and body. It runs once per actor, so destructive requests always
	// find a fresh target.
	prep    func(f *fixtureCtx) (path string, body any)
	success int
	// orgLevel cases are not reachable with a project key at all.
	orgLevel bool
}

func matrix() []matrixCase {
	newToken := func(f *fixtureCtx) string {
		var t struct {
			Token string `json:"token"`
		}
		f.owner.post(f.base+"/tokens", map[string]any{"name": "t"}).want(200).json(&t)
		return t.Token
	}
	newKey := func(f *fixtureCtx) int64 {
		var k struct {
			ID int64 `json:"id"`
		}
		f.owner.post(f.base+"/keys", map[string]any{"name": "k", "scopes": []string{"query"}}).want(200).json(&k)
		return k.ID
	}
	newFlag := func(f *fixtureCtx) string {
		key := fmt.Sprintf("flag-%d", f.seq())
		f.owner.post(f.base+"/flags", map[string]any{"key": key, "enabled": true, "rules": []any{map[string]any{"rollout": 5000}}}).want(200)
		return key
	}
	insightBody := map[string]any{"name": "Daily", "kind": "trends", "query": map[string]any{"range": "30d", "interval": "day", "series": []any{}}}
	newInsight := func(f *fixtureCtx) int64 {
		var i struct {
			ID int64 `json:"id"`
		}
		f.owner.post(f.base+"/insights", insightBody).want(200).json(&i)
		return i.ID
	}
	newDashboard := func(f *fixtureCtx) int64 {
		var d struct {
			ID int64 `json:"id"`
		}
		f.owner.post(f.base+"/dashboards", map[string]any{"name": "Main", "items": []any{}}).want(200).json(&d)
		return d.ID
	}
	newMember := func(f *fixtureCtx) int64 {
		var m struct {
			UserID int64 `json:"user_id"`
		}
		f.owner.post(fmt.Sprintf("/api/orgs/%d/members", f.org), map[string]any{
			"email": fmt.Sprintf("target-%d@example.com", f.seq()), "role": "viewer", "password": testPassword}).want(200).json(&m)
		return m.UserID
	}
	static := func(suffix string, body any) func(f *fixtureCtx) (string, any) {
		return func(f *fixtureCtx) (string, any) { return f.base + suffix, body }
	}
	orgPath := func(suffix string, body any) func(f *fixtureCtx) (string, any) {
		return func(f *fixtureCtx) (string, any) { return fmt.Sprintf("/api/orgs/%d%s", f.org, suffix), body }
	}
	return []matrixCase{
		// §2 organisation
		{name: "members.list", method: "GET", minRole: "viewer", success: 200, orgLevel: true, prep: orgPath("/members", nil)},
		{name: "members.add", method: "POST", minRole: "owner", success: 200, orgLevel: true, prep: func(f *fixtureCtx) (string, any) {
			return fmt.Sprintf("/api/orgs/%d/members", f.org), map[string]any{"email": fmt.Sprintf("new-%d@example.com", f.seq()), "role": "member", "password": testPassword}
		}},
		{name: "members.update", method: "PATCH", minRole: "owner", success: 200, orgLevel: true, prep: func(f *fixtureCtx) (string, any) {
			return fmt.Sprintf("/api/orgs/%d/members/%d", f.org, newMember(f)), map[string]any{"role": "member"}
		}},
		{name: "members.remove", method: "DELETE", minRole: "owner", success: 204, orgLevel: true, prep: func(f *fixtureCtx) (string, any) {
			return fmt.Sprintf("/api/orgs/%d/members/%d", f.org, newMember(f)), nil
		}},
		{name: "audit.list", method: "GET", minRole: "admin", success: 200, orgLevel: true, prep: orgPath("/audit?limit=5", nil)},
		// §3 projects
		{name: "project.create", method: "POST", minRole: "admin", success: 200, orgLevel: true, prep: func(f *fixtureCtx) (string, any) {
			return "/api/projects", map[string]any{"org_id": f.org, "name": "another"}
		}},
		{name: "project.get", method: "GET", minRole: "viewer", success: 200, prep: static("", nil)},
		{name: "project.update", method: "PATCH", minRole: "admin", success: 200, prep: static("", map[string]any{"retention_days": 180})},
		// §4 tokens and keys
		{name: "tokens.list", method: "GET", minRole: "viewer", success: 200, prep: static("/tokens", nil)},
		{name: "tokens.create", method: "POST", minRole: "admin", success: 200, prep: static("/tokens", map[string]any{"name": "web"})},
		{name: "tokens.update", method: "PATCH", minRole: "admin", success: 200, prep: func(f *fixtureCtx) (string, any) {
			return f.base + "/tokens/" + newToken(f), map[string]any{"status": "disabled"}
		}},
		{name: "tokens.delete", method: "DELETE", minRole: "admin", success: 204, prep: func(f *fixtureCtx) (string, any) {
			return f.base + "/tokens/" + newToken(f), nil
		}},
		{name: "keys.list", method: "GET", minRole: "admin", success: 200, prep: static("/keys", nil)},
		{name: "keys.create", method: "POST", minRole: "admin", success: 200, prep: static("/keys", map[string]any{"name": "ci", "scopes": []string{"query"}})},
		{name: "keys.delete", method: "DELETE", minRole: "admin", success: 204, prep: func(f *fixtureCtx) (string, any) {
			return fmt.Sprintf("%s/keys/%d", f.base, newKey(f)), nil
		}},
		// §5 flags
		{name: "flags.list", method: "GET", minRole: "viewer", success: 200, prep: static("/flags", nil)},
		{name: "flags.create", method: "POST", minRole: "admin", success: 200, prep: func(f *fixtureCtx) (string, any) {
			return f.base + "/flags", map[string]any{"key": fmt.Sprintf("created-%d", f.seq())}
		}},
		{name: "flags.get", method: "GET", minRole: "viewer", success: 200, prep: func(f *fixtureCtx) (string, any) { return f.base + "/flags/" + newFlag(f), nil }},
		{name: "flags.put", method: "PUT", minRole: "admin", success: 200, prep: func(f *fixtureCtx) (string, any) {
			return f.base + "/flags/" + newFlag(f), map[string]any{"enabled": false}
		}},
		{name: "flags.delete", method: "DELETE", minRole: "admin", success: 204, prep: func(f *fixtureCtx) (string, any) { return f.base + "/flags/" + newFlag(f), nil }},
		{name: "flags.versions", method: "GET", minRole: "viewer", success: 200, prep: func(f *fixtureCtx) (string, any) {
			return f.base + "/flags/" + newFlag(f) + "/versions", nil
		}},
		{name: "flags.evaluate", method: "POST", minRole: "viewer", success: 200, prep: func(f *fixtureCtx) (string, any) {
			return f.base + "/flags/" + newFlag(f) + "/evaluate", map[string]any{"props": map[string]any{}, "user_id": "u", "anon_id": "a"}
		}},
		// §6 insights and dashboards
		{name: "insights.list", method: "GET", minRole: "viewer", success: 200, prep: static("/insights", nil)},
		{name: "insights.create", method: "POST", minRole: "member", success: 200, prep: static("/insights", insightBody)},
		{name: "insights.get", method: "GET", minRole: "viewer", success: 200, prep: func(f *fixtureCtx) (string, any) {
			return fmt.Sprintf("%s/insights/%d", f.base, newInsight(f)), nil
		}},
		{name: "insights.put", method: "PUT", minRole: "member", success: 200, prep: func(f *fixtureCtx) (string, any) {
			return fmt.Sprintf("%s/insights/%d", f.base, newInsight(f)), insightBody
		}},
		{name: "insights.delete", method: "DELETE", minRole: "member", success: 204, prep: func(f *fixtureCtx) (string, any) {
			return fmt.Sprintf("%s/insights/%d", f.base, newInsight(f)), nil
		}},
		{name: "dashboards.list", method: "GET", minRole: "viewer", success: 200, prep: static("/dashboards", nil)},
		{name: "dashboards.create", method: "POST", minRole: "member", success: 200, prep: static("/dashboards", map[string]any{"name": "Main", "items": []any{}})},
		{name: "dashboards.get", method: "GET", minRole: "viewer", success: 200, prep: func(f *fixtureCtx) (string, any) {
			return fmt.Sprintf("%s/dashboards/%d", f.base, newDashboard(f)), nil
		}},
		{name: "dashboards.put", method: "PUT", minRole: "member", success: 200, prep: func(f *fixtureCtx) (string, any) {
			return fmt.Sprintf("%s/dashboards/%d", f.base, newDashboard(f)), map[string]any{"name": "Renamed", "items": []any{}}
		}},
		{name: "dashboards.delete", method: "DELETE", minRole: "member", success: 204, prep: func(f *fixtureCtx) (string, any) {
			return fmt.Sprintf("%s/dashboards/%d", f.base, newDashboard(f)), nil
		}},
		// §7 deletions
		{name: "deletions.list", method: "GET", minRole: "admin", success: 200, prep: static("/deletions", nil)},
		{name: "deletions.create", method: "POST", minRole: "admin", success: 200, prep: static("/deletions", map[string]any{"user_ids": []string{"42"}})},
		// §8 query proxy
		{name: "q.trends", method: "POST", minRole: "viewer", success: 200, prep: static("/q/trends", map[string]any{"from": "2026-11-01", "to": "2026-11-18"})},
		{name: "q.sql", method: "POST", minRole: "viewer", success: 200, prep: static("/q/sql", map[string]any{"sql": "SELECT 1"})},
		{name: "q.schema", method: "POST", minRole: "viewer", success: 200, prep: static("/q/schema", map[string]any{})},
		{name: "q.replay", method: "GET", minRole: "viewer", success: 200, prep: func(f *fixtureCtx) (string, any) {
			return fmt.Sprintf("%s/q/replay?key=replay/p=%d/d=2026-11-18/%s/000001.bin.gz", f.base, f.pid, strings.Repeat("ab", 16)), nil
		}},
	}
}

// TestRoleMatrix runs every endpoint class as every kind of caller:
// the four roles, a member of another organisation, an anonymous client,
// and secret keys (admin-scoped, query-only, and one of another project).
func TestRoleMatrix(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	f := &fixtureCtx{e: e, owner: owner, org: org, pid: p.ID, base: fmt.Sprintf("/api/projects/%d", p.ID)}

	// A second organisation with its own owner and project. The first owner
	// creates the user (as a viewer of Acme) — then it is moved out, so the
	// outsider ends up belonging to no organisation that owns `p`.
	outsider := e.member(owner, org, "viewer")
	owner.del(fmt.Sprintf("/api/orgs/%d/members/%d", org, outsider.userID)).want(204)
	if len(outsider.me().Orgs) != 0 {
		t.Fatal("outsider still has an organisation")
	}
	otherOrgOwner, otherOrg, otherProject := secondOrg(t, e)

	actors := []struct {
		name string
		c    *client
		role string // "" = not a member
	}{
		{"owner", owner, "owner"},
		{"admin", e.member(owner, org, "admin"), "admin"},
		{"member", e.member(owner, org, "member"), "member"},
		{"viewer", e.member(owner, org, "viewer"), "viewer"},
	}
	adminKey, _ := e.secretKey(owner, p.ID, "admin")
	bothKey, _ := e.secretKey(owner, p.ID, "query", "admin")
	queryKey, _ := e.secretKey(owner, p.ID, "query")
	foreignKey, _ := e.secretKey(otherOrgOwner, otherProject, "admin")
	_ = otherOrg

	for _, mc := range matrix() {
		t.Run(mc.name, func(t *testing.T) {
			for _, a := range actors {
				path, body := mc.prep(f)
				res := a.c.do(mc.method, path, body)
				if db.RoleRank(a.role) >= db.RoleRank(mc.minRole) {
					if res.Status != mc.success {
						t.Errorf("%s as %s: status %d, want %d (%s)", mc.name, a.name, res.Status, mc.success, res.Body)
					}
				} else if res.Status != http.StatusForbidden {
					t.Errorf("%s as %s: status %d, want 403 (%s)", mc.name, a.name, res.Status, res.Body)
				} else {
					res.wantError(403, "forbidden")
				}
			}

			// Someone outside the organisation: the resource does not exist.
			for name, c := range map[string]*client{"user without org": outsider, "owner of another org": otherOrgOwner} {
				path, body := mc.prep(f)
				res := c.do(mc.method, path, body)
				if res.Status != http.StatusNotFound {
					t.Errorf("%s as %s: status %d, want 404 (%s)", mc.name, name, res.Status, res.Body)
				}
			}

			// No credentials at all.
			path, body := mc.prep(f)
			if res := e.anon().do(mc.method, path, body); res.Status != http.StatusUnauthorized {
				t.Errorf("%s anonymously: status %d, want 401", mc.name, res.Status)
			}

			// Secret keys.
			for name, kc := range map[string]*client{"admin key": adminKey, "query+admin key": bothKey} {
				path, body = mc.prep(f)
				res := kc.do(mc.method, path, body)
				switch {
				case mc.orgLevel:
					if res.Status != http.StatusForbidden {
						t.Errorf("%s with %s: status %d, want 403 (keys only reach /api/projects/{pid}/…)", mc.name, name, res.Status)
					}
				case db.RoleRank("admin") >= db.RoleRank(mc.minRole):
					if res.Status != mc.success {
						t.Errorf("%s with %s: status %d, want %d (%s)", mc.name, name, res.Status, mc.success, res.Body)
					}
				default:
					if res.Status != http.StatusForbidden {
						t.Errorf("%s with %s: status %d, want 403", mc.name, name, res.Status)
					}
				}
			}
			path, body = mc.prep(f)
			if res := queryKey.do(mc.method, path, body); res.Status != http.StatusForbidden {
				t.Errorf("%s with a query-only key: status %d, want 403", mc.name, res.Status)
			}
			path, body = mc.prep(f)
			res := foreignKey.do(mc.method, path, body)
			want := http.StatusNotFound
			if mc.orgLevel {
				want = http.StatusForbidden
			}
			if res.Status != want {
				t.Errorf("%s with another project's key: status %d, want %d", mc.name, res.Status, want)
			}
			path, body = mc.prep(f)
			bogus := e.anon()
			bogus.bearer = "sk_" + strings.Repeat("0", 32)
			if res := bogus.do(mc.method, path, body); res.Status != http.StatusUnauthorized {
				t.Errorf("%s with an unknown key: status %d, want 401", mc.name, res.Status)
			}
		})
	}

	// DELETE project (admin): needs a fresh project per actor.
	t.Run("project.close", func(t *testing.T) {
		for _, a := range actors {
			victim := e.project(owner, org, "victim-"+a.name)
			res := a.c.del(fmt.Sprintf("/api/projects/%d", victim.ID))
			if db.RoleRank(a.role) >= db.RoleRank("admin") {
				res.want(204)
			} else {
				res.wantError(403, "forbidden")
			}
		}
		victim := e.project(owner, org, "victim-outsider")
		path := fmt.Sprintf("/api/projects/%d", victim.ID)
		outsider.del(path).wantError(404, "not_found")
		otherOrgOwner.del(path).wantError(404, "not_found")
		e.anon().del(path).wantError(401, "unauthorized")
		foreignKey.del(path).wantError(404, "not_found")
		adminKey.del(path).wantError(404, "not_found") // the key belongs to project p, not to victim
		owner.get(path).want(200)
	})

	// GET /api/projects: everyone sees exactly their own organisations' projects.
	t.Run("project.list", func(t *testing.T) {
		for _, a := range actors {
			list := a.c.get("/api/projects").want(200).list()
			if len(list) == 0 {
				t.Errorf("%s sees no projects", a.name)
			}
			for _, pr := range list {
				if int64(pr["org_id"].(float64)) != org {
					t.Errorf("%s sees a project of org %v", a.name, pr["org_id"])
				}
			}
		}
		if l := outsider.get("/api/projects").want(200).list(); len(l) != 0 {
			t.Errorf("a user without organisations sees %d projects", len(l))
		}
		l := otherOrgOwner.get("/api/projects").want(200).list()
		if len(l) != 1 || uint32(l[0]["id"].(float64)) != otherProject {
			t.Errorf("other org's owner sees %v", l)
		}
		adminKey.get("/api/projects").wantError(403, "forbidden")
		adminKey.get("/api/me").wantError(403, "forbidden")
		e.anon().get("/api/projects").wantError(401, "unauthorized")
	})

	// DELETE org (owner only).
	t.Run("org.delete", func(t *testing.T) {
		path := fmt.Sprintf("/api/orgs/%d", org)
		for _, a := range actors[1:] {
			a.c.del(path).wantError(403, "forbidden")
		}
		outsider.del(path).wantError(404, "not_found")
		otherOrgOwner.del(path).wantError(404, "not_found")
		adminKey.del(path).wantError(403, "forbidden")
		e.anon().del(path).wantError(401, "unauthorized")
	})
}

// secondOrg creates an organisation that shares nothing with the one made
// by setup. There is no "create organisation" endpoint in v1, so it is
// inserted the way `la-control bootstrap` does it.
func secondOrg(t *testing.T, e *env) (*client, int64, uint32) {
	t.Helper()
	res := bootstrapOrg(t, e, "other-owner@example.com", "Other Inc", "other project")
	c := e.login("other-owner@example.com", testPassword)
	return c, res.OrgID, res.ProjectID
}

func TestKeysOfClosedProjectsStopWorking(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	key, _ := e.secretKey(owner, p.ID, "admin")
	base := fmt.Sprintf("/api/projects/%d", p.ID)
	key.get(base).want(200)
	key.get(base + "/flags").want(200)
	owner.del(base).want(204)
	key.get(base).wantError(401, "unauthorized")
	owner.get(base).wantError(404, "not_found")
	owner.get(base+"/tokens").wantError(404, "not_found")
	owner.patch(base, map[string]any{"name": "zombie"}).wantError(404, "not_found")
	owner.del(base).wantError(404, "not_found")
}

func TestKeyUsageIsRecorded(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	key, id := e.secretKey(owner, p.ID, "admin")
	base := fmt.Sprintf("/api/projects/%d", p.ID)
	find := func() map[string]any {
		for _, k := range owner.get(base + "/keys").want(200).list() {
			if int64(k["id"].(float64)) == id {
				return k
			}
		}
		t.Fatal("key not listed")
		return nil
	}
	if k := find(); k["last_used_at"] != nil {
		t.Fatalf("unused key has last_used_at %v", k["last_used_at"])
	}
	key.get(base).want(200)
	if k := find(); k["last_used_at"] != float64(e.clock.Now().UnixMilli()) {
		t.Fatalf("last_used_at = %v", k["last_used_at"])
	}
	// Mutations by a key are attributed to it in the audit log and in
	// updated_by, as "key:<id>".
	var f struct {
		UpdatedBy any `json:"updated_by"`
	}
	key.post(base+"/flags", map[string]any{"key": "by-key"}).want(200).json(&f)
	if f.UpdatedBy != fmt.Sprintf("key:%d", id) {
		t.Fatalf("updated_by = %v", f.UpdatedBy)
	}
	audit := owner.get(fmt.Sprintf("/api/orgs/%d/audit", org)).want(200).list()
	if audit[0]["action"] != "flag.create" || audit[0]["actor"] != fmt.Sprintf("key:%d", id) {
		t.Fatalf("audit entry: %v", audit[0])
	}
}
