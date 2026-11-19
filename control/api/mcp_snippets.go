package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
)

// Setup snippets for the get_sdk_setup tool. They follow the SDK READMEs
// (sdk-web/README.md, sdk-kmp/README.md), the README's "Sending events"
// section and the snippets of the web UI (ui/src/lib/snippets.ts); when one
// of those changes, this file has to follow.

// placeholderIngestHost stands in for the ingestion address when
// LA_PUBLIC_INGEST_URL is not set. The .invalid top-level domain can never
// resolve, so a snippet pasted unchanged fails visibly instead of sending
// events somewhere else.
const placeholderIngestHost = "https://YOUR-INGEST-HOST.invalid"

var sdkPlatforms = []string{"web", "web_module", "android", "ios", "jvm", "posthog_js", "posthog_node", "http"}

type setupStep struct {
	Title    string `json:"title"`
	Language string `json:"language"`
	Code     string `json:"code"`
	Note     string `json:"note,omitempty"`
}

// Quoting for the two string-literal styles the snippets use. Tokens are
// base62 and the host comes from configuration, so this is a second line of
// defence, not the first.
func sq(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", "", "\r", "").Replace(s)
}

func dq(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", "", "\r", "").Replace(s)
}

func trimFloat(f float64) string {
	raw, _ := json.Marshal(f)
	return string(raw)
}

