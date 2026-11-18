package dev.izlek.analytics

import dev.izlek.analytics.core.AnalyticsCore
import dev.izlek.analytics.internal.Json
import dev.izlek.analytics.internal.Wire
import dev.izlek.analytics.platform.HttpResponse
import dev.izlek.analytics.testkit.NetworkDown
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNotEquals
import kotlin.test.assertNull
import kotlin.test.assertTrue

/** Identity, session, lifecycle events and privacy (design §4.6 – §4.9). */
class IdentitySessionTest {

    @Test
    fun identifyIsASegmentBoundary() = sdkTest {
        start()
        capture("before")
        Analytics.identify("user-42", mapOf("plan" to "pro", "age" to 34))
        capture("after")
        Analytics.flush()

        val (anonymous, identified) = batches()
        assertEquals(listOf("before"), anonymous.names)
        assertEquals("", anonymous.userId)
        assertEquals("user-42", identified.userId)
        assertEquals(anonymous.anonId, identified.anonId, "both ids travel together after identify")
        assertEquals(anonymous.sessionIdHex, identified.sessionIdHex)

        val (identify, after) = identified.records
        assertEquals(Wire.KIND_IDENTIFY, identify.kind)
        assertEquals("", identify.name)
        assertEquals("user-42", identify.prop("user_id"))
        assertEquals("pro", identify.prop("plan"))
        assertEquals(34L, identify.prop("age"))
        assertEquals("after", after.name)

        val snap = core.snapshot.value!!
        assertEquals("user-42", snap.userId)
        assertEquals("pro", snap.props["plan"])
    }

    @Test
    fun setPersonPropsWritesASetPersonRecord() = sdkTest {
        start()
        Analytics.setPersonProps(mapOf("beta" to true, "score" to 0.75))
        Analytics.flush()
        val r = records().single()
        assertEquals(Wire.KIND_SET_PERSON, r.kind)
        assertEquals("", r.name)
        assertEquals(true, r.prop("beta"))
        assertEquals(0.75, r.prop("score"))
        assertEquals(true, core.snapshot.value!!.props["beta"])
    }

    @Test
    fun resetSendsOldSegmentWithOldIdentityThenStartsFresh() = sdkTest {
        // Offline on purpose: the header must carry the identity the records were captured
        // with even when the batch is sent long after reset().
        env.http.handler = { throw NetworkDown() }
        start()
        Analytics.identify("user-1")
        capture("as-user-1")
        val before = core.snapshot.value!!
        Analytics.reset()
        capture("anonymous-again")
        val after = core.snapshot.value!!
        assertNotEquals(before.anonId, after.anonId)
        assertEquals("", after.userId)
        assertNotEquals(before.sessionId, after.sessionId)
        assertTrue(pendingFiles().isNotEmpty(), "reset closes the open segment immediately")

        env.http.handler = { HttpResponse(200) }
        advance(AnalyticsCore.BACKOFF_MAX_MS)
        Analytics.flush()
        val delivered = batches().filter { it.records.isNotEmpty() }.distinctBy { it.batchIdHex }
        val old = delivered.first { it.userId == "user-1" }
        assertEquals(before.anonId, old.anonId)
        assertEquals(listOf(Wire.KIND_IDENTIFY, Wire.KIND_EVENT, Wire.KIND_RESET), old.records.map { it.kind })
        val fresh = delivered.last()
        assertEquals(listOf("anonymous-again"), fresh.names)
        assertEquals(after.anonId, fresh.anonId)
        assertEquals("", fresh.userId)
        // every batch holds records of exactly one identity
        assertTrue(delivered.none { b -> b.userId == "" && b.records.any { it.name == "as-user-1" } })
    }

    @Test
    fun identifyingAsAnotherUserImpliesReset() = sdkTest {
        start()
        Analytics.identify("user-1")
        capture("one")
        val first = core.snapshot.value!!.anonId
        Analytics.identify("user-2")
        capture("two")
        Analytics.flush()
        val byUser = batches().groupBy { it.userId }
        assertEquals(setOf("user-1", "user-2"), byUser.keys)
        assertEquals(Wire.KIND_RESET, byUser["user-1"]!!.flatMap { it.records }.last().kind)
        assertEquals(first, byUser["user-1"]!!.first().anonId)
        assertNotEquals(first, byUser["user-2"]!!.first().anonId)
        assertEquals(listOf("", "two"), byUser["user-2"]!!.flatMap { it.names })
    }

