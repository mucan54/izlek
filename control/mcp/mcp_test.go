package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeBackend struct {
	lastRequest Request
	lastArgs    string
}

func (b *fakeBackend) Tools(context.Context) []Tool {
	return []Tool{{
		Name: "echo", Title: "Echo", Description: "Returns its arguments.",
		InputSchema: map[string]any{"type": "object"},
		Annotations: &Annotations{ReadOnlyHint: true, IdempotentHint: true},
	}, {
		Name: "ünïcode tool", Description: "A name that is not header-safe.", InputSchema: map[string]any{"type": "object"},
	}}
}

func (b *fakeBackend) Call(ctx context.Context, name string, args json.RawMessage) (*CallResult, error) {
	b.lastRequest, b.lastArgs = RequestFrom(ctx), string(args)
	switch name {
	case "echo", "ünïcode tool":
		var v any
		_ = json.Unmarshal(args, &v)
		return &CallResult{Content: []Content{Text(string(args))}, StructuredContent: map[string]any{"arguments": v}}, nil
	case "fails":
		return &CallResult{Content: []Content{Text("it did not work")}, IsError: true}, nil
	case "scope":
		return nil, &HTTPError{Status: http.StatusForbidden, Header: http.Header{"Www-Authenticate": {`Bearer error="insufficient_scope"`}},
			Err: &Error{Code: CodeInvalidRequest, Message: "Insufficient scope"}}
	case "panic-free-internal":
		return nil, errors.New("database password is hunter2")
	}
	return nil, &Error{Code: CodeInvalidParams, Message: "Unknown tool: " + name}
}

type reply struct {
	status int
	header http.Header
	raw    string
	body   struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  map[string]any  `json:"result"`
		Error   *struct {
			Code    int            `json:"code"`
			Message string         `json:"message"`
			Data    map[string]any `json:"data"`
		} `json:"error"`
	}
}

type harness struct {
	t       *testing.T
	h       *Handler
	backend *fakeBackend
	seen    []string
}

func newHarness(t *testing.T) *harness {
	hn := &harness{t: t, backend: &fakeBackend{}}
	hn.h = &Handler{
		Info: Implementation{Name: "test-server", Version: "1.2.3"}, Instructions: "Be careful.", Backend: hn.backend,
		Observe: func(_ *http.Request, method, tool, outcome string) {
			hn.seen = append(hn.seen, method+"|"+tool+"|"+outcome)
		},
	}
	return hn
}

func (hn *harness) do(method, body string, headers map[string]string) *reply {
	hn.t.Helper()
	req := httptest.NewRequest(method, "/mcp", strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	hn.h.ServeHTTP(rec, req)
	r := &reply{status: rec.Code, header: rec.Header(), raw: rec.Body.String()}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &r.body); err != nil {
			hn.t.Fatalf("response is not JSON: %q", r.raw)
		}
		if r.body.JSONRPC != "2.0" {
			hn.t.Fatalf("response is not JSON-RPC 2.0: %s", r.raw)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			hn.t.Fatalf("Content-Type %q", ct)
		}
	}
	return r
}

func (r *reply) wantError(t *testing.T, status, code int) *reply {
	t.Helper()
	if r.status != status || r.body.Error == nil || r.body.Error.Code != code {
		t.Fatalf("status %d, body %s; want status %d with error %d", r.status, r.raw, status, code)
	}
	return r
}

func (r *reply) wantResult(t *testing.T) map[string]any {
	t.Helper()
	if r.status != 200 || r.body.Result == nil || r.body.Error != nil {
		t.Fatalf("status %d, body %s; want a result", r.status, r.raw)
	}
	return r.body.Result
}

// ---- legacy revisions: the initialize handshake ----

