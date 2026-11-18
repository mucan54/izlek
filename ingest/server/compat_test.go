package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mucan54/izlek/ingest/wal"
)

func form(kv ...string) []byte {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return []byte(v.Encode())
}

func TestPostHogBodyVariants(t *testing.T) {
	h := newHarness(t, testTokens, nil)

	batch := `{"api_key":"pk_test","batch":[{"event":"$pageview","distinct_id":"u1","properties":{"$lib":"posthog-node"}},{"event":"signup","distinct_id":"u1"}],"sent_at":"2026-11-18T21:00:00.000Z"}`
	single := `{"token": "pk_test", "event": "single", "properties": {"distinct_id": "d-1", "k": "çğıöşü 🚀"}, "timestamp": "2026-11-18T21:00:00Z"}`
	array := `[{"event":"a1","properties":{"token":"pk_test","distinct_id":"d"}},{"event":"a2","properties":{"token":"pk_test","distinct_id":"d"}},{"event":"a3","properties":{"distinct_id":"d"}}]`
	propsTok := `{"event":"props-token","properties":{"token":"pk_test","distinct_id":"d"}}`
	batchPropsTok := `{"batch":[{"event":"b1","properties":{"token":"pk_test"}}]}`
	noTok := `{"batch":[{"event":"hdr","distinct_id":"d"}]}`
	bothKeys := `{"token":"pk_nope","api_key":"pk_test","event":"precedence","distinct_id":"d"}`
	spaced := "  \n" + batch + "\n"
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	cases := []struct {
		name, method, path string
		body               []byte
		hdr                []string
		want               string // JSON expected in the WAL, "" if none
		events             int
	}{
		{"batch plain", "POST", "/batch", []byte(batch), []string{"Content-Type", "application/json"}, batch, 2},
		{"batch trailing slash", "POST", "/batch/", []byte(batch), nil, batch, 2},
		{"i/v0/e", "POST", "/i/v0/e", []byte(single), nil, single, 1},
		{"i/v0/e/ with query", "POST", "/i/v0/e/?ip=1&_=1795046400000&ver=1.200.0", []byte(single), nil, single, 1},
		{"e array, token in first event properties", "POST", "/e/", []byte(array), nil, array, 3},
		{"e without slash", "POST", "/e", []byte(propsTok), nil, propsTok, 1},
		{"capture", "POST", "/capture", []byte(single), nil, single, 1},
		{"capture/", "POST", "/capture/", []byte(batchPropsTok), nil, batchPropsTok, 1},
		{"content-encoding gzip", "POST", "/batch/", gz([]byte(batch)), []string{"Content-Encoding", "gzip", "Content-Type", "application/json"}, batch, 2},
		{"compression=gzip-js", "POST", "/e/?compression=gzip-js&ip=1", gz([]byte(array)), []string{"Content-Type", "text/plain"}, array, 3},
		{"gzip magic only", "POST", "/e/", gz([]byte(array)), []string{"Content-Type", "text/plain"}, array, 3},
		{"gzip header but plain body", "POST", "/batch/", []byte(batch), []string{"Content-Encoding", "gzip"}, batch, 2},
		{"form data=base64", "POST", "/e/", form("data", b64(array), "compression", "base64"),
			[]string{"Content-Type", "application/x-www-form-urlencoded"}, array, 3},
		{"form data=base64, compression in query", "POST", "/e/?compression=base64", form("data", b64(single)),
			[]string{"Content-Type", "application/x-www-form-urlencoded"}, single, 1},
		{"form data=json", "POST", "/capture/", form("data", single), []string{"Content-Type", "application/x-www-form-urlencoded; charset=UTF-8"}, single, 1},
		{"form without content-type (sendBeacon)", "POST", "/e/?beacon=1", form("data", b64(array)), []string{"Content-Type", "text/plain"}, array, 3},
		{"form with unescaped base64 plus signs", "POST", "/e/", []byte("data=" + b64(single)), []string{"Content-Type", "application/x-www-form-urlencoded"}, single, 1},
		{"url-safe base64 without padding", "POST", "/e/", form("data", base64.RawURLEncoding.EncodeToString([]byte(single))), nil, single, 1},
		{"bare base64 body", "POST", "/e/?compression=base64", []byte(b64(single)), []string{"Content-Type", "text/plain"}, single, 1},
		{"base64 of gzip", "POST", "/e/", form("data", base64.StdEncoding.EncodeToString(gz([]byte(array)))), nil, array, 3},
		{"GET data=base64", "GET", "/e/?data=" + url.QueryEscape(b64(single)), nil, nil, single, 1},
		{"token from bearer", "POST", "/batch", []byte(noTok), []string{"Authorization", "Bearer pk_test"}, noTok, 1},
		{"token from ?t=", "POST", "/batch?t=pk_test", []byte(noTok), nil, noTok, 1},
		{"api_key wins over token", "POST", "/capture/", []byte(bothKeys), nil, bothKeys, 1},
		{"body token wins over bearer", "POST", "/batch", []byte(batch), []string{"Authorization", "Bearer pk_nope"}, batch, 2},
		{"surrounding whitespace is preserved", "POST", "/batch", []byte(spaced), nil, spaced, 2},
		{"empty batch is not written", "POST", "/batch", []byte(`{"api_key":"pk_test","batch":[]}`), nil, "", 0},
		{"empty array is not written", "POST", "/e/?t=pk_test", []byte(`[]`), nil, "", 0},
	}
	var want []string
	wantEvents := 0
	for _, c := range cases {
		resp := h.do(c.method, c.path, c.body, c.hdr...)
		body := readAll(t, resp)
		if resp.StatusCode != 200 || body != `{"status": 1}` || resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: status %d body %q", c.name, resp.StatusCode, body)
			continue
		}
		if c.want != "" {
			want = append(want, c.want)
			wantEvents += c.events
		}
	}

	rejects := []struct {
		name, method, path string
		body               []byte
		hdr                []string
		want               int
	}{
		{"unknown token", "POST", "/batch", []byte(`{"api_key":"pk_nope","batch":[{"event":"e"}]}`), nil, 401},
		{"no token anywhere", "POST", "/batch", []byte(noTok), nil, 401},
		{"body token unknown, valid bearer ignored", "POST", "/batch", []byte(`{"api_key":"pk_nope","batch":[{"event":"e"}]}`), []string{"Authorization", "Bearer pk_test"}, 401},
		{"disabled token", "POST", "/e/", []byte(`{"token":"pk_off","event":"e"}`), nil, 403},
		{"invalid json", "POST", "/batch", []byte(`{"api_key":"pk_test","batch":[`), nil, 400},
		{"json scalar", "POST", "/batch?t=pk_test", []byte(`"just a string"`), nil, 400},
		{"json number", "POST", "/batch?t=pk_test", []byte(`42`), nil, 400},
		{"empty body", "POST", "/batch?t=pk_test", nil, nil, 400},
		{"corrupt gzip", "POST", "/batch", append([]byte{0x1f, 0x8b, 8, 0}, "garbage"...), nil, 400},
		{"bad base64", "POST", "/e/", form("data", "!!!not-base64!!!"), nil, 400},
		{"form without data", "POST", "/e/", form("other", "x"), []string{"Content-Type", "application/x-www-form-urlencoded"}, 400},
		{"lz64 unsupported", "POST", "/e/?compression=lz64", form("data", "N4IgZg9hIFwgTgSwHYBcCmAnA"), nil, 400},
		{"unsupported content-encoding", "POST", "/batch", []byte(batch), []string{"Content-Encoding", "br"}, 400},
		{"PUT", "PUT", "/batch", []byte(batch), nil, 405},
		{"sub path", "POST", "/batch/extra", []byte(batch), nil, 404},
		{"decoded too large", "POST", "/batch", gz([]byte(`{"api_key":"pk_test","batch":[{"event":"` + strings.Repeat("x", 1<<20) + `"}]}`)), nil, 413},
		{"raw too large", "POST", "/batch", bytes.Repeat([]byte("x"), 2<<20+1), nil, 413},
	}
	for _, c := range rejects {
		if got := h.status(c.method, c.path, c.body, c.hdr...); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}

	// WAL: one format=2 entry per accepted non-empty request, body = the
	// JSON exactly as sent (after peeling the transport encodings).
	entries := h.shipped()
	if len(entries) != len(want) {
		t.Fatalf("WAL has %d entries, want %d", len(entries), len(want))
	}
	got := map[string]int{}
	for _, e := range entries {
		if e.Format != wal.FormatPostHog || e.ProjectID != 1017 || e.Flags != 0 {
			t.Fatalf("envelope %+v", e.Envelope)
		}
		got[string(e.Body)]++
	}
	wantCount := map[string]int{}
	for _, w := range want {
		wantCount[w]++
	}
	if !reflect.DeepEqual(got, wantCount) {
		t.Fatalf("WAL bodies differ from what was sent:\n got  %v\n want %v", got, wantCount)
	}
	if n := h.metric("la_ingest_records_in_total"); n != float64(wantEvents) {
		t.Fatalf("records_in_total = %v, want %d", n, wantEvents)
	}
	if n := h.metric(`la_ingest_requests_total{code="200",format="posthog"}`); n != float64(len(cases)) {
		t.Fatalf("requests_total{200,posthog} = %v, want %d", n, len(cases))
	}
}

