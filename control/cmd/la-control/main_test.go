package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
)

type cli struct {
	t   *testing.T
	env map[string]string
}

func (c *cli) run(args ...string) (code int, stdout, stderr string) {
	c.t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb, func(k string) string { return c.env[k] })
	return code, out.String(), errb.String()
}

func parseKV(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

func TestBootstrapCommand(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	c := &cli{t: t, env: map[string]string{
		"LA_CONTROL_DB": filepath.Join(dir, "db", "control.db"),
		"LA_STORE_URL":  "file://" + storeDir,
	}}
	args := []string{"bootstrap", "--email", "admin@example.com", "--password", "correct horse battery", "--org", "Acme", "--project", "Shop"}

	code, out, errOut := c.run(args...)
	if code != exitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	first := parseKV(out)
	if first["user_id"] != "1" || first["org_id"] != "1" || first["project_id"] != "1" ||
		!regexp.MustCompile(`^pk_[0-9A-Za-z]{32}$`).MatchString(first["token"]) || !regexp.MustCompile(`^sk_[0-9A-Za-z]{32}$`).MatchString(first["secret"]) {
		t.Fatalf("first run output:\n%s", out)
	}
	if len(first) != 5 || strings.Contains(errOut, first["secret"]) {
		t.Fatalf("unexpected output (stdout must be exactly the five key=value lines, the secret only there):\n%s\n%s", out, errOut)
	}
	// Published right away: a data plane started next finds its files.
	tokens, err := os.ReadFile(filepath.Join(storeDir, "control", "tokens.json"))
	if err != nil || !strings.Contains(string(tokens), first["token"]) {
		t.Fatalf("tokens.json after bootstrap: %s, %v", tokens, err)
	}
	keys, err := os.ReadFile(filepath.Join(storeDir, "control", "keys.json"))
	if err != nil || !strings.Contains(string(keys), auth.HashSecret(first["secret"])) || strings.Contains(string(keys), first["secret"]) {
		t.Fatalf("keys.json after bootstrap: %s, %v", keys, err)
	}
	for _, f := range []string{"control/projects.json", "flags/" + first["token"] + ".json"} {
		if _, err := os.Stat(filepath.Join(storeDir, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s not published: %v", f, err)
		}
	}

	// Second run: same ids and token, no secret, nothing new, exit 0.
	code, out, errOut = c.run(args...)
	second := parseKV(out)
	if code != exitOK || second["token"] != first["token"] || second["project_id"] != "1" || second["user_id"] != "1" || second["org_id"] != "1" {
		t.Fatalf("second run: exit %d\n%s\n%s", code, out, errOut)
	}
	if _, has := second["secret"]; has || !strings.Contains(errOut, "cannot be shown again") {
		t.Fatalf("second run must not print a secret:\n%s\n%s", out, errOut)
	}
	tokens2, _ := os.ReadFile(filepath.Join(storeDir, "control", "tokens.json"))
	if string(tokens2) != string(tokens) {
		t.Fatalf("a repeated bootstrap changed tokens.json:\n%s\n%s", tokens, tokens2)
	}

	// --json, password from the environment, --rotate-key, --no-publish.
	c.env["LA_BOOTSTRAP_PASSWORD"] = "correct horse battery"
	code, out, _ = c.run("bootstrap", "--email", "admin@example.com", "--org", "Acme", "--project", "Shop", "--json", "--rotate-key", "--no-publish")
	var res struct {
		ProjectID uint32   `json:"project_id"`
		Token     string   `json:"token"`
		Secret    string   `json:"secret"`
		Created   []string `json:"created"`
	}
	if code != exitOK || json.Unmarshal([]byte(out), &res) != nil || res.Token != first["token"] || res.Secret == "" || res.Secret == first["secret"] || len(res.Created) != 1 {
		t.Fatalf("json run: exit %d: %s", code, out)
	}
	if keys3, _ := os.ReadFile(filepath.Join(storeDir, "control", "keys.json")); strings.Contains(string(keys3), auth.HashSecret(res.Secret)) {
		t.Fatal("--no-publish published")
	}
	if code, _, _ := c.run("publish"); code != exitOK {
		t.Fatalf("publish: exit %d", code)
	}
	if keys4, _ := os.ReadFile(filepath.Join(storeDir, "control", "keys.json")); !strings.Contains(string(keys4), auth.HashSecret(res.Secret)) {
		t.Fatal("`la-control publish` did not publish the rotated key")
	}

	// Usage errors.
	for _, bad := range [][]string{
		{"bootstrap"}, {"bootstrap", "--email", "a@example.com"}, {"bootstrap", "--email", "a@example.com", "--org", "A", "--project", "P", "extra"},
		{"bootstrap", "--nope"}, {"frobnicate"}, {"backup"}, {"backup", "a", "b"},
	} {
		if code, _, _ := c.run(bad...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", bad, code, exitUsage)
		}
	}
	// Everything from the environment (how docker-compose.coolify.yml calls it): idempotent, no new key.
	c.env["LA_BOOTSTRAP_EMAIL"], c.env["LA_BOOTSTRAP_ORG"], c.env["LA_BOOTSTRAP_PROJECT"] = "admin@example.com", "Acme", "Shop"
	if code, out, _ := c.run("bootstrap", "--json"); code != exitOK || strings.Contains(out, `"secret"`) || !strings.Contains(out, res.Token) {
		t.Errorf("bootstrap from the environment: exit %d: %s", code, out)
	}
	delete(c.env, "LA_BOOTSTRAP_EMAIL")
	delete(c.env, "LA_BOOTSTRAP_ORG")
	delete(c.env, "LA_BOOTSTRAP_PROJECT")
	delete(c.env, "LA_BOOTSTRAP_PASSWORD")
	if code, out, _ := c.run("bootstrap", "--email", "new@example.com", "--password", "short", "--org", "A", "--project", "P"); code != exitFailure || out != "" {
		t.Errorf("weak password: exit %d, stdout %q", code, out)
	}
	if code, out, _ := c.run("help"); code != exitOK || !strings.Contains(out, "bootstrap") {
		t.Errorf("help: exit %d", code)
	}

	// An unreachable store: the database is still updated, the credentials
	// are still printed, and the exit code says publishing is outstanding.
	c.env["LA_STORE_URL"] = "s3://bucket"
	c.env["LA_S3_ENDPOINT"] = "http://127.0.0.1:1"
	code, out, errOut = c.run("bootstrap", "--email", "admin@example.com", "--password", "correct horse battery", "--org", "Acme", "--project", "Second")
	kv := parseKV(out)
	if code != exitPublish || kv["project_id"] != "2" || kv["secret"] == "" || !strings.Contains(errOut, "publishing failed") {
		t.Fatalf("unreachable store: exit %d\n%s\n%s", code, out, errOut)
	}
	// Without any store configured, bootstrap is a database-only operation.
	delete(c.env, "LA_STORE_URL")
	delete(c.env, "LA_S3_ENDPOINT")
	if code, _, errOut := c.run("bootstrap", "--email", "admin@example.com", "--password", "x", "--org", "Acme", "--project", "Second"); code != exitOK || !strings.Contains(errOut, "no object store configured") {
		t.Fatalf("no store: exit %d: %s", code, errOut)
	}
}

func TestBackupCommand(t *testing.T) {
	dir := t.TempDir()
	c := &cli{t: t, env: map[string]string{"LA_CONTROL_DB": filepath.Join(dir, "control.db")}}
	dest := filepath.Join(dir, "backups", "control-2026-11-18.db")

	// Nothing to back up yet: an error, and no empty database is created as a side effect.
	if code, _, _ := c.run("backup", dest); code != exitFailure {
		t.Fatalf("backup of a missing database: exit %d", code)
	}
	if _, err := os.Stat(c.env["LA_CONTROL_DB"]); err == nil {
		t.Fatal("backup created the source database")
	}

	if code, out, errOut := c.run("bootstrap", "--email", "admin@example.com", "--password", "correct horse battery", "--org", "Acme", "--project", "Shop"); code != exitOK {
		t.Fatalf("bootstrap: %d %s %s", code, out, errOut)
	}
	// Keep the source open, as the running service would.
	live, err := db.Open(context.Background(), c.env["LA_CONTROL_DB"])
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if code, _, errOut := c.run("backup", dest); code != exitOK {
		t.Fatalf("backup: exit %d: %s", code, errOut)
	}
	if code, _, _ := c.run("backup", dest); code != exitFailure {
		t.Fatalf("backup over an existing file: exit %d", code)
	}

	// The copy opens as a database and holds the data.
	b, err := db.Open(context.Background(), dest)
	if err != nil {
		t.Fatalf("backup does not open: %v", err)
	}
	defer b.Close()
	u, err := db.UserByEmail(context.Background(), b.R(), "admin@example.com")
	if err != nil || !strings.HasPrefix(u.PasswordHash, "$argon2id$v=19$m=65536,t=3,p=1$") {
		t.Fatalf("user in the backup: %+v, %v (the CLI must hash with the production parameters)", u, err)
	}
	projects, err := db.AllProjects(context.Background(), b.R())
	if err != nil || len(projects) != 1 || projects[0].Name != "Shop" {
		t.Fatalf("projects in the backup: %+v, %v", projects, err)
	}
	toks, _ := db.AllTokens(context.Background(), b.R())
	keys, _ := db.ActiveKeys(context.Background(), b.R())
	if len(toks) != 1 || len(keys) != 1 {
		t.Fatalf("backup has %d tokens and %d keys", len(toks), len(keys))
	}
	var integrity string
	if err := b.R().QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check: %q, %v", integrity, err)
	}
}

func TestServeRefusesBadConfiguration(t *testing.T) {
	dir := t.TempDir()
	base := map[string]string{"LA_CONTROL_DB": filepath.Join(dir, "control.db"), "LA_STORE_URL": "file://" + filepath.Join(dir, "store")}
	for name, extra := range map[string]map[string]string{
		"bad query url":   {"LA_QUERY_URL": "not a url"},
		"bad bool":        {"LA_COOKIE_SECURE": "maybe"},
		"missing ui dir":  {"LA_UI_DIR": filepath.Join(dir, "nope")},
		"debounce > 2 s":  {"LA_CONTROL_PUBLISH_DEBOUNCE_MS": "5000"},
		"bad log level":   {"LA_LOG_LEVEL": "chatty"},
		"no object store": {"LA_STORE_URL": ""},
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		c := &cli{t: t, env: env}
		if code, out, _ := c.run("serve"); code != exitUsage {
			t.Errorf("%s: exit %d, want %d (%s)", name, code, exitUsage, out)
		}
	}
}