    @Test
    fun invalidUserIdsAreIgnored() = sdkTest {
        start()
        Analytics.identify("")
        Analytics.identify("x".repeat(201))
        capture("a")
        Analytics.flush()
        assertEquals("", batches().single().userId)
        assertEquals(listOf("a"), names())
    }

    @Test
    fun superPropertiesAreAddedToEveryEventAndPersisted() = sdkTest {
        start()
        Analytics.register(mapOf("plan" to "pro", "n" to 1))
        capture("a", mapOf("n" to 2))
        Analytics.identify("u")
        Analytics.unregister("plan")
        capture("b")
        Analytics.flush()
        val a = records().first { it.name == "a" }
        assertEquals("pro", a.prop("plan"))
        assertEquals(2L, a.prop("n"), "the event's own value wins")
        assertEquals(1, a.props.count { it.key == "n" })
        val identify = records().first { it.kind == Wire.KIND_IDENTIFY }
        assertFalse(identify.has("plan"), "super properties are not person properties")
        val b = records().first { it.name == "b" }
        assertFalse(b.has("plan"))
        assertEquals(1L, b.prop("n"))

        restart()
        capture("c")
        Analytics.flush()
        assertEquals(1L, records().first { it.name == "c" }.prop("n"))
    }

    @Test
    fun screenEvent() = sdkTest {
        start()
        Analytics.screen("Checkout", mapOf("step" to 2, "screen" to "ignored"))
        Analytics.flush()
        val r = records().single()
        assertEquals("screen", r.name)
        assertEquals("Checkout", r.prop("screen"))
        assertEquals(1, r.props.count { it.key == "screen" })
        assertEquals(2L, r.prop("step"))
    }

    // --- session ----------------------------------------------------------------------------------

    @Test
    fun sessionRollsOverAfterThirtyMinutesOfInactivity() = sdkTest {
        start()
        capture("a")
        advance(29 * 60_000)
        capture("b")
        advance(30 * 60_000 + 1)
        capture("c")
        Analytics.flush()
        val (first, second) = batches()
        assertEquals(listOf("a", "b"), first.names)
        assertEquals(listOf("c"), second.names, "a new session is a segment boundary")
        assertNotEquals(first.sessionIdHex, second.sessionIdHex)
        assertEquals(first.anonId, second.anonId)
        assertEquals(7, (second.sessionId[6].toInt() and 0xF0) shr 4)
    }

    @Test
    fun sessionTimeoutIsCheckedWhenReturningFromBackground() = sdkTest {
        start()
        capture("a")
        val s1 = core.snapshot.value!!.sessionId
        env.onBackground!!()
        advance(10 * 60_000)
        env.onForeground!!()
        settle()
        assertEquals(s1, core.snapshot.value!!.sessionId)
        env.onBackground!!()
        advance(31 * 60_000)
        env.onForeground!!()
        settle()
        assertNotEquals(s1, core.snapshot.value!!.sessionId)
    }

    @Test
    fun sessionSurvivesAQuickRestartButNotALongOne() = sdkTest {
        start()
        capture("a")
        val s1 = core.snapshot.value!!.sessionId
        advance(5 * 60_000)
        restart()
        assertEquals(s1, core.snapshot.value!!.sessionId)
        kill()
        advance(31 * 60_000)
        start()
        assertNotEquals(s1, core.snapshot.value!!.sessionId)
    }

    // --- lifecycle events (design §4.8) ---------------------------------------------------------