func TestPostHogFlagsEndpoints(t *testing.T) {
	h := newHarness(t, testTokens, nil)
	// spec/flags.md §6, verbatim.
	const spec = `{ "config": { "enable_collect_everything": false }, "toolbarParams": {}, "isAuthenticated": false,
  "supportedCompression": ["gzip", "gzip-js"], "featureFlags": {}, "featureFlagPayloads": {},
  "flags": {}, "errorsWhileComputingFlags": false, "sessionRecording": false,
  "surveys": false, "capturePerformance": false, "autocapture_opt_out": true }`
	var want any
	if err := json.Unmarshal([]byte(spec), &want); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/decide"}, {"POST", "/decide/?v=3&ip=1"}, {"GET", "/decide/"}, {"POST", "/flags"},
		{"POST", "/flags/?v=2&config=true"}, {"GET", "/flags?anything=goes"}, {"PUT", "/flags/"}, {"DELETE", "/decide"},
	} {
		resp := h.do(c.method, c.path, []byte(`data=eyJ0b2tlbiI6ICJ3aGF0ZXZlciJ9`), "Origin", "https://any.example")
		body := readAll(t, resp)
		var got any
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%s %s: body is not JSON: %q", c.method, c.path, body)
		}
		if resp.StatusCode != 200 || !reflect.DeepEqual(got, want) || resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s: %d %s", c.method, c.path, resp.StatusCode, body)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "https://any.example" {
			t.Errorf("%s %s: no CORS header", c.method, c.path)
		}
	}
	if n := len(h.shipped()); n != 0 {
		t.Fatalf("flags endpoints wrote %d WAL entries", n)
	}
}

