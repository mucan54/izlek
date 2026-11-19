// SDK install snippets for the settings page, filled with the selected write
// token and the ingestion host. Kept as plain template strings so they stay
// easy to compare with the SDK READMEs. Only the human-readable comments come
// from the locale (read when a snippet is built, so they follow the language);
// event names and identifiers are sample data and are not translated.
import { S } from '../i18n'

export interface SnippetInput {
  token: string
  host: string
  /** project replay sample rate, 0..1 */
  sampleRate: number
  replay: boolean
}

const q = (s: string) => s.replace(/\\/g, '\\\\').replace(/'/g, "\\'")
const dq = (s: string) => s.replace(/\\/g, '\\\\').replace(/"/g, '\\"')

export const webNpm = ({ token, host, sampleRate, replay }: SnippetInput): string =>
  `import { init, capture, identify } from '@izlek/web'
import { initFlags, flag } from '@izlek/web/flags'
${replay ? "import { startReplay } from '@izlek/web/replay'\n" : ''}
init({ token: '${q(token)}', host: '${q(host)}' })
initFlags()
${replay ? `startReplay({ sampleRate: ${sampleRate} })\n` : ''}
capture('sepete_eklendi', { urun_id: 42, fiyat: 129.9 })
identify('kullanici-17', { plan: 'pro' })

if (flag('new_checkout') === 'b') {
  // …
}
`

export const webScript = ({ token, host, sampleRate, replay }: SnippetInput): string =>
  `<script src="/js/izlek.min.js"></script>
<script src="/js/izlek-flags.min.js"></script>
${replay ? '<script src="/js/izlek-replay.min.js"></script>\n' : ''}<script src="/js/analytics-init.js"></script>

// analytics-init.js
izlek.init({ token: '${q(token)}', host: '${q(host)}' })
izlekFlags.initFlags()
${replay ? `izlekReplay.startReplay({ sampleRate: ${sampleRate} })\n` : ''}`

export const android = ({ token, host }: SnippetInput): string =>
  `// build.gradle.kts
dependencies {
    implementation("dev.izlek:analytics-core:0.1.0")
    implementation("dev.izlek:analytics-flags:0.1.0")   // ${S.sdk.commentOptional}
    implementation("dev.izlek:analytics-replay:0.1.0")  // ${S.sdk.commentOptional}
}

// Application.onCreate()
import dev.izlek.analytics.Analytics
import dev.izlek.analytics.Config
import dev.izlek.analytics.PlatformContext

Analytics.setup(
    Config(token = "${dq(token)}", host = "${dq(host)}"),
    PlatformContext(applicationContext),
)

Analytics.capture("sepete_eklendi", mapOf("urun_id" to 42, "fiyat" to 129.9))
Analytics.identify("kullanici-17", mapOf("plan" to "pro"))
`

export const ios = ({ token, host }: SnippetInput): string =>
  `// AppDelegate / App.init — ${S.sdk.commentKmp} (IzlekAnalytics.xcframework)
import IzlekAnalytics

Analytics.shared.setup(
    config: Config(token: "${dq(token)}", host: "${dq(host)}"),
    context: PlatformContext()
)

Analytics.shared.capture(event: "sepete_eklendi", props: ["urun_id": 42, "fiyat": 129.9])
Analytics.shared.identify(userId: "kullanici-17", personProps: ["plan": "pro"])
`
