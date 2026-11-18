package server

import (
	"context"
	"testing"
)

const rulesetTokens = `{"version":1,"tokens":{
  "pk_test":{"project_id":7,"status":"active"},
  "pk_site":{"project_id":7,"status":"active","allowed_origins":["https://shop.example"]},
  "pk_off":{"project_id":7,"status":"disabled"}}}`

func TestRulesetEndpoint(t *testing.T) {
	h := newHarness(t, rulesetTokens, func(c *Config) { c.CompatPostHog = false })
	ctx := context.Background()
	body := `{"v":3,"flags":[{"key":"a","enabled":true,"salt":"s","rules":[],"default":true}]}`
	for _, tok := range []string{"pk_test", "pk_site", "pk_off", "pk_ghost"} {
		if _, err := h.store.Put(ctx, "flags/"+tok+".json", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	resp := h.do("GET", "/flags/pk_test.json", nil)
	if got := readAll(t, resp); resp.StatusCode != 200 || got != body {
		t.Fatalf("GET: %d %q", resp.StatusCode, got)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" || resp.Header.Get("Cache-Control") != rulesetMaxAge || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers: %v", resp.Header)
	}
	resp = h.do("GET", "/flags/pk_test.json", nil, "If-None-Match", etag)
	if got := readAll(t, resp); resp.StatusCode != 304 || got != "" || resp.Header.Get("ETag") != etag {
		t.Fatalf("conditional GET: %d %q %v", resp.StatusCode, got, resp.Header)
	}
	if st := h.status("GET", "/flags/pk_test.json", nil, "If-None-Match", `"other", W/`+etag); st != 304 {
		t.Fatalf("weak/list If-None-Match: %d", st)
	}
	if st := h.status("HEAD", "/flags/pk_test.json", nil); st != 200 {
		t.Fatalf("HEAD: %d", st)
	}
	if st := h.status("POST", "/flags/pk_test.json", nil); st != 405 {
		t.Fatalf("POST: %d", st)
	}

	// Disabled and unknown tokens are not served, whatever is in the store.
	for _, p := range []string{"/flags/pk_off.json", "/flags/pk_ghost.json", "/flags/../control/tokens.json", "/flags/.json", "/flags/a/b.json"} {
		if st := h.status("GET", p, nil); st != 404 {
			t.Fatalf("GET %s: %d", p, st)
		}
	}

	// The token's origin allow-list applies; preflights (If-None-Match is not a simple header) pass.
	resp = h.do("GET", "/flags/pk_site.json", nil, "Origin", "https://shop.example")
	readAll(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "https://shop.example" ||
		resp.Header.Get("Access-Control-Expose-Headers") != "Retry-After, ETag" {
		t.Fatalf("allowed origin: %d %v", resp.StatusCode, resp.Header)
	}
	resp = h.do("GET", "/flags/pk_site.json", nil, "Origin", "https://evil.example")
	readAll(t, resp)
	if resp.StatusCode != 403 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign origin: %d %v", resp.StatusCode, resp.Header)
	}
	resp = h.do("OPTIONS", "/flags/pk_site.json", nil, "Origin", "https://shop.example",
		"Access-Control-Request-Method", "GET", "Access-Control-Request-Headers", "if-none-match")
	readAll(t, resp)
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Headers") != "if-none-match" {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}

	// The copy is cached: a changed object shows up only after the TTL …
	if _, err := h.store.Put(ctx, "flags/pk_test.json", []byte(`{"v":4,"flags":[]}`)); err != nil {
		t.Fatal(err)
	}
	resp = h.do("GET", "/flags/pk_test.json", nil)
	if got := readAll(t, resp); got != body {
		t.Fatalf("cached copy expected, got %q", got)
	}
	// … and then with a new ETag.
	e, _ := h.srv.rulesets.get("pk_test")
	e.at = e.at.Add(-2 * h.cfg.FlagsCache)
	h.srv.rulesets.put("pk_test", e)
	resp = h.do("GET", "/flags/pk_test.json", nil, "If-None-Match", etag)
	if got := readAll(t, resp); resp.StatusCode != 200 || got != `{"v":4,"flags":[]}` || resp.Header.Get("ETag") == etag {
		t.Fatalf("after TTL: %d %q", resp.StatusCode, got)
	}

	// A token without a published ruleset answers 404 (the SDK keeps its last copy).
	h.store.Delete(ctx, "flags/pk_site.json")
	if st := h.status("GET", "/flags/pk_site.json", nil); st != 200 {
		t.Fatalf("still cached: %d", st)
	}
	e, _ = h.srv.rulesets.get("pk_site")
	e.at = e.at.Add(-2 * h.cfg.FlagsCache)
	h.srv.rulesets.put("pk_site", e)
	if st := h.status("GET", "/flags/pk_site.json", nil); st != 404 {
		t.Fatalf("deleted ruleset: %d", st)
	}

	// The PostHog /flags endpoint is a different route (compat is off here).
	if st := h.status("POST", "/flags", nil); st != 404 {
		t.Fatalf("/flags with compat off: %d", st)
	}
}
