// Package mcp is a small Model Context Protocol server for the Streamable
// HTTP transport: JSON-RPC framing, protocol version handling and the
// tools/list and tools/call methods. What the tools are and who may call
// them is the Backend's business; authentication happens before a request
// reaches the Handler.
//
// One endpoint serves both protocol eras (modelcontextprotocol.io,
// "Versioning: Backward Compatibility"):
//
//   - Modern, 2026-07-28: stateless. Every request names its protocol
//     version in _meta and in the MCP-Protocol-Version header, mirrors its
//     method (and tool name) in Mcp-Method / Mcp-Name, and there is no
//     handshake. server/discover describes the server.
//   - Legacy, 2025-03-26 to 2025-11-25: an initialize request negotiates the
//     version; later requests carry it in MCP-Protocol-Version. This server
//     keeps no session for them either (it issues no Mcp-Session-Id), which
//     those revisions allow.
//
// Responses are always a single application/json object. The server never
// opens an event stream and never sends a request of its own, so GET and
// DELETE answer 405.
//
// The official Go SDK (github.com/modelcontextprotocol/go-sdk) covers the
// same ground and more, but every release that knows 2026-07-28 needs Go
// 1.25, and this repository builds with Go 1.24.
package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Protocol revisions.
const (
	Version20260728 = "2026-07-28"
	Version20251125 = "2025-11-25"
	Version20250618 = "2025-06-18"
	Version20250326 = "2025-03-26"

	// latestLegacy is offered to a handshake that asks for a revision this
	// server does not speak.
	latestLegacy = Version20251125
	// assumedVersion is what a request without MCP-Protocol-Version speaks
	// (the header did not exist before 2025-06-18).
	assumedVersion = Version20250326
)

// SupportedVersions lists the revisions the Handler speaks, newest first.
func SupportedVersions() []string {
	return []string{Version20260728, Version20251125, Version20250618, Version20250326}
}

func isModern(version string) bool { return version >= Version20260728 }

// Reserved _meta keys of 2026-07-28.
const (
	metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaServerInfo         = "io.modelcontextprotocol/serverInfo"
)

// JSON-RPC and MCP error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	CodeHeaderMismatch             = -32020
	CodeUnsupportedProtocolVersion = -32022
)

// Error is a JSON-RPC error object. A Backend returns it for protocol
// errors (unknown tool, malformed arguments object); failures of a tool
// itself are results with IsError set.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("mcp: %s (%d)", e.Message, e.Code) }

// HTTPError lets a Backend answer with a specific HTTP status and headers,
// for failures the transport must see: 403 with a WWW-Authenticate
// challenge when a token lacks a scope.
type HTTPError struct {
	Status int
	Header http.Header
	Err    *Error
}

func (e *HTTPError) Error() string { return e.Err.Error() }

// Implementation names a client or server.
type Implementation struct {
	Name       string `json:"name"`
	Title      string `json:"title,omitempty"`
	Version    string `json:"version"`
	WebsiteURL string `json:"websiteUrl,omitempty"`
}

// Annotations are the behaviour hints of a tool. All four are always sent:
// the protocol's defaults (not read-only, destructive, open world) are the
// pessimistic ones, so leaving a field out would say something.
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

// Tool is a tool definition as listed by tools/list.
type Tool struct {
	Name         string         `json:"name"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Annotations  *Annotations   `json:"annotations,omitempty"`
}

// Content is one block of a tool result; only text is used.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Text returns a text content block.
func Text(s string) Content { return Content{Type: "text", Text: s} }

// CallResult is the result of tools/call.
type CallResult struct {
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError"`
}

// Backend provides the tools.
type Backend interface {
	// Tools lists the tools the caller identified by ctx may use, in a
	// stable order.
	Tools(ctx context.Context) []Tool
	// Call runs a tool. arguments is a JSON object (never empty). Protocol
	// errors are returned as *Error or *HTTPError; any other error is
	// reported to the client as an internal error without its text.
	Call(ctx context.Context, name string, arguments json.RawMessage) (*CallResult, error)
}

