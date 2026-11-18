// Package compat holds the PostHog SDK compatibility tests of design §17.2:
// real, version-pinned PostHog SDKs (posthog-node, and posthog-js in headless
// Chromium) are pointed at an in-process ingestion node, and the test asserts
// that the SDK reports no errors, that its queue drains, and that the
// expected format=2 entries reach the WAL.
//
// The tests need Node.js, the npm packages pinned in package.json and (for
// posthog-js) Playwright with Chromium, so they are behind a build tag:
//
//	cd ingest/compat && npm ci
//	go test -tags compat -count=1 -v ./ingest/compat/
package compat