func TestInitializeNegotiatesTheVersion(t *testing.T) {
	hn := newHarness(t)
	for asked, want := range map[string]string{
		"2025-11-25": "2025-11-25",
		"2025-06-18": "2025-06-18",
		"2025-03-26": "2025-03-26",
		// Not spoken: the newest legacy revision is offered instead.
		"2024-11-05": "2025-11-25",
		"1999-01-01": "2025-11-25",
		// A handshake cannot select the stateless revision.
		"2026-07-28": "2025-11-25",
	} {
		res := hn.do("POST", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+asked+`","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`, nil).wantResult(t)
		if res["protocolVersion"] != want {
			t.Errorf("asked for %s: got %v, want %s", asked, res["protocolVersion"], want)
		}
		if res["instructions"] != "Be careful." || res["serverInfo"].(map[string]any)["name"] != "test-server" {
			t.Errorf("initialize result: %v", res)
		}
		if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
			t.Errorf("no tools capability: %v", res)
		}
		// Legacy results do not carry the 2026-07-28 members.
		if _, ok := res["resultType"]; ok {
			t.Errorf("resultType on a legacy result: %v", res)
		}
	}
	if r := hn.do("POST", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil); r.status != 200 || r.body.Error.Code != CodeInvalidParams {
		t.Fatalf("initialize without a version: %s", r.raw)
	}
	// No session is created: the server is stateless in every revision.
	r := hn.do("POST", `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, nil)
	if r.header.Get("Mcp-Session-Id") != "" || string(r.body.ID) != `"a"` {
		t.Fatalf("session %q, id %s", r.header.Get("Mcp-Session-Id"), r.body.ID)
	}
}

func TestLegacyRequests(t *testing.T) {
	hn := newHarness(t)
	legacy := map[string]string{"MCP-Protocol-Version": "2025-06-18"}

	// Notifications are accepted without a body.
	for _, n := range []string{"notifications/initialized", "notifications/cancelled"} {
		if r := hn.do("POST", `{"jsonrpc":"2.0","method":"`+n+`"}`, legacy); r.status != http.StatusAccepted || r.raw != "" {
			t.Fatalf("%s: %d %q", n, r.status, r.raw)
		}
	}
	// So is a response to a request this server never sent.
	if r := hn.do("POST", `{"jsonrpc":"2.0","id":7,"result":{}}`, legacy); r.status != http.StatusAccepted {
		t.Fatalf("client response: %d", r.status)
	}
	if res := hn.do("POST", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, legacy).wantResult(t); len(res) != 0 {
		t.Fatalf("ping: %v", res)
	}

	res := hn.do("POST", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, legacy).wantResult(t)
	tools := res["tools"].([]any)
	if len(tools) != 2 || len(res) != 1 {
		t.Fatalf("tools/list: %v", res)
	}
	echo := tools[0].(map[string]any)
	ann := echo["annotations"].(map[string]any)
	// All four hints are explicit: an absent hint would mean the protocol's
	// pessimistic default.
	if ann["readOnlyHint"] != true || ann["destructiveHint"] != false || ann["idempotentHint"] != true || ann["openWorldHint"] != false {
		t.Fatalf("annotations: %v", ann)
	}

	// Without the header the request is taken as 2025-03-26.
	res = hn.do("POST", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"a":1}}}`, nil).wantResult(t)
	if res["isError"] != false || res["structuredContent"].(map[string]any)["arguments"].(map[string]any)["a"] != float64(1) {
		t.Fatalf("tools/call: %v", res)
	}
	if hn.backend.lastRequest.ProtocolVersion != "2025-03-26" {
		t.Fatalf("assumed version %q", hn.backend.lastRequest.ProtocolVersion)
	}
	// Missing or null arguments are an empty object.
	for _, params := range []string{`{"name":"echo"}`, `{"name":"echo","arguments":null}`} {
		hn.do("POST", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":`+params+`}`, legacy).wantResult(t)
		if hn.backend.lastArgs != "{}" {
			t.Fatalf("arguments %q", hn.backend.lastArgs)
		}
	}
	// A failing tool is a result, not a protocol error.
	res = hn.do("POST", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fails"}}`, legacy).wantResult(t)
	if res["isError"] != true || res["content"].([]any)[0].(map[string]any)["text"] != "it did not work" {
		t.Fatalf("tool error: %v", res)
	}

	// Protocol errors arrive as JSON-RPC errors with HTTP 200: legacy
	// clients read errors only from successful responses.
	hn.do("POST", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`, legacy).wantError(t, 200, CodeInvalidParams)
	hn.do("POST", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`, legacy).wantError(t, 200, CodeInvalidParams)
	hn.do("POST", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":[1]}}`, legacy).wantError(t, 200, CodeInvalidParams)
	hn.do("POST", `{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{"cursor":"abc"}}`, legacy).wantError(t, 200, CodeInvalidParams)
	hn.do("POST", `{"jsonrpc":"2.0","id":6,"method":"resources/list"}`, legacy).wantError(t, 200, CodeMethodNotFound)
	// server/discover belongs to 2026-07-28.
	hn.do("POST", `{"jsonrpc":"2.0","id":6,"method":"server/discover"}`, legacy).wantError(t, 200, CodeMethodNotFound)
	// The backend's HTTP-level refusal keeps its status and headers.
	r := hn.do("POST", `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"scope"}}`, legacy).wantError(t, 403, CodeInvalidRequest)
	if !strings.Contains(r.header.Get("WWW-Authenticate"), "insufficient_scope") {
		t.Fatalf("challenge: %v", r.header)
	}
	// An unexpected failure does not leak its text.
	r = hn.do("POST", `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"panic-free-internal"}}`, legacy).wantError(t, 200, CodeInternalError)
	if strings.Contains(r.raw, "hunter2") {
		t.Fatalf("internal error text leaked: %s", r.raw)
	}

	// An unsupported revision in the header is refused with the list of
	// supported ones.
	r = hn.do("POST", `{"jsonrpc":"2.0","id":10,"method":"tools/list"}`, map[string]string{"MCP-Protocol-Version": "2024-11-05"}).wantError(t, 400, CodeUnsupportedProtocolVersion)
	if got := r.body.Error.Data["supported"].([]any); len(got) != 4 || got[0] != "2026-07-28" || r.body.Error.Data["requested"] != "2024-11-05" {
		t.Fatalf("error data: %v", r.body.Error.Data)
	}
}

// ---- 2026-07-28: stateless, per-request metadata ----

const modernMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"ExampleClient","version":"1.0.0"},"io.modelcontextprotocol/clientCapabilities":{}}`

func modern(method string, extra ...string) map[string]string {
	h := map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": method}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return h
}

func TestModernRequests(t *testing.T) {
	hn := newHarness(t)

	res := hn.do("POST", `{"jsonrpc":"2.0","id":"d1","method":"server/discover","params":{`+modernMeta+`}}`, modern("server/discover")).wantResult(t)
	if res["resultType"] != "complete" || res["instructions"] != "Be careful." || res["cacheScope"] != "public" || res["ttlMs"].(float64) <= 0 {
		t.Fatalf("discover: %v", res)
	}
	if v := res["supportedVersions"].([]any); len(v) != 4 || v[0] != "2026-07-28" || v[3] != "2025-03-26" {
		t.Fatalf("supportedVersions: %v", v)
	}
	if info := res["_meta"].(map[string]any)["io.modelcontextprotocol/serverInfo"].(map[string]any); info["name"] != "test-server" || info["version"] != "1.2.3" {
		t.Fatalf("serverInfo: %v", res["_meta"])
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("capabilities: %v", res)
	}

	res = hn.do("POST", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+modernMeta+`}}`, modern("tools/list")).wantResult(t)
	if res["resultType"] != "complete" || res["cacheScope"] != "private" || res["ttlMs"].(float64) != 300000 || len(res["tools"].([]any)) != 2 {
		t.Fatalf("tools/list: %v", res)
	}

	res = hn.do("POST", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"x":"y"},`+modernMeta+`}}`, modern("tools/call", "Mcp-Name", "echo")).wantResult(t)
	if res["resultType"] != "complete" || res["isError"] != false || res["_meta"] == nil {
		t.Fatalf("tools/call: %v", res)
	}
	if got := hn.backend.lastRequest; got.ProtocolVersion != "2026-07-28" || got.Client.Name != "ExampleClient" || got.Method != "tools/call" {
		t.Fatalf("request info: %+v", got)
	}
	// A name that is not header-safe travels Base64-encoded.
	encoded := "=?base64?" + base64.StdEncoding.EncodeToString([]byte("ünïcode tool")) + "?="
	hn.do("POST", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ünïcode tool",`+modernMeta+`}}`, modern("tools/call", "Mcp-Name", encoded)).wantResult(t)

	// ping and the handshake notifications no longer exist; an unknown
	// method is a 404 that still carries a JSON-RPC error.
	hn.do("POST", `{"jsonrpc":"2.0","id":4,"method":"ping","params":{`+modernMeta+`}}`, modern("ping")).wantError(t, 404, CodeMethodNotFound)
	hn.do("POST", `{"jsonrpc":"2.0","id":4,"method":"resources/list","params":{`+modernMeta+`}}`, modern("resources/list")).wantError(t, 404, CodeMethodNotFound)
	// Invalid params are a 400.
	hn.do("POST", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope",`+modernMeta+`}}`, modern("tools/call", "Mcp-Name", "nope")).wantError(t, 400, CodeInvalidParams)
	// A failing tool is still a 200 result.
	res = hn.do("POST", `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"fails",`+modernMeta+`}}`, modern("tools/call", "Mcp-Name", "fails")).wantResult(t)
	if res["isError"] != true {
		t.Fatalf("tool error: %v", res)
	}
	// An initialize request still gets the legacy handshake (dual-era server).
	res = hn.do("POST", `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`, nil).wantResult(t)
	if res["protocolVersion"] != "2025-11-25" {
		t.Fatalf("initialize next to modern requests: %v", res)
	}
}

func TestModernHeaderValidation(t *testing.T) {
	hn := newHarness(t)
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + modernMeta + `}}`
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo",` + modernMeta + `}}`
	cases := []struct {
		name    string
		body    string
		headers map[string]string
		status  int
		code    int
	}{
		{"version header missing", list, map[string]string{"Mcp-Method": "tools/list"}, 400, CodeHeaderMismatch},
		{"version header differs from body", list, map[string]string{"MCP-Protocol-Version": "2025-11-25", "Mcp-Method": "tools/list"}, 400, CodeHeaderMismatch},
		{"version in header only", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, modern("tools/list"), 400, CodeInvalidParams},
		{"no params at all", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, modern("tools/list"), 400, CodeInvalidParams},
		{"client capabilities missing", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`, modern("tools/list"), 400, CodeInvalidParams},
		{"Mcp-Method missing", list, map[string]string{"MCP-Protocol-Version": "2026-07-28"}, 400, CodeHeaderMismatch},
		{"Mcp-Method differs", list, modern("tools/call"), 400, CodeHeaderMismatch},
		{"Mcp-Method case differs", list, modern("Tools/List"), 400, CodeHeaderMismatch},
		{"Mcp-Name missing", call, modern("tools/call"), 400, CodeHeaderMismatch},
		{"Mcp-Name differs", call, modern("tools/call", "Mcp-Name", "other"), 400, CodeHeaderMismatch},
		{"Mcp-Name bad base64", call, modern("tools/call", "Mcp-Name", "=?base64?!!!?="), 400, CodeHeaderMismatch},
		{"unknown future version", strings.ReplaceAll(list, "2026-07-28", "2027-01-01"), map[string]string{"MCP-Protocol-Version": "2027-01-01", "Mcp-Method": "tools/list"}, 400, CodeUnsupportedProtocolVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := hn.do("POST", tc.body, tc.headers).wantError(t, tc.status, tc.code)
			if string(r.body.ID) != "1" {
				t.Fatalf("id %s", r.body.ID)
			}
		})
	}
	// The error names the versions a client can retry with.
	r := hn.do("POST", strings.ReplaceAll(list, "2026-07-28", "2027-01-01"), map[string]string{"MCP-Protocol-Version": "2027-01-01", "Mcp-Method": "tools/list"})
	if r.body.Error.Data["requested"] != "2027-01-01" || len(r.body.Error.Data["supported"].([]any)) != 4 {
		t.Fatalf("data: %v", r.body.Error.Data)
	}
	// Echoed header values cannot smuggle line breaks into the message.
	r = hn.do("POST", list, modern("tools/list\r\nX: y"))
	if strings.ContainsAny(r.body.Error.Message, "\r\n") {
		t.Fatalf("message %q", r.body.Error.Message)
	}
}

func TestTransportRules(t *testing.T) {
	hn := newHarness(t)
	// No event stream to open, no session to end.
	for _, m := range []string{"GET", "DELETE", "PUT", "PATCH"} {
		r := hn.do(m, "", nil)
		if r.status != http.StatusMethodNotAllowed || r.header.Get("Allow") != "POST" {
			t.Fatalf("%s: %d, Allow %q", m, r.status, r.header.Get("Allow"))
		}
	}
	ok := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if r := hn.do("POST", ok, map[string]string{"Content-Type": "text/plain"}); r.status != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain: %d", r.status)
	}
	if r := hn.do("POST", ok, map[string]string{"Content-Type": ""}); r.status != http.StatusUnsupportedMediaType {
		t.Fatalf("no content type: %d", r.status)
	}
	if r := hn.do("POST", ok, map[string]string{"Accept": "text/event-stream"}); r.status != http.StatusNotAcceptable {
		t.Fatalf("event stream only: %d", r.status)
	}
	for _, accept := range []string{"", "application/json", "*/*", "text/event-stream, application/json;q=0.9"} {
		if r := hn.do("POST", ok, map[string]string{"Accept": accept}); r.status != 200 {
			t.Fatalf("Accept %q: %d", accept, r.status)
		}
	}
	hn.h.MaxBodyBytes = 64
	if r := hn.do("POST", `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"`+strings.Repeat("x", 100)+`"}}`, nil); r.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body: %d", r.status)
	}
	hn.h.MaxBodyBytes = 0

	for name, body := range map[string]string{
		"empty":            ``,
		"not json":         `hello`,
		"truncated":        `{"jsonrpc":"2.0","id":1,`,
		"batch":            `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		"wrong version":    `{"jsonrpc":"1.0","id":1,"method":"ping"}`,
		"no method":        `{"jsonrpc":"2.0","id":1}`,
		"null id":          `{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		"object id":        `{"jsonrpc":"2.0","id":{},"method":"ping"}`,
		"boolean id":       `{"jsonrpc":"2.0","id":true,"method":"ping"}`,
		"request no id":    `{"jsonrpc":"2.0","method":"tools/list"}`,
		"array params":     `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[1]}`,
		"init notify":      `{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		"method not a str": `{"jsonrpc":"2.0","id":1,"method":5}`,
	} {
		if r := hn.do("POST", body, nil); r.status != http.StatusBadRequest || r.body.Error == nil {
			t.Errorf("%s: status %d, body %q", name, r.status, r.raw)
		}
	}
	// Numeric and string ids come back unchanged.
	for _, id := range []string{`0`, `-7`, `12345678901234567890`, `1.5`, `"abc"`, `""`} {
		r := hn.do("POST", `{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`, nil)
		if r.status != 200 || string(r.body.ID) != id {
			t.Errorf("id %s: status %d, echoed %s", id, r.status, r.body.ID)
		}
	}
}

func TestObserve(t *testing.T) {
	hn := newHarness(t)
	hn.do("POST", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	hn.do("POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`, nil)
	hn.do("POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fails"}}`, nil)
	hn.do("POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"made-up-by-the-client"}}`, nil)
	hn.do("POST", `{"jsonrpc":"2.0","id":1,"method":"made/up"}`, nil)
	want := []string{"tools/list||ok", "tools/call|echo|ok", "tools/call|fails|tool_error", "tools/call||error", "made/up||error"}
	if strings.Join(hn.seen, ",") != strings.Join(want, ",") {
		t.Fatalf("observed %v, want %v", hn.seen, want)
	}
}
