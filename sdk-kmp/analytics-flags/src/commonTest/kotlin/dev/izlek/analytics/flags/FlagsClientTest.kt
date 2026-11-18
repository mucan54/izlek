package dev.izlek.analytics.flags

import dev.izlek.analytics.Analytics
import dev.izlek.analytics.Config
import dev.izlek.analytics.core.AnalyticsCore
import dev.izlek.analytics.internal.Json
import dev.izlek.analytics.platform.HttpResponse
import dev.izlek.analytics.testkit.DecodedRecord
import dev.izlek.analytics.testkit.FakeHttp
import dev.izlek.analytics.testkit.NetworkDown
import dev.izlek.analytics.testkit.TestEnv
import dev.izlek.analytics.testkit.VectorJson
import kotlinx.coroutines.test.TestResult
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.runTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNull
import kotlin.test.assertTrue

/** Download, caching, refresh and exposure events (spec/flags.md §5), on fakes and virtual time. */
class FlagsClientTest {

    private class Fixture(val scope: TestScope) {
        val env = TestEnv(scope.testScheduler)
        var config = Config("pk_flags", "https://in.example.test", flushAt = 100, flushIntervalMs = 0, captureLifecycle = false, gzip = false)
        var flagsConfig = FlagsConfig()
        lateinit var core: AnalyticsCore

        /** The ruleset of spec/vectors/flags.json, served with an ETag. */
        var rulesetJson: String = Json.write(VectorJson.flags()["ruleset"])
        var etag = "\"v31\""
        var serveFlags: suspend (FakeHttp.Request) -> HttpResponse = { r ->
            if (r.headers["If-None-Match"] == etag) HttpResponse(304) else HttpResponse(200, mapOf("etag" to etag), rulesetJson.encodeToByteArray())
        }

        init {
            env.http.handler = { r -> if (r.method == "GET") serveFlags(r) else HttpResponse(200) }
        }

        fun start() {
            core = Analytics.start(config, env.deps())
            Flags.setup(flagsConfig)
            settle()
        }

        fun kill() {
            Flags.shutdown()
            Analytics.shutdown()
        }

        fun settle() = scope.testScheduler.runCurrent()

        fun advance(ms: Long) {
            scope.testScheduler.advanceTimeBy(ms)
            scope.testScheduler.runCurrent()
        }

        val gets get() = env.http.requests.filter { it.method == "GET" }

        suspend fun exposures(): List<DecodedRecord> {
            Analytics.flush()
            return env.http.batches().flatMap { it.records }.filter { it.name == "flag_exposed" }
        }
    }

    private fun flagsTest(block: suspend Fixture.() -> Unit): TestResult = runTest {
        val f = Fixture(this)
        try {
            f.block()
        } finally {
            f.kill()
        }
    }

    @Test
    fun rulesetIsDownloadedFromFlagsHostAndEvaluatedLocally() = flagsTest {
        flagsConfig = FlagsConfig(flagsHost = "https://cdn.example.test/")
        start()
        val get = gets.single()
        assertEquals("https://cdn.example.test/flags/pk_flags.json", get.url)
        assertNull(get.headers["If-None-Match"])
        assertNull(get.headers["Authorization"], "the ruleset is public; the token is only in the path")

        assertEquals(true, Flags.flag("everyone"))
        assertEquals("legacy", Flags.flag("disabled_with_default", "x"))
        assertEquals("fallback", Flags.flag("nobody", "fallback"), "null result → caller default")
        assertEquals("d", Flags.flag("no_such_flag", "d"))
        assertTrue(Flags.isEnabled("everyone"))
        assertFalse(Flags.isEnabled("nobody"))
        assertTrue(Flags.isEnabled("nobody", default = true))
        assertEquals("only", Flags.variant("short_weights"))
        assertNull(Flags.variant("everyone"), "an on/off flag has no variant")
        assertEquals("x", Flags.variant("nobody", "x"))
    }

    @Test
    fun defaultHostIsTheIngestionHostAndDefaultsApplyWithoutRuleset() = flagsTest {
        serveFlags = { throw NetworkDown() }
        start()
        assertEquals("https://in.example.test/flags/pk_flags.json", gets.first().url)
        assertEquals("fallback", Flags.flag("everyone", "fallback"))
        assertFalse(Flags.isEnabled("everyone"))
        assertTrue(exposures().isEmpty(), "no exposure without a ruleset")

        // retried sooner than the regular interval until the first ruleset arrives
        val before = gets.size
        serveFlags = { HttpResponse(200, mapOf("etag" to etag), rulesetJson.encodeToByteArray()) }
        advance(5_000)
        assertEquals(before + 1, gets.size)
        assertEquals(true, Flags.flag("everyone", "fallback"))
    }