// Request describes the request being served; a Backend reads it from the
// context with RequestFrom.
type Request struct {
	ProtocolVersion string
	Method          string
	// Client is what the client says about itself (2026-07-28 only; legacy
	// clients say it once, in initialize). Self-reported: for display and
	// logs, never for decisions.
	Client Implementation
}

type requestKey struct{}

// RequestFrom returns the Request of ctx.
func RequestFrom(ctx context.Context) Request {
	r, _ := ctx.Value(requestKey{}).(Request)
	return r
}

// Handler serves the MCP endpoint. The caller authenticates the request and
// validates its Origin first.
type Handler struct {
	Info         Implementation
	Instructions string
	Backend      Backend
	// MaxBodyBytes bounds a request body (default 1 MiB).
	MaxBodyBytes int64
	// ToolsTTL is the freshness hint of tools/list in 2026-07-28 (default
	// 5 minutes).
	ToolsTTL time.Duration
	// Observe, if set, is told about every JSON-RPC request once it has been
	// answered: its method, the tool name of a tools/call (only when the
	// tool exists) and the outcome ("ok", "tool_error", "error").
	Observe func(r *http.Request, method, tool, outcome string)
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  *string         `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

// validID reports whether raw is a JSON string or number: the only request
// ids MCP allows.
func validID(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case '"':
		var s string
		return json.Unmarshal(raw, &s) == nil
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var n json.Number
		return json.Unmarshal(raw, &n) == nil
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		status, raw = http.StatusInternalServerError, []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"Internal error"}}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

type errorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Error   *Error          `json:"error"`
}

// WriteError writes a JSON-RPC error response with an HTTP status. It is
// exported for the code in front of the Handler (authentication, Origin
// check), which has no request id to answer to.
func WriteError(w http.ResponseWriter, status int, id json.RawMessage, e *Error) {
	writeJSON(w, status, errorResponse{JSONRPC: "2.0", ID: id, Error: e})
}

func headerMismatch(format string, args ...any) *Error {
	return &Error{Code: CodeHeaderMismatch, Message: "Header mismatch: " + fmt.Sprintf(format, args...)}
}

// decodeHeaderValue undoes the Base64 sentinel encoding clients use for
// header values that are not plain ASCII ("=?base64?…?=").
func decodeHeaderValue(v string) (string, bool) {
	inner, ok := strings.CutPrefix(v, "=?base64?")
	if !ok {
		return v, true
	}
	inner, ok = strings.CutSuffix(inner, "?=")
	if !ok {
		return v, true
	}
	raw, err := base64.StdEncoding.DecodeString(inner)
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(inner); err != nil {
			return "", false
		}
	}
	return string(raw), true
}

func acceptsJSON(r *http.Request) bool {
	values := r.Header.Values("Accept")
	if len(values) == 0 {
		return true
	}
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err == nil && (mt == "application/json" || mt == "application/*" || mt == "*/*") {
				return true
			}
		}
	}
	return false
}

// ServeHTTP implements the Streamable HTTP transport.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// No standalone event stream (GET) and no sessions to end (DELETE).
		w.Header().Set("Allow", "POST")
		WriteError(w, http.StatusMethodNotAllowed, nil, &Error{Code: CodeInvalidRequest, Message: "This endpoint accepts POST only"})
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		WriteError(w, http.StatusUnsupportedMediaType, nil, &Error{Code: CodeInvalidRequest, Message: "Content-Type must be application/json"})
		return
	}
	if !acceptsJSON(r) {
		WriteError(w, http.StatusNotAcceptable, nil, &Error{Code: CodeInvalidRequest, Message: "Accept must include application/json"})
		return
	}
	limit := h.MaxBodyBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			WriteError(w, http.StatusRequestEntityTooLarge, nil, &Error{Code: CodeInvalidRequest, Message: "Request body is too large"})
			return
		}
		WriteError(w, http.StatusBadRequest, nil, &Error{Code: CodeParseError, Message: "Request body could not be read"})
		return
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '[' {
		// Batches existed only in 2025-03-26 and were removed again.
		WriteError(w, http.StatusBadRequest, nil, &Error{Code: CodeInvalidRequest, Message: "JSON-RPC batches are not supported; send one message per POST"})
		return
	}
	var msg message
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &msg) != nil {
		WriteError(w, http.StatusBadRequest, nil, &Error{Code: CodeParseError, Message: "Parse error: the body must be one JSON-RPC message"})
		return
	}
	if msg.JSONRPC != "2.0" {
		WriteError(w, http.StatusBadRequest, nil, &Error{Code: CodeInvalidRequest, Message: `Invalid request: "jsonrpc" must be "2.0"`})
		return
	}
	hasID := len(bytes.TrimSpace(msg.ID)) > 0
	if msg.Method == nil {
		// A response to a server request. This server never sends requests;
		// legacy clients are allowed to post responses, so it is accepted
		// and ignored.
		if hasID && (msg.Result != nil || msg.Error != nil) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		WriteError(w, http.StatusBadRequest, nil, &Error{Code: CodeInvalidRequest, Message: `Invalid request: "method" is missing`})
		return
	}
	method := *msg.Method
	if hasID && !validID(msg.ID) {
		WriteError(w, http.StatusBadRequest, nil, &Error{Code: CodeInvalidRequest, Message: `Invalid request: "id" must be a string or a number`})
		return
	}
	var id json.RawMessage
	if hasID {
		id = msg.ID
	}

	// params must be an object when present; _meta lives inside it.
	var params map[string]json.RawMessage
	if p := bytes.TrimSpace(msg.Params); len(p) > 0 && !bytes.Equal(p, []byte("null")) {
		if p[0] != '{' || json.Unmarshal(p, &params) != nil {
			h.fail(w, r, http.StatusBadRequest, id, method, &Error{Code: CodeInvalidParams, Message: `Invalid params: "params" must be an object`})
			return
		}
	}
	var meta map[string]json.RawMessage
	if m := bytes.TrimSpace(params["_meta"]); len(m) > 0 && m[0] == '{' {
		_ = json.Unmarshal(m, &meta)
	}
	var bodyVersion string
	hasBodyVersion := false
	if v, ok := meta[metaProtocolVersion]; ok {
		hasBodyVersion = json.Unmarshal(v, &bodyVersion) == nil && bodyVersion != ""
	}
	headerVersion := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version"))

	req := Request{Method: method}
	if v, ok := meta[metaClientInfo]; ok {
		_ = json.Unmarshal(v, &req.Client)
		req.Client.Name, req.Client.Version = clip(req.Client.Name, 100), clip(req.Client.Version, 50)
		req.Client.Title, req.Client.WebsiteURL = "", ""
	}

	// The legacy handshake. It selects legacy semantics whatever the
	// headers say.
	if method == "initialize" {
		if !hasID {
			WriteError(w, http.StatusBadRequest, nil, &Error{Code: CodeInvalidRequest, Message: "Invalid request: initialize must be a request"})
			return
		}
		h.initialize(w, r, id, params)
		return
	}

	version := headerVersion
	switch {
	case headerVersion == "" && !hasBodyVersion:
		version = assumedVersion
	case headerVersion == "":
		// A request that speaks per-request metadata must also carry the
		// header (2026-07-28, Streamable HTTP, "Server Validation").
		h.fail(w, r, http.StatusBadRequest, id, method, headerMismatch("the MCP-Protocol-Version header is required"))
		return
	}
	if !slices.Contains(SupportedVersions(), version) {
		h.fail(w, r, http.StatusBadRequest, id, method, &Error{
			Code: CodeUnsupportedProtocolVersion, Message: "Unsupported protocol version",
			Data: map[string]any{"supported": SupportedVersions(), "requested": version},
		})
		return
	}
	req.ProtocolVersion = version
	modern := isModern(version)

	if !hasID {
		// A notification: nothing to answer. (notifications/initialized and
		// notifications/cancelled from legacy clients end up here.)
		if !strings.HasPrefix(method, "notifications/") {
			WriteError(w, http.StatusBadRequest, nil, &Error{Code: CodeInvalidRequest, Message: `Invalid request: a request needs an "id"`})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if modern {
		if !hasBodyVersion {
			h.fail(w, r, http.StatusBadRequest, id, method, &Error{Code: CodeInvalidParams, Message: "Invalid params: _meta." + metaProtocolVersion + " is required"})
			return
		}
		if bodyVersion != headerVersion {
			h.fail(w, r, http.StatusBadRequest, id, method, headerMismatch("MCP-Protocol-Version header value %q does not match body value %q", clip(headerVersion, 40), clip(bodyVersion, 40)))
			return
		}
		mirrored := r.Header.Get("Mcp-Method")
		if mirrored == "" {
			h.fail(w, r, http.StatusBadRequest, id, method, headerMismatch("the Mcp-Method header is required"))
			return
		}
		if mirrored != method {
			h.fail(w, r, http.StatusBadRequest, id, method, headerMismatch("Mcp-Method header value %q does not match body value %q", clip(mirrored, 60), clip(method, 60)))
			return
		}
		if caps := bytes.TrimSpace(meta[metaClientCapabilities]); len(caps) == 0 || caps[0] != '{' {
			h.fail(w, r, http.StatusBadRequest, id, method, &Error{Code: CodeInvalidParams, Message: "Invalid params: _meta." + metaClientCapabilities + " is required"})
			return
		}
	}

	ctx := context.WithValue(r.Context(), requestKey{}, req)
	switch {
	case method == "tools/list":
		h.listTools(ctx, w, r, id, params, modern)
	case method == "tools/call":
		h.callTool(ctx, w, r, id, params, modern)
	case method == "server/discover" && modern:
		h.ok(w, r, id, method, "", map[string]any{
			"supportedVersions": SupportedVersions(),
			"capabilities":      capabilities(),
			"instructions":      h.Instructions,
			"ttlMs":             int64(time.Hour / time.Millisecond),
			"cacheScope":        "public",
		}, modern)
	case method == "ping" && !modern:
		h.ok(w, r, id, method, "", map[string]any{}, modern)
	default:
		// 2026-07-28 wants an unknown method to be visible to HTTP
		// intermediaries; legacy clients only read JSON-RPC errors from a
		// 200 response.
		status := http.StatusOK
		if modern {
			status = http.StatusNotFound
		}
		h.fail(w, r, status, id, method, &Error{Code: CodeMethodNotFound, Message: "Method not found: " + clip(method, 80)})
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	// Echoed input must not carry line breaks or other control characters.
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func capabilities() map[string]any {
	return map[string]any{"tools": map[string]any{"listChanged": false}}
}

func (h *Handler) observe(r *http.Request, method, tool, outcome string) {
	if h.Observe != nil {
		h.Observe(r, method, tool, outcome)
	}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, id json.RawMessage, method string, e *Error) {
	h.observe(r, method, "", "error")
	WriteError(w, status, id, e)
}

// ok writes a result. 2026-07-28 results carry resultType and the server's
// identity.
func (h *Handler) ok(w http.ResponseWriter, r *http.Request, id json.RawMessage, method, tool string, result map[string]any, modern bool) {
	if modern {
		result["resultType"] = "complete"
		result["_meta"] = map[string]any{metaServerInfo: h.Info}
	}
	outcome := "ok"
	if isErr, _ := result["isError"].(bool); isErr {
		outcome = "tool_error"
	}
	h.observe(r, method, tool, outcome)
	writeJSON(w, http.StatusOK, struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  map[string]any  `json:"result"`
	}{"2.0", id, result})
}

// initialize answers the legacy handshake: the requested revision if this
// server speaks it as a legacy revision, otherwise the newest legacy one.
func (h *Handler) initialize(w http.ResponseWriter, r *http.Request, id json.RawMessage, params map[string]json.RawMessage) {
	var requested string
	if json.Unmarshal(params["protocolVersion"], &requested) != nil || requested == "" {
		h.fail(w, r, http.StatusOK, id, "initialize", &Error{Code: CodeInvalidParams, Message: `Invalid params: "protocolVersion" is required`})
		return
	}
	version := latestLegacy
	if slices.Contains(SupportedVersions(), requested) && !isModern(requested) {
		version = requested
	}
	result := map[string]any{
		"protocolVersion": version,
		"capabilities":    capabilities(),
		"serverInfo":      h.Info,
	}
	if h.Instructions != "" {
		result["instructions"] = h.Instructions
	}
	h.ok(w, r, id, "initialize", "", result, false)
}

func (h *Handler) listTools(ctx context.Context, w http.ResponseWriter, r *http.Request, id json.RawMessage, params map[string]json.RawMessage, modern bool) {
	// Everything fits on one page, so no cursor is ever handed out.
	if c := bytes.TrimSpace(params["cursor"]); len(c) > 0 && !bytes.Equal(c, []byte("null")) && !bytes.Equal(c, []byte(`""`)) {
		h.fail(w, r, errorStatus(CodeInvalidParams, modern), id, "tools/list", &Error{Code: CodeInvalidParams, Message: "Invalid params: unknown cursor"})
		return
	}
	tools := h.Backend.Tools(ctx)
	if tools == nil {
		tools = []Tool{}
	}
	result := map[string]any{"tools": tools}
	if modern {
		ttl := h.ToolsTTL
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		result["ttlMs"] = int64(ttl / time.Millisecond)
		// The list depends on what the caller's credential allows.
		result["cacheScope"] = "private"
	}
	h.ok(w, r, id, "tools/list", "", result, modern)
}

func errorStatus(code int, modern bool) int {
	if !modern {
		return http.StatusOK
	}
	switch code {
	case CodeMethodNotFound:
		return http.StatusNotFound
	case CodeInvalidParams:
		return http.StatusBadRequest
	}
	return http.StatusOK
}

func (h *Handler) callTool(ctx context.Context, w http.ResponseWriter, r *http.Request, id json.RawMessage, params map[string]json.RawMessage, modern bool) {
	const method = "tools/call"
	var name string
	if json.Unmarshal(params["name"], &name) != nil || name == "" {
		h.fail(w, r, errorStatus(CodeInvalidParams, modern), id, method, &Error{Code: CodeInvalidParams, Message: `Invalid params: "name" is required`})
		return
	}
	if modern {
		mirrored := r.Header.Get("Mcp-Name")
		if mirrored == "" {
			h.fail(w, r, http.StatusBadRequest, id, method, headerMismatch("the Mcp-Name header is required for tools/call"))
			return
		}
		decoded, ok := decodeHeaderValue(mirrored)
		if !ok || decoded != name {
			h.fail(w, r, http.StatusBadRequest, id, method, headerMismatch("Mcp-Name header value does not match the tool name in the body"))
			return
		}
	}
	args := bytes.TrimSpace(params["arguments"])
	if len(args) == 0 || bytes.Equal(args, []byte("null")) {
		args = []byte("{}")
	}
	if args[0] != '{' {
		h.fail(w, r, errorStatus(CodeInvalidParams, modern), id, method, &Error{Code: CodeInvalidParams, Message: `Invalid params: "arguments" must be an object`})
		return
	}
	res, err := h.Backend.Call(ctx, name, json.RawMessage(args))
	if err != nil {
		var he *HTTPError
		var pe *Error
		switch {
		case errors.As(err, &he):
			for k, v := range he.Header {
				w.Header()[k] = v
			}
			h.fail(w, r, he.Status, id, method, he.Err)
		case errors.As(err, &pe):
			h.fail(w, r, errorStatus(pe.Code, modern), id, method, pe)
		default:
			h.fail(w, r, http.StatusOK, id, method, &Error{Code: CodeInternalError, Message: "Internal error"})
		}
		return
	}
	if res.Content == nil {
		res.Content = []Content{}
	}
	result := map[string]any{"content": res.Content, "isError": res.IsError}
	if res.StructuredContent != nil {
		result["structuredContent"] = res.StructuredContent
	}
	h.ok(w, r, id, method, name, result, modern)
}