    @Test
    fun lifecycleEvents() = sdkTest {
        config = config.copy(captureLifecycle = true)
        start()
        Analytics.flush()
        assertEquals(listOf("app_installed", "app_opened"), names())
        assertEquals("1.0.0", records()[0].prop("version"))
        assertEquals(false, records()[1].prop("from_background"))

        env.http.requests.clear()
        restart()
        Analytics.flush()
        assertEquals(listOf("app_opened"), names(), "no app_installed on the second start")

        env.http.requests.clear()
        kill()
        env.context = linkedMapOf("platform" to "test", "app_version" to "1.1.0", "app_build" to "2")
        start()
        Analytics.flush()
        assertEquals(listOf("app_updated", "app_opened"), names())
        assertEquals("1.0.0", records()[0].prop("previous_version"))
        assertEquals("1", records()[0].prop("previous_build"))
        assertEquals("1.1.0", batches()[0].context("app_version"))

        env.http.requests.clear()
        env.onForeground!!() // duplicate foreground notification right after start: ignored
        env.onBackground!!()
        settle()
        assertEquals(listOf("app_backgrounded"), names(), "background flushes right away")
        env.onForeground!!()
        settle()
        Analytics.flush()
        assertEquals(listOf("app_backgrounded", "app_opened"), names())
        assertEquals(true, records().last().prop("from_background"))
    }

    @Test
    fun startInBackgroundDoesNotReportAppOpened() = sdkTest {
        config = config.copy(captureLifecycle = true, flushIntervalMs = 30_000)
        env.initialForeground = false
        start()
        Analytics.flush()
        assertEquals(listOf("app_installed"), names())
        capture("push_received")
        advance(120_000)
        assertEquals(1, requests.size, "no flush timer in the background")
        env.onForeground!!()
        settle()
        Analytics.flush()
        assertEquals(listOf("app_installed", "push_received", "app_opened"), names())
        assertEquals(false, records().last().prop("from_background"))
    }

    // --- privacy (design §4.9) -------------------------------------------------------------------

    @Test
    fun optOutWipesTheQueueAndPersists() = sdkTest {
        config = config.copy(flushAt = 2, captureLifecycle = true)
        env.http.handler = { throw NetworkDown() }
        start()
        capture("a")
        capture("b")
        capture("c")
        assertTrue(pendingFiles().isNotEmpty())
        val attempts = requests.size
        Analytics.optOut()
        settle()
        assertEquals(listOf("state"), files(), "queue deleted")
        assertTrue(core.snapshot.value!!.optedOut)
        assertEquals(true, (Json.parse(file("state")!!.decodeToString()) as Map<*, *>)["opt_out"])

        env.http.handler = { HttpResponse(200) }
        capture("ignored")
        Analytics.identify("nobody")
        Analytics.flush()
        advance(3_600_000)
        assertEquals(listOf("state"), files())
        assertEquals(attempts, requests.size)

        restart()
        capture("still-ignored")
        Analytics.flush()
        assertEquals(attempts, requests.size, "opt-out survives restarts; no lifecycle events either")
        assertEquals("", core.snapshot.value!!.userId)

        Analytics.optIn()
        capture("back")
        Analytics.flush()
        assertEquals(listOf("back"), batches().drop(attempts).flatMap { it.names })
        assertNull(batches().last().context("dropped_count"))
    }

    @Test
    fun optOutWhileARequestIsInFlight() = sdkTest {
        config = config.copy(flushAt = 1)
        env.http.handler = {
            kotlinx.coroutines.delay(1_000)
            HttpResponse(500)
        }
        start()
        capture("a")
        Analytics.optOut()
        settle()
        advance(60_000)
        assertEquals(1, requests.size)
        assertEquals(listOf("state"), files())
        assertEquals(0, core.stats.internalErrors)
    }

    @Test
    fun callsBeforeSetupAndOddValuesNeverThrow() = sdkTest {
        Analytics.capture("too-early")
        Analytics.identify("x")
        Analytics.reset()
        Analytics.flush()
        start()
        val cyclic = ArrayList<Any?>()
        cyclic.add(cyclic)
        Analytics.capture("odd", mapOf("cyclic" to cyclic, "nan" to Double.NaN, "obj" to object {}, "nested" to mapOf(1 to listOf(null))))
        Analytics.setPersonProps(mapOf("cyclic" to cyclic))
        Analytics.register(mapOf("s" to sequenceOf(1, 2)))
        Analytics.flush()
        assertEquals(0, core.stats.internalErrors)
        assertEquals(0, core.stats.callerErrors)
        val r = records().first { it.name == "odd" }
        assertTrue((r.prop("nan") as Double).isNaN())
        assertEquals("""{"1":[null]}""", r.prop("nested"))
    }
}
