//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/mucan54/izlek/golib/objstore"
	"github.com/mucan54/izlek/ingest/server"
	"github.com/mucan54/izlek/ingest/wal"
)

const (
	token     = "pk_compat_test"
	projectID = 4242
)

type node struct {
	t     *testing.T
	store objstore.Store
	srv   *server.Server
	ts    *httptest.Server
	logs  *bytes.Buffer
}

// startNode runs an ingestion node on a file:// store. allowedOrigins, when
// non-empty, becomes the token's Origin allow-list.
func startNode(t *testing.T, allowedOrigins ...string) *node {
	t.Helper()
	if _, err := os.Stat("node_modules/posthog-node/package.json"); err != nil {
		t.Fatal("npm packages missing: run `npm ci` in ingest/compat first")
	}
	st, err := objstore.Open("file://"+t.TempDir(), objstore.S3Config{})
	if err != nil {
		t.Fatal(err)
	}
	origins, _ := json.Marshal(allowedOrigins)
	doc := fmt.Sprintf(`{"version":1,"tokens":{%q:{"project_id":%d,"status":"active","allowed_origins":%s}}}`, token, projectID, origins)
	if _, err := st.Put(context.Background(), "control/tokens.json", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	cfg := server.DefaultConfig()
	cfg.NodeID = "ing-compat"
	cfg.WALDir = filepath.Join(t.TempDir(), "wal")
	cfg.WALMaxLocalBytes = 1 << 30
	cfg.DrainDelay = 0
	cfg.RateIP = 0
	n := &node{t: t, store: st, logs: &bytes.Buffer{}}
	n.srv, err = server.New(cfg, st, slog.New(slog.NewJSONHandler(n.logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatal(err)
	}
	n.ts = httptest.NewServer(n.srv.Handler())
	t.Cleanup(func() {
		if n.ts != nil {
			n.stop()
		}
	})
	return n
}

type event struct {
	Event      string         `json:"event"`
	DistinctID any            `json:"distinct_id"`
	Properties map[string]any `json:"properties"`
	Set        map[string]any `json:"$set"`
	UUID       string         `json:"uuid"`
	Timestamp  string         `json:"timestamp"`
	web        bool
}

// requestCodes scrapes /metrics for requests_total{code,format}.
func (n *node) requestCodes() map[string]int {
	n.t.Helper()
	resp, err := http.Get(n.ts.URL + "/metrics")
	if err != nil {
		n.t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	out := map[string]int{}
	re := regexp.MustCompile(`(?m)^la_ingest_requests_total\{code="(\d+)",format="(\w+)"\} (\d+)$`)
	for _, m := range re.FindAllStringSubmatch(buf.String(), -1) {
		var c int
		fmt.Sscan(m[3], &c)
		out[m[2]+"/"+m[1]] = c
	}
	return out
}

// stop shuts the node down (seal + upload) and returns every PostHog event
// found in the WAL objects.
func (n *node) stop() []event {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := n.srv.Shutdown(ctx); err != nil {
		n.t.Fatalf("shutdown: %v", err)
	}
	n.ts.Close()
	n.ts = nil
	objs, err := n.store.List(ctx, "wal/")
	if err != nil {
		n.t.Fatal(err)
	}
	var events []event
	for _, o := range objs {
		raw, _, err := n.store.Get(ctx, o.Key, nil)
		if err != nil {
			n.t.Fatal(err)
		}
		zr, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			n.t.Fatal(err)
		}
		entries, torn, err := wal.ReadAll(zr)
		zr.Close()
		if err != nil || torn {
			n.t.Fatalf("segment %s: torn=%v err=%v", o.Key, torn, err)
		}
		for _, e := range entries {
			if e.Format != wal.FormatPostHog || e.ProjectID != projectID {
				n.t.Fatalf("unexpected WAL entry: format=%d project=%d", e.Format, e.ProjectID)
			}
			if d := time.Since(time.UnixMilli(int64(e.RecvTSMs))); d < 0 || d > 5*time.Minute {
				n.t.Fatalf("recv_ts %d", e.RecvTSMs)
			}
			var evs []event
			body := bytes.TrimSpace(e.Body)
			switch {
			case len(body) > 0 && body[0] == '[': // shape 3: array of events
				if err := json.Unmarshal(body, &evs); err != nil {
					n.t.Fatalf("WAL body is not a JSON event array: %v", err)
				}
			default:
				var env struct {
					Batch []event `json:"batch"`
					event
				}
				if err := json.Unmarshal(body, &env); err != nil {
					n.t.Fatalf("WAL body is not JSON: %v\n%s", err, body)
				}
				if env.Batch != nil { // shape 1: {api_key, batch}
					evs = env.Batch
				} else { // shape 2: single event
					evs = []event{env.event}
				}
			}
			for i := range evs {
				evs[i].web = e.Flags&wal.FlagWeb != 0
			}
			events = append(events, evs...)
		}
	}
	return events
}

func byName(events []event) map[string][]event {
	out := map[string][]event{}
	for _, e := range events {
		out[e.Event] = append(out[e.Event], e)
	}
	return out
}

func runNode(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node %v: %v\nstdout: %s\nstderr: %s", args, err, out, stderr.String())
	}
	if s := strings.TrimSpace(stderr.String()); s != "" {
		t.Errorf("SDK wrote to stderr:\n%s", s)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return []byte(lines[len(lines)-1])
}

func sdkVersion(t *testing.T, pkg string) string {
	raw, err := os.ReadFile(filepath.Join("node_modules", pkg, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Version string `json:"version"`
	}
	json.Unmarshal(raw, &p)
	return p.Version
}

func TestPostHogNode(t *testing.T) {
	n := startNode(t)
	t.Logf("posthog-node %s", sdkVersion(t, "posthog-node"))
	var res struct {
		Errors      []string `json:"errors"`
		QueueLength int      `json:"queueLength"`
		Flag        any      `json:"flag"`
		FlagEnabled any      `json:"flagEnabled"`
		Sent        int      `json:"sent"`
	}
	out := runNode(t, "posthog_node.cjs", n.ts.URL, token)
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("script output %q: %v", out, err)
	}
	if len(res.Errors) != 0 {
		t.Errorf("SDK reported errors: %v", res.Errors)
	}
	if res.QueueLength != 0 {
		t.Errorf("SDK queue not drained: %d", res.QueueLength)
	}
	if (res.Flag != nil && res.Flag != false) || (res.FlagEnabled != nil && res.FlagEnabled != false) {
		t.Errorf("flag results from the empty /flags response: %v %v", res.Flag, res.FlagEnabled)
	}
	codes := n.requestCodes()
	t.Logf("requests by format/code: %v", codes)
	for k, c := range codes {
		if !strings.HasSuffix(k, "/200") && !strings.HasPrefix(k, "other/") {
			t.Errorf("SDK received non-200 responses: %s x%d", k, c)
		}
	}
	if codes["posthog/200"] < 2 || codes["flags/200"] < 1 {
		t.Errorf("expected capture and flags requests, got %v", codes)
	}

	events := n.stop()
	by := byName(events)
	// The SDK adds its own $feature_flag_called event when a flag is read.
	if got := len(events) - len(by["$feature_flag_called"]); got != res.Sent {
		t.Errorf("WAL has %d events (plus %d $feature_flag_called), SDK sent %d", got, len(by["$feature_flag_called"]), res.Sent)
	}
	if len(by["node_capture"]) != 1 || len(by["$identify"]) != 1 || len(by["$create_alias"]) != 1 || len(by["node_bulk"]) != 12 {
		t.Fatalf("events in WAL: node_capture=%d $identify=%d $create_alias=%d node_bulk=%d",
			len(by["node_capture"]), len(by["$identify"]), len(by["$create_alias"]), len(by["node_bulk"]))
	}
	c := by["node_capture"][0]
	if c.DistinctID != "node-user-1" || c.Properties["plan"] != "pro" || c.Properties["$lib"] != "posthog-node" || c.Timestamp == "" || c.UUID == "" {
		t.Errorf("node_capture: %+v", c)
	}
	id := by["$identify"][0]
	set, _ := id.Properties["$set"].(map[string]any)
	if set == nil {
		set = id.Set
	}
	if id.DistinctID != "node-user-1" || set["name"] != "Node User" {
		t.Errorf("$identify: %+v", id)
	}
	al := by["$create_alias"][0]
	if al.DistinctID != "node-user-1" || al.Properties["alias"] != "node-anon-1" {
		t.Errorf("$create_alias: %+v", al)
	}
	seen := map[float64]bool{}
	for _, e := range by["node_bulk"] {
		i, _ := e.Properties["i"].(float64)
		seen[i] = true
		if e.Properties["text"] != "çğıöşü 🚀" || e.DistinctID != "node-user-2" || e.web {
			t.Errorf("node_bulk: %+v", e)
		}
	}
	if len(seen) != 12 {
		t.Errorf("node_bulk indexes: %v", seen)
	}
}

func TestPostHogWeb(t *testing.T) {
	// The page lives on its own origin; ingestion only accepts that origin
	// for this token, so the browser's CORS handling is part of the test.
	var n *node
	page, err := os.ReadFile("page.html")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile("node_modules/posthog-js/dist/array.full.no-external.js")
	if err != nil {
		t.Fatalf("posthog-js bundle: %v (run `npm ci` in ingest/compat)", err)
	}
	mux := http.NewServeMux()
	site := httptest.NewServer(mux)
	defer site.Close()
	n = startNode(t, site.URL)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		html := strings.ReplaceAll(string(page), "__INGEST_URL__", n.ts.URL)
		html = strings.ReplaceAll(html, "__TOKEN__", token)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(html))
	})
	mux.HandleFunc("/posthog.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write(bundle)
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	t.Logf("posthog-js %s", sdkVersion(t, "posthog-js"))

	var res struct {
		Problems  []string `json:"problems"`
		Responses []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
			Query  string `json:"query"`
			Status int    `json:"status"`
			CT     string `json:"contentType"`
			CE     string `json:"contentEncoding"`
			Head   string `json:"bodyHead"` // first 4 body bytes, hex
		} `json:"responses"`
		QueueLength      int    `json:"queueLength"`
		RetryQueueLength int    `json:"retryQueueLength"`
		DistinctID       string `json:"distinctId"`
	}
	out := runNode(t, "posthog_web.cjs", site.URL+"/")
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("script output %q: %v", out, err)
	}
	if len(res.Problems) != 0 {
		t.Errorf("browser reported problems:\n  %s", strings.Join(res.Problems, "\n  "))
	}
	if res.QueueLength != 0 || res.RetryQueueLength != 0 {
		t.Errorf("SDK queues not drained: queue=%d retry=%d", res.QueueLength, res.RetryQueueLength)
	}
	if res.DistinctID != "web-user-1" {
		t.Errorf("distinct id after identify: %q", res.DistinctID)
	}
	paths := map[string]int{}
	for _, r := range res.Responses {
		key := fmt.Sprintf("%s %s -> %d", r.Method, r.Path, r.Status)
		if m := regexp.MustCompile(`compression=[\w-]+`).FindString(r.Query); m != "" {
			key += " (" + m + ")"
		}
		if r.Method == "POST" {
			key += fmt.Sprintf(" [query keys %v content-type=%q content-encoding=%q body starts %s]", queryKeys(r.Query), r.CT, r.CE, r.Head)
		}
		paths[key]++
		if r.Status >= 400 {
			t.Errorf("browser got %d for %s %s%s", r.Status, r.Method, r.Path, r.Query)
		}
	}
	t.Logf("browser requests: %v", paths)
	codes := n.requestCodes()
	t.Logf("ingestion requests by format/code: %v", codes)
	for k, c := range codes {
		if !strings.HasSuffix(k, "/200") && !strings.HasSuffix(k, "/204") {
			t.Errorf("ingestion answered %s x%d", k, c)
		}
	}
	if codes["posthog/200"] < 1 {
		t.Errorf("no capture request reached the adapter: %v", codes)
	}

	events := n.stop()
	by := byName(events)
	for _, name := range []string{"web_capture", "$identify", "web_after_identify"} {
		if len(by[name]) != 1 {
			t.Errorf("event %s appears %d times in the WAL", name, len(by[name]))
		}
	}
	if len(by["$pageview"]) != 2 { // automatic on load + the manual one
		t.Errorf("$pageview appears %d times, want 2", len(by["$pageview"]))
	}
	if t.Failed() {
		t.Fatalf("events in WAL: %v", func() (names []string) {
			for _, e := range events {
				names = append(names, e.Event)
			}
			return
		}())
	}
	for _, e := range events {
		if e.Properties["token"] != token || !e.web || e.UUID == "" {
			t.Errorf("event %s: token=%v web=%v uuid=%q", e.Event, e.Properties["token"], e.web, e.UUID)
		}
		if lib := e.Properties["$lib"]; lib != "web" {
			t.Errorf("event %s: $lib=%v", e.Event, lib)
		}
	}
	wc := by["web_capture"][0]
	anon, _ := wc.Properties["distinct_id"].(string)
	if anon == "" || anon == "web-user-1" || wc.Properties["plan"] != "pro" {
		t.Errorf("web_capture: distinct_id=%q props=%v", anon, wc.Properties["plan"])
	}
	id := by["$identify"][0]
	if id.Properties["distinct_id"] != "web-user-1" || id.Properties["$anon_distinct_id"] != anon {
		t.Errorf("$identify: distinct_id=%v $anon_distinct_id=%v (anon was %q)", id.Properties["distinct_id"], id.Properties["$anon_distinct_id"], anon)
	}
	if set, _ := id.Set["name"].(string); set != "Web User" {
		if ps, _ := id.Properties["$set"].(map[string]any); ps["name"] != "Web User" {
			t.Errorf("$identify person properties missing: %+v", id)
		}
	}
	after := by["web_after_identify"][0]
	if after.Properties["distinct_id"] != "web-user-1" || after.Properties["text"] != "çğıöşü 🚀" {
		t.Errorf("web_after_identify: %v", after.Properties)
	}
	if s := strings.TrimSpace(n.logs.String()); s != "" {
		t.Errorf("ingestion logged warnings/errors:\n%s", s)
	}
}