func TestPostHogCompatDisabled(t *testing.T) {
	h := newHarness(t, testTokens, func(c *Config) { c.CompatPostHog = false })
	body := []byte(`{"api_key":"pk_test","batch":[{"event":"e","distinct_id":"d"}]}`)
	for _, p := range []string{"/batch", "/batch/", "/i/v0/e", "/i/v0/e/", "/e", "/e/", "/capture", "/capture/", "/decide", "/decide/", "/flags", "/flags/"} {
		if got := h.status("POST", p, body); got != 404 {
			t.Errorf("%s with LA_COMPAT_POSTHOG=false: %d", p, got)
		}
	}
	if got := h.send(mkBatch(1, 1)); got != 200 {
		t.Fatalf("native path affected: %d", got)
	}
	if n := len(h.shipped()); n != 1 {
		t.Fatalf("WAL entries: %d", n)
	}
}

func TestScanPostHog(t *testing.T) {
	cases := []struct {
		in     string
		ok     bool
		events int
		token  string
	}{
		{`{"api_key":"a","token":"b","batch":[{"properties":{"token":"c"}}]}`, true, 1, "a"},
		{`{"token":"b","batch":[{"properties":{"token":"c"}},{}]}`, true, 2, "b"},
		{`{"batch":[{"properties":{"token":"c"}},{"properties":{"token":"d"}}]}`, true, 2, "c"},
		{`{"batch":[]}`, true, 0, ""},
		{`{"batch":[{"event":"x"}]}`, true, 1, ""},
		{`{"event":"x","properties":{"token":"p","nested":{"token":"no"}}}`, true, 1, "p"},
		{`{"properties":{"nested":{"token":"no"}}}`, true, 1, ""},
		{`[{"properties":{"token":"first"}},{"properties":{"token":"second"}}]`, true, 2, "first"},
		{`[]`, true, 0, ""},
		{`[1,2,3]`, true, 3, ""},
		{` [ { "properties" : { "token" : "sp" } } ] `, true, 1, "sp"},
		{`{"api_key":123,"token":"t"}`, true, 1, "t"},
		{`{"api_key":"","token":"t"}`, true, 1, "t"},
		{`{"api_key":"first","api_key":"last"}`, true, 1, "last"},
		{`{"api_key":"esc"}`, true, 1, "esc"},
		{`{"api_key":"pk_test\"q"}`, true, 1, `pk_test"q`},
		{`{"x":"} ] \" {","api_key":"after-tricky-string"}`, true, 1, "after-tricky-string"},
		{`{"x":{"api_key":"nested-not-top"},"batch":[{"properties":{"token":"bt"}}]}`, true, 1, "bt"},
		{`{"batch":{"not":"array"},"properties":{"token":"p"}}`, true, 1, "p"},
		{`{"batch":[{"properties":{"token":"x"}}],"batch":7,"properties":{"token":"p"}}`, true, 1, "p"},
		{`"string"`, false, 0, ""},
		{`12`, false, 0, ""},
		{`null`, false, 0, ""},
		{``, false, 0, ""},
	}
	for _, c := range cases {
		got := scanPostHog([]byte(c.in))
		if got.ok != c.ok || got.events != c.events || got.token != c.token {
			t.Errorf("scanPostHog(%s) = %+v, want ok=%v events=%d token=%q", c.in, got, c.ok, c.events, c.token)
		}
	}
}