func sdkSteps(platform, token, host string, replay bool, sampleRate float64) (steps []setupStep, notes []string) {
	const notPublishedWeb = "The @izlek/web package is not published to npm yet. Build it from the izlek repository (cd sdk-web && npm install && npm run build) and use the files in sdk-web/dist."
	const notPublishedKMP = "The Kotlin Multiplatform SDK (dev.izlek:analytics-*) is not published to Maven Central yet. Build it from the izlek repository (sdk-kmp) and publish it to a local or internal Maven repository, or include the modules in the application's Gradle build."
	rate := trimFloat(sampleRate)

	switch platform {
	case "web":
		scripts := "<script src=\"/js/izlek.min.js\"></script>\n<script src=\"/js/izlek-flags.min.js\"></script>   <!-- optional: feature flags -->\n"
		initCode := fmt.Sprintf("<script>\n  izlek.init({ token: '%s', host: '%s' })\n  izlekFlags.initFlags()\n", sq(token), sq(host))
		if replay {
			scripts += "<script src=\"/js/izlek-replay.min.js\"></script>  <!-- optional: session replay -->\n"
			initCode += fmt.Sprintf("  izlekReplay.startReplay({ sampleRate: %s })\n", rate)
		}
		initCode += "\n  izlek.capture('added_to_cart', { sku: 'bag-01', price: 59 })\n  izlek.identify('user-1017', { plan: 'pro' })   // after sign-in\n</script>"
		steps = []setupStep{
			{Title: "Build the SDK and host its files", Language: "sh", Code: "cd sdk-web && npm install && npm run build\n# then copy dist/izlek.min.js, dist/izlek-flags.min.js and dist/izlek-replay.min.js to your site, e.g. /js/", Note: notPublishedWeb},
			{Title: "Load and initialise it on every page (order matters: the core first)", Language: "html", Code: scripts + initCode},
			{Title: "Optional: track clicks on marked elements", Language: "html", Code: "<button data-track=\"checkout_started\" data-track-plan=\"pro\">Checkout</button>\n<!-- sends capture('checkout_started', { plan: 'pro' }) -->"},
		}
		notes = append(notes, "pageview and pageleave events are sent automatically, including on single-page navigation.",
			"The SDK does nothing during server-side rendering and does not start for known bot user agents.",
			"If consent is required, call init() only after consent was given: nothing is stored or sent before init().")
		if !replay {
			notes = append(notes, "This write token does not accept session replay uploads. To record sessions, enable replay on the token (update_write_token) and add the replay script.")
		}
	case "web_module":
		imports := "import { init, capture, identify } from '@izlek/web'\nimport { initFlags, flag } from '@izlek/web/flags'\n"
		body := fmt.Sprintf("\ninit({ token: '%s', host: '%s' })\ninitFlags()\n", sq(token), sq(host))
		if replay {
			imports += "import { startReplay } from '@izlek/web/replay'\n"
			body += fmt.Sprintf("startReplay({ sampleRate: %s })\n", rate)
		}
		body += "\ncapture('added_to_cart', { sku: 'bag-01', price: 59 })\nidentify('user-1017', { plan: 'pro' })   // after sign-in\n\nif (flag('new_checkout') === 'b') {\n  // variant b\n}"
		steps = []setupStep{
			{Title: "Build the SDK and add it to the project", Language: "sh", Code: "cd sdk-web && npm install && npm run build\n# then, in your application:\nnpm install /path/to/izlek/sdk-web", Note: notPublishedWeb},
			{Title: "Initialise it once, in the browser entry point", Language: "ts", Code: imports + body},
		}
		notes = append(notes, "pageview and pageleave events are sent automatically, including on single-page navigation.",
			"The SDK does nothing during server-side rendering; call init() in code that runs in the browser.")
		if !replay {
			notes = append(notes, "This write token does not accept session replay uploads. To record sessions, enable replay on the token (update_write_token) and call startReplay().")
		}
	case "android":
		steps = []setupStep{
			{Title: "Add the dependencies (build.gradle.kts)", Language: "kotlin", Code: "dependencies {\n    implementation(\"dev.izlek:analytics-core:0.1.0\")\n    implementation(\"dev.izlek:analytics-flags:0.1.0\")   // optional\n    implementation(\"dev.izlek:analytics-replay:0.1.0\")  // optional\n}", Note: notPublishedKMP},
			{Title: "Initialise in Application.onCreate()", Language: "kotlin", Code: fmt.Sprintf("import dev.izlek.analytics.Analytics\nimport dev.izlek.analytics.Config\nimport dev.izlek.analytics.PlatformContext\n\nAnalytics.setup(\n    Config(token = \"%s\", host = \"%s\"),\n    PlatformContext(applicationContext),\n)\n\nAnalytics.capture(\"purchase\", mapOf(\"amount\" to 12.5, \"items\" to listOf(1, 2)))\nAnalytics.screen(\"Checkout\")\nAnalytics.identify(\"user-1017\", mapOf(\"plan\" to \"pro\"))   // after sign-in", dq(token), dq(host))},
		}
		notes = append(notes, "The Android source set compiles, but it has not been run on a device or emulator by the izlek project. Test the integration on a device before relying on it.",
			"The SDK's manifest adds the INTERNET and ACCESS_NETWORK_STATE permissions.")
	case "ios":
		steps = []setupStep{
			{Title: "Build the framework", Language: "sh", Code: "# on macOS, in sdk-kmp (see sdk-kmp/README.md and sample-ios/)\n./gradlew build -Pizlek.ios=true", Note: "No XCFramework is published or packaged yet; the iOS target has only been cross-compiled, never linked or run, by the izlek project."},
			{Title: "Initialise at application start (Swift, on the main thread)", Language: "swift", Code: fmt.Sprintf("import IzlekAnalytics\n\nAnalytics.shared.setup(\n    config: Config(token: \"%s\", host: \"%s\"),\n    context: PlatformContext()\n)\n\nAnalytics.shared.capture(event: \"purchase\", props: [\"amount\": 12.5])\nAnalytics.shared.identify(userId: \"user-1017\", personProps: [\"plan\": \"pro\"])   // after sign-in", dq(token), dq(host)), Note: "The exact Swift signatures depend on how the Kotlin framework is exported; check them against the generated header."},
		}
		notes = append(notes, "Treat the iOS SDK as experimental: it has not been run on a device or simulator by the izlek project.",
			"Add PrivacyInfo.xcprivacy from analytics-core/src/iosMain/resources to the framework bundle.")
	case "jvm":
		steps = []setupStep{
			{Title: "Add the dependencies (build.gradle.kts)", Language: "kotlin", Code: "dependencies {\n    implementation(\"dev.izlek:analytics-core:0.1.0\")\n    implementation(\"dev.izlek:analytics-flags:0.1.0\")   // optional\n}", Note: notPublishedKMP},
			{Title: "Initialise at start-up", Language: "kotlin", Code: fmt.Sprintf("import dev.izlek.analytics.Analytics\nimport dev.izlek.analytics.Config\nimport dev.izlek.analytics.PlatformContext\n\nAnalytics.setup(\n    Config(token = \"%s\", host = \"%s\"),\n    PlatformContext(storageDir = \"/var/lib/myapp/analytics\", appVersion = \"1.0\"),\n)\n\nAnalytics.capture(\"job_finished\", mapOf(\"duration_ms\" to 5400))\nAnalytics.identify(\"user-1017\", mapOf(\"plan\" to \"pro\"))", dq(token), dq(host))},
		}
		notes = append(notes, "The JVM target is the one the izlek project has compiled and tested end to end.",
			"storageDir holds the on-disk event queue; it must be writable and should survive restarts.")
	case "posthog_js":
		steps = []setupStep{
			{Title: "Point an existing posthog-js at izlek", Language: "js", Code: fmt.Sprintf("posthog.init('%s', { api_host: '%s' })\n\nposthog.capture('added_to_cart', { sku: 'bag-01', price: 59 })\nposthog.identify('user-1017', { plan: 'pro' })", sq(token), sq(host))},
		}
		notes = append(notes, "Verified by the izlek project against posthog-js 1.451.1. Events, $identify, $set, $set_once and $unset are mapped.",
			"PostHog feature flags, surveys and session recording are not served by izlek: the flag endpoints answer with an empty set. Use the izlek SDK for izlek's flags and replay.")
	case "posthog_node":
		steps = []setupStep{
			{Title: "Install a supported posthog-node version", Language: "sh", Code: "npm install posthog-node@5"},
			{Title: "Point it at izlek", Language: "js", Code: fmt.Sprintf("import { PostHog } from 'posthog-node'\n\nconst client = new PostHog('%s', { host: '%s' })\n\nclient.capture({ distinctId: 'user-1017', event: 'purchase', properties: { amount: 59 } })\nawait client.shutdown()   // flush before the process exits", sq(token), sq(host))},
		}
		notes = append(notes, "Verified by the izlek project against posthog-node 5.74.0. posthog-node 6 and later use a different capture endpoint and are NOT supported.",
			"The write token goes where PostHog expects its project API key. Do not use an sk_ secret key here.")
	case "http":
		steps = []setupStep{
			{Title: "Send a batch with curl", Language: "sh", Code: fmt.Sprintf("curl -sS -H 'Content-Type: application/json' '%s/batch/' -d '{\n  \"api_key\": \"%s\",\n  \"batch\": [\n    { \"event\": \"signup\", \"distinct_id\": \"user-1017\", \"properties\": { \"method\": \"email\" } },\n    { \"event\": \"purchase\", \"distinct_id\": \"user-1017\", \"properties\": { \"amount\": 59.0 },\n      \"timestamp\": \"2026-11-18T09:30:00Z\" }\n  ]\n}'", sq(host), dq(token))},
		}
		notes = append(notes, "This is the PostHog-compatible JSON endpoint; it answers 200 {\"status\": 1} once the batch is stored durably.",
			"Status codes: 200 stored; 400 malformed (do not retry); 401/403 unknown or disabled token, or origin not allowed; 413 split the batch; 429/503 slow down and honour Retry-After.",
			"\"timestamp\" is optional (RFC 3339); without it the time of receipt is used. \"distinct_id\" identifies the person.",
			"The native endpoint is POST /v1/e with the binary LA1 format (spec/wire.md in the repository); use an SDK for that.")
	}
	return steps, notes
}