// Known gap, kept executable so it cannot go unnoticed: posthog-node 6.x
// sends everything to the capture v1 endpoint (/i/v1/analytics/events) and
// has no legacy mode. Design §3.7 puts the v1 capture endpoints out of scope,
// so the adapter answers 404 and the SDK reports the batch as undelivered.
// When v1 support is added to the adapter this test must be replaced by a
// real compatibility test like TestPostHogNode.
func TestPostHogNodeV6CaptureV1NotSupported(t *testing.T) {
	n := startNode(t)
	t.Logf("posthog-node %s (npm alias posthog-node-v6)", sdkVersion(t, "posthog-node-v6"))
	var res struct {
		Errors []string `json:"errors"`
		Sent   int      `json:"sent"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "node", "posthog_node.cjs", n.ts.URL, token, "posthog-node-v6").Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err != nil {
		t.Fatalf("script output %q: %v", out, err)
	}
	codes := n.requestCodes()
	t.Logf("requests by format/code: %v; SDK errors: %d", codes, len(res.Errors))
	if codes["other/404"] == 0 || codes["posthog/200"] != 0 {
		t.Fatalf("expected the v1 capture path to be unknown (404), got %v", codes)
	}
	if len(res.Errors) == 0 || !strings.Contains(strings.Join(res.Errors, " "), "/i/v1/analytics/events") {
		t.Fatalf("expected the SDK to report undelivered capture v1 batches, got %v", res.Errors)
	}
	if events := n.stop(); len(events) != 0 {
		t.Fatalf("%d events reached the WAL", len(events))
	}
}

func queryKeys(rawQuery string) []string {
	v, _ := url.ParseQuery(strings.TrimPrefix(rawQuery, "?"))
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