// refScan is the obviously-correct (and slow) version of scanPostHog.
func refScan(doc any) (ok bool, events int, token string) {
	str := func(v any) string { s, _ := v.(string); return s }
	propsToken := func(ev any) string {
		m, _ := ev.(map[string]any)
		p, _ := m["properties"].(map[string]any)
		return str(p["token"])
	}
	switch v := doc.(type) {
	case []any:
		if len(v) > 0 {
			token = propsToken(v[0])
		}
		return true, len(v), token
	case map[string]any:
		events = 1
		batch, hasBatch := v["batch"].([]any)
		if hasBatch {
			events = len(batch)
		}
		switch {
		case str(v["api_key"]) != "":
			token = str(v["api_key"])
		case str(v["token"]) != "":
			token = str(v["token"])
		case hasBatch:
			if len(batch) > 0 {
				token = propsToken(batch[0])
			}
		default:
			token = propsToken(v)
		}
		return true, events, token
	}
	return false, 0, ""
}

// FuzzScanPostHog: on any valid JSON the hand-written scanner must agree
// with encoding/json; on anything else it must simply not panic.
func FuzzScanPostHog(f *testing.F) {
	for _, s := range []string{
		`{"api_key":"pk","batch":[{"event":"e","properties":{"token":"t"}}]}`,
		`[{"properties":{"token":"first"}},{}]`,
		`{"token":"t","event":"x","properties":{"token":"p"}}`,
		`{"api_key":"esc","batch":[]}`,
		`{"batch":[{"properties":{"token":"x"}}],"batch":7}`,
		`{"x":"} ] \" {","api_key":"k"}`, `[]`, `{}`, `"s"`, `1e9`, ` [ 1 , [ 2 ] , { "a" : [ ] } ] `, `{"a":{"b":{"c":[1,{"d":"\\"}]}}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		got := scanPostHog(data) // must not panic on arbitrary input
		if !json.Valid(data) {
			return
		}
		var doc any
		if err := json.Unmarshal(data, &doc); err != nil {
			return
		}
		ok, events, token := refScan(doc)
		// encoding/json replaces invalid UTF-8 inside strings; compare modulo that.
		gotToken := got.token
		if !utf8.ValidString(gotToken) {
			gotToken = strings.ToValidUTF8(gotToken, "�")
		}
		if got.ok != ok || got.events != events || gotToken != token {
			t.Fatalf("scanPostHog(%q) = {ok:%v events:%d token:%q}, reference {ok:%v events:%d token:%q}",
				data, got.ok, got.events, got.token, ok, events, token)
		}
	})
}

// FuzzDecodePostHog: arbitrary bodies under every transport flavour never
// panic and never yield more than the decoded-size limit.
func FuzzDecodePostHog(f *testing.F) {
	f.Add([]byte(`{"api_key":"pk"}`), uint8(0))
	f.Add(gz([]byte(`[{"event":"e"}]`)), uint8(1))
	f.Add([]byte("data=eyJhIjoxfQ%3D%3D&compression=base64"), uint8(2))
	f.Add([]byte("eyJhIjoxfQ=="), uint8(3))
	f.Fuzz(func(t *testing.T, body []byte, mode uint8) {
		r := mustRequest("POST", "/e/")
		switch mode % 5 {
		case 1:
			r.Header.Set("Content-Encoding", "gzip")
		case 2:
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		case 3:
			r.URL.RawQuery = "compression=base64"
		case 4:
			r.URL.RawQuery = "compression=gzip-js"
		}
		out, err := decodePostHogBody(r, body)
		if err == nil && (len(out) == 0 || len(out) > 1<<20) {
			t.Fatalf("decoded %d bytes without error", len(out))
		}
		if err == nil && json.Valid(out) {
			scanPostHog(out)
		}
	})
}