    @Test
    fun evaluationUsesContextSuperAndPersonPropertiesAndIdentity() = flagsTest {
        env.context = linkedMapOf("platform" to "ios", "app_version" to "3.1.0")
        start()
        // rule: platform == ios && app_version >= 3.2.0, rollout 25 %, variants control/b
        assertNull(Flags.flag("new_checkout"))
        Analytics.register(mapOf("app_version" to "3.2.0")) // super properties override context
        Analytics.identify("user-7")
        settle()
        assertEquals("control", Flags.flag("new_checkout"), "vector: user-7 is in the rollout")
        Analytics.identify("user-23", mapOf("plan" to "pro"))
        settle()
        assertEquals("b", Flags.flag("new_checkout"))
        assertEquals("fallback", Flags.flag("first_match_decides"), "person property plan=pro hits the 0 % rule")
        Analytics.reset()
        settle()
        assertEquals(true, Flags.flag("first_match_decides"), "person properties are cleared by reset")
    }

    @Test
    fun exposureIsRecordedOncePerSessionAndFlag() = flagsTest {
        start()
        repeat(3) { Flags.flag("everyone") }
        Flags.isEnabled("nobody")
        Flags.variant("short_weights")
        Flags.flag("no_such_flag")
        settle()
        var seen = exposures()
        assertEquals(listOf("everyone", "nobody", "short_weights", "no_such_flag"), seen.map { it.prop("flag") })
        assertEquals(listOf("true", "false", "only", "false"), seen.map { it.prop("variant") })
        assertTrue(seen.all { it.prop("ruleset_v") == 31L })

        // same session: nothing new
        Flags.flag("everyone")
        settle()
        assertEquals(4, exposures().size)

        // new session (30 min of inactivity): exposed again
        advance(31 * 60_000)
        Analytics.capture("wake")
        settle()
        Flags.flag("everyone")
        settle()
        seen = exposures()
        assertEquals(5, seen.size)
        assertEquals("everyone", seen.last().prop("flag"))
    }

    @Test
    fun lastGoodCopyIsUsedFromDiskAndRefreshedWithIfNoneMatch() = flagsTest {
        start()
        assertEquals(1, gets.size)
        assertTrue(env.fs.files.keys.any { it.endsWith("/flags") }, "ruleset stored on disk")
        kill()

        // next start: offline. The stored ruleset works immediately.
        serveFlags = { throw NetworkDown() }
        start()
        assertEquals(true, Flags.flag("everyone"))
        assertEquals(etag, gets.last().headers["If-None-Match"])
        kill()

        // online again, unchanged on the server: 304, still working
        var notModified = 0
        serveFlags = { r ->
            if (r.headers["If-None-Match"] == etag) {
                notModified++
                HttpResponse(304)
            } else {
                HttpResponse(200, mapOf("etag" to etag), rulesetJson.encodeToByteArray())
            }
        }
        start()
        assertEquals(1, notModified)
        assertEquals(true, Flags.flag("everyone"))
    }

    @Test
    fun refreshesEveryFiveMinutesInForegroundOnly() = flagsTest {
        start()
        assertEquals(1, gets.size)
        advance(5 * 60_000 - 1)
        assertEquals(1, gets.size)
        advance(1)
        assertEquals(2, gets.size)
        assertEquals(etag, gets.last().headers["If-None-Match"])

        // a new ruleset version is picked up
        rulesetJson = """{"v":32,"flags":[{"key":"everyone","enabled":false,"salt":"s1","rules":[],"default":null}]}"""
        etag = "\"v32\""
        advance(5 * 60_000)
        assertEquals(3, gets.size)
        assertNull(Flags.flag("everyone"))

        // background: no polling; back to foreground: refresh as soon as the interval has passed
        env.onBackground!!()
        settle()
        advance(30 * 60_000)
        assertEquals(3, gets.size)
        env.onForeground!!()
        settle()
        assertEquals(4, gets.size)
    }

    @Test
    fun badDownloadsKeepTheLastGoodRuleset() = flagsTest {
        start()
        assertEquals(true, Flags.flag("everyone"))
        val answers = ArrayDeque<HttpResponse>(
            listOf(
                HttpResponse(500),
                HttpResponse(200, mapOf("etag" to "\"bad\""), "<html>captive portal</html>".encodeToByteArray()),
                HttpResponse(200, mapOf("etag" to "\"bad2\""), """{"v":40}""".encodeToByteArray()),
                HttpResponse(404),
            ),
        )
        serveFlags = { answers.removeFirst() }
        repeat(4) {
            assertFalse(Flags.refresh())
            assertEquals(true, Flags.flag("everyone"))
        }
        serveFlags = { r -> HttpResponse(if (r.headers["If-None-Match"] == etag) 304 else 500) }
        assertTrue(Flags.refresh(), "the ETag of the last good copy is still the one sent")
    }

    @Test
    fun optOutSuppressesExposureEvents() = flagsTest {
        start()
        Analytics.optOut()
        settle()
        assertEquals(true, Flags.flag("everyone"), "flags still evaluate")
        settle()
        assertTrue(exposures().isEmpty())
    }
}
