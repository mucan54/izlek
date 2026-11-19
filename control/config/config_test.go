package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaultsMatchTheSpec(t *testing.T) {
	c, err := FromEnvFunc(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	// spec/control.md §11.
	if c.Listen != ":8090" || c.DBPath != "/var/lib/la/control.db" || c.QueryURL != "http://127.0.0.1:8081" || !c.CookieSecure {
		t.Fatalf("defaults: %+v", c)
	}
	if c.InternalSecret != "" || c.UIDir != "" || c.AdminListen != "" || c.TrustedProxies != 0 {
		t.Fatalf("defaults: %+v", c)
	}
	// spec/control.md §9: ≤ 2 s after a change, full republish every 5 min.
	if c.PublishDebounce <= 0 || c.PublishDebounce > 2*time.Second || c.RepublishEvery != 5*time.Minute {
		t.Fatalf("publish timing: %v / %v", c.PublishDebounce, c.RepublishEvery)
	}
	if c.LogLevel != slog.LevelInfo || c.QueryTimeout != 120*time.Second || c.DeletionCheck != time.Minute {
		t.Fatalf("defaults: %+v", c)
	}
	// MCP is served, but the OAuth server stays off until a public URL is set.
	if !c.MCPEnabled || c.OAuthEnabled() || c.PublicURL != "" || c.MCPResource() != "" || c.PublicIngestURL != "" ||
		c.MCPOrigins() != nil || !c.OAuthDCR || !c.OAuthCIMD {
		t.Fatalf("mcp defaults: %+v", c)
	}
}

func TestPublicURLs(t *testing.T) {
	c, err := FromEnvFunc(env(map[string]string{
		"LA_PUBLIC_URL": "https://Izlek.Example.com/", "LA_PUBLIC_INGEST_URL": "https://in.example.com/",
		"LA_MCP_ALLOWED_ORIGINS": " https://inspector.example.com , http://localhost:6274/ ", "LA_OAUTH_CLIENT_REGISTRATION": "cimd",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://izlek.example.com" || c.MCPResource() != "https://izlek.example.com/mcp" || !c.OAuthEnabled() {
		t.Fatalf("public url: %+v", c)
	}
	if c.PublicIngestURL != "https://in.example.com" || c.OAuthDCR || !c.OAuthCIMD {
		t.Fatalf("%+v", c)
	}
	if got := c.MCPOrigins(); len(got) != 2 || got[0] != "https://inspector.example.com" || got[1] != "http://localhost:6274" {
		t.Fatalf("origins: %q", got)
	}
	// Plain http is for local development only.
	for _, ok := range []string{"http://localhost:8090", "http://127.0.0.1:8090", "http://[::1]:8090"} {
		if c, err := FromEnvFunc(env(map[string]string{"LA_PUBLIC_URL": ok})); err != nil || c.PublicURL != ok {
			t.Errorf("%s: %v %q", ok, err, c.PublicURL)
		}
	}
	for _, bad := range []string{"http://izlek.example.com", "https://izlek.example.com/control", "izlek.example.com",
		"https://user@izlek.example.com", "https://izlek.example.com?x=1", "ftp://izlek.example.com"} {
		if _, err := FromEnvFunc(env(map[string]string{"LA_PUBLIC_URL": bad})); err == nil || !strings.Contains(err.Error(), "LA_PUBLIC_URL") {
			t.Errorf("LA_PUBLIC_URL=%q: err = %v", bad, err)
		}
	}
	// Switching MCP off switches the OAuth server off with it.
	c, err = FromEnvFunc(env(map[string]string{"LA_PUBLIC_URL": "https://izlek.example.com", "LA_MCP_ENABLED": "false"}))
	if err != nil || c.MCPEnabled || c.OAuthEnabled() {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestFromEnv(t *testing.T) {
	dir := t.TempDir()
	c, err := FromEnvFunc(env(map[string]string{
		"LA_CONTROL_LISTEN": "127.0.0.1:9000", "LA_CONTROL_ADMIN_LISTEN": "127.0.0.1:9001", "LA_CONTROL_DB": "/tmp/x.db",
		"LA_QUERY_URL": "https://query.internal:8443/", "LA_QUERY_INTERNAL_SECRET": "s3cret", "LA_UI_DIR": dir,
		"LA_COOKIE_SECURE": "false", "LA_TRUSTED_PROXIES": "2", "LA_CONTROL_QUERY_TIMEOUT_SECONDS": "30",
		"LA_CONTROL_PUBLISH_DEBOUNCE_MS": "0", "LA_CONTROL_REPUBLISH_SECONDS": "60", "LA_CONTROL_DELETION_CHECK_SECONDS": "10",
		"LA_LOG_LEVEL": "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Listen: "127.0.0.1:9000", AdminListen: "127.0.0.1:9001", DBPath: "/tmp/x.db", QueryURL: "https://query.internal:8443",
		InternalSecret: "s3cret", QueryTimeout: 30 * time.Second, UIDir: dir, CookieSecure: false, TrustedProxies: 2,
		PublishDebounce: 0, RepublishEvery: time.Minute, DeletionCheck: 10 * time.Second, LogLevel: slog.LevelDebug,
		MCPEnabled: true, OAuthDCR: true, OAuthCIMD: true}
	if c != want {
		t.Fatalf("\n got %+v\nwant %+v", c, want)
	}
}

func TestInvalidValuesAreReportedByName(t *testing.T) {
	for name, value := range map[string]string{
		"LA_COOKIE_SECURE": "maybe", "LA_TRUSTED_PROXIES": "-1", "LA_CONTROL_PUBLISH_DEBOUNCE_MS": "2001",
		"LA_CONTROL_REPUBLISH_SECONDS": "0", "LA_QUERY_URL": "ftp://x", "LA_UI_DIR": "/definitely/not/here", "LA_LOG_LEVEL": "loud",
		"LA_CONTROL_QUERY_TIMEOUT_SECONDS": "soon", "LA_MCP_ENABLED": "perhaps", "LA_PUBLIC_INGEST_URL": "in.example.com",
		"LA_MCP_ALLOWED_ORIGINS": "https://a.example.com/path", "LA_OAUTH_CLIENT_REGISTRATION": "magic",
	} {
		_, err := FromEnvFunc(env(map[string]string{name: value}))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: err = %v", name, value, err)
		}
	}
	// Several problems are all reported at once.
	_, err := FromEnvFunc(env(map[string]string{"LA_COOKIE_SECURE": "x", "LA_TRUSTED_PROXIES": "x"}))
	if err == nil || !strings.Contains(err.Error(), "LA_COOKIE_SECURE") || !strings.Contains(err.Error(), "LA_TRUSTED_PROXIES") {
		t.Errorf("joined errors: %v", err)
	}
}