func (s *Server) sdkSetupTool(tc *toolCall, args json.RawMessage) (*toolResult, error) {
	var in struct {
		projectArg
		Platform string `json:"platform"`
		Token    string `json:"token"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return nil, err
	}
	platform := in.Platform
	if platform == "" {
		platform = "web"
	}
	known := false
	for _, p := range sdkPlatforms {
		known = known || p == platform
	}
	if !known {
		return nil, failf("Unknown platform %q. Use one of: %s.", clipASCII(platform, 40), strings.Join(sdkPlatforms, ", "))
	}
	pid, err := tc.projectID(in.ProjectID)
	if err != nil {
		return nil, err
	}
	params := map[string]string{"pid": pid}
	rawProject, err := tc.invoke(s.getProject, http.MethodGet, params, nil)
	if err != nil {
		return nil, err
	}
	project := rawProject.(db.Project)
	rawTokens, err := tc.invoke(s.listTokens, http.MethodGet, params, nil)
	if err != nil {
		return nil, err
	}
	var chosen *db.Token
	for i, t := range rawTokens.([]db.Token) {
		if in.Token != "" {
			if t.Token == in.Token {
				chosen = &rawTokens.([]db.Token)[i]
			}
		} else if t.Status == "active" && chosen == nil {
			chosen = &rawTokens.([]db.Token)[i]
		}
	}
	notes := []string{}
	switch {
	case chosen == nil && in.Token != "":
		if !auth.LooksLikeWriteToken(in.Token) {
			return nil, failf("\"token\" must be a write token (pk_ followed by 32 characters). Secret keys (sk_…) must never be put in client code.")
		}
		return nil, failf("That write token does not belong to this project. See list_write_tokens.")
	case chosen == nil:
		return nil, failf("The project has no active write token. Create one with create_write_token (or enable one with update_write_token) first.")
	case chosen.Status != "active":
		notes = append(notes, "WARNING: this write token is disabled, so everything sent with it is rejected. Enable it with update_write_token.")
	}
	host, placeholder := s.cfg.PublicIngestURL, false
	if host == "" {
		host, placeholder = placeholderIngestHost, true
		notes = append(notes, "IMPORTANT: this server does not know the public address of its ingestion service (LA_PUBLIC_INGEST_URL is not set), so the snippets contain the placeholder "+placeholderIngestHost+". Replace it with the real address of the la-ingest service — not the address of this control plane — before using the code.")
	}
	steps, more := sdkSteps(platform, chosen.Token, host, chosen.Replay, project.ReplaySampleRate)
	notes = append(notes, more...)
	if len(chosen.AllowedOrigins) > 0 && (platform == "web" || platform == "web_module" || platform == "posthog_js") {
		notes = append(notes, "This token only accepts events from these browser origins: "+strings.Join(chosen.AllowedOrigins, ", ")+". Pages served from any other origin are rejected.")
	}
	notes = append(notes, "The write token is a public identifier and is meant to be embedded in client code.")
	next := "After the application has run with this code, call verify_installation to confirm that events arrive (allow about a minute)."

	data := map[string]any{
		"platform": platform, "token": chosen.Token, "ingest_host": host, "ingest_host_is_placeholder": placeholder,
		"steps": steps, "notes": notes, "next": next,
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Setup for platform %q, project %d (%s), write token %s, ingestion host %s\n", platform, project.ID, project.Name, chosen.Token, host)
	for i, st := range steps {
		fmt.Fprintf(&b, "\n%d. %s\n\n```%s\n%s\n```\n", i+1, st.Title, st.Language, st.Code)
		if st.Note != "" {
			fmt.Fprintf(&b, "\nNote: %s\n", st.Note)
		}
	}
	b.WriteString("\nNotes:\n")
	for _, n := range notes {
		b.WriteString("- " + n + "\n")
	}
	b.WriteString("\n" + next + "\n")
	return &toolResult{Data: data, Text: b.String()}, nil
}
