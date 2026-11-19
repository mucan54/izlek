// Package conformance tests the control plane's MCP endpoint and its OAuth
// authorization server against the official MCP TypeScript clients: real,
// version-pinned packages are pointed at an in-process control plane, connect
// with an sk_ key and through the full OAuth flow, list and call tools, and
// refresh their tokens.
//
// Two client generations are used, because they speak different protocol
// revisions:
//
//   - @modelcontextprotocol/sdk 1.x: the initialize handshake (2025-11-25
//     and, pinned by the test, 2025-06-18 and 2025-03-26), OAuth with dynamic
//     client registration.
//   - @modelcontextprotocol/client 2.x: the stateless 2026-07-28 revision
//     (server/discover probe and pinned), plus its legacy mode, OAuth with a
//     Client ID Metadata Document.
//
// The tests need Node.js and the npm packages pinned in package.json, so
// they are behind a build tag:
//
//	cd control/mcp/conformance && npm ci
//	go test -tags compat -count=1 -v ./control/mcp/conformance/
package conformance
