package dev.izlek.analytics.replay

import dev.izlek.analytics.Analytics
import dev.izlek.analytics.Config
import dev.izlek.analytics.core.AnalyticsCore
import dev.izlek.analytics.internal.Murmur3
import dev.izlek.analytics.platform.HttpResponse
import dev.izlek.analytics.testkit.FakeHttp
import dev.izlek.analytics.testkit.NetworkDown
import dev.izlek.analytics.testkit.TestEnv
import kotlinx.coroutines.test.TestResult
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.runTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNotEquals
import kotlin.test.assertNull
import kotlin.test.assertTrue

/** Scheduler, chunking, sampling and upload rules of spec/replay.md §3–§4. */
class ReplayRecorderTest {

    private class FakeSource : ViewTreeSource {
        var tree: TreeCapture? = null
        var captures = 0

        override suspend fun capture(): TreeCapture? {
            captures++
            return tree
        }
    }

    private class Upload(val request: FakeHttp.Request) {
        val query: Map<String, String> = request.url.substringAfter('?').split('&').associate {
            it.substringBefore('=') to it.substringAfter('=')
        }
        val raw: ByteArray = TestEnv.fakeGunzip(request.body!!)
        val chunk: DecodedChunk = LarDecoder.decode(raw)
    }

    private class Fixture(val scope: TestScope) {
        val env = TestEnv(scope.testScheduler)
        val source = FakeSource()
        var replayConfig = ReplayConfig()
        var replayStatus: suspend (FakeHttp.Request) -> HttpResponse = { HttpResponse(200) }
        lateinit var core: AnalyticsCore
        lateinit var recorder: ReplayRecorder

        init {
            env.http.handler = { r -> if (r.url.contains("/v1/r")) replayStatus(r) else HttpResponse(200) }
            source.tree = screen("Hoş geldin")
        }

        fun start() {
            core = Analytics.start(
                Config("pk_replay", "https://in.example.test", flushAt = 1000, flushIntervalMs = 0, captureLifecycle = false, gzip = false),
                env.deps(),
            )
            scope.testScheduler.runCurrent()
            recorder = ReplayRecorder(core, replayConfig, source)
            recorder.start()
            scope.testScheduler.runCurrent()
        }

        fun kill() {
            if (::recorder.isInitialized) recorder.stop()
            Analytics.shutdown()
        }

        fun settle() = scope.testScheduler.runCurrent()

        fun advance(ms: Long) {
            scope.testScheduler.advanceTimeBy(ms)
            scope.testScheduler.runCurrent()
        }

        val uploads get() = env.http.requests.filter { it.url.contains("/v1/r") }.map { Upload(it) }
        val session get() = core.snapshot.value!!.sessionId
        fun spool() = env.fs.names(core.dir + "/replay").filter { it.startsWith("r-") }

        fun screen(title: String, password: String = "hunter2", extra: Int = 0, width: Int = 390, height: Int = 844): TreeCapture {
            val nodes = arrayListOf(
                RawNode(1, 0, NodeType.CONTAINER, 0, 0, width, height, color = 0xFFFFFFFF.toInt()),
                RawNode(2, 1, NodeType.TEXT, 16, 60, 200, 24, text = title),
                RawNode(3, 1, NodeType.INPUT, 16, 100, 358, 44, text = password, focused = true),
                RawNode(4, 1, NodeType.BUTTON, 16, 300, 358, 48, color = 0xFF0A84FF.toInt(), text = "Devam", clickable = true),
            )
            for (i in 0 until extra) nodes.add(RawNode(100L + i, 1, NodeType.TEXT, 0, 400 + i, 300, 20, text = "row number $i of the list"))
            return TreeCapture(width, height, nodes)
        }
    }

    private fun replayTest(block: suspend Fixture.() -> Unit): TestResult = runTest {
        val f = Fixture(this)
        try {
            f.block()
        } finally {
            f.kill()
        }
    }

    @Test
    fun fullSnapshotFirstThenDeltasOnlyWhenTheTreeChanges() = replayTest {
        start()
        val t0 = env.wallNow()
        advance(500) // first capture → full snapshot opens chunk 0
        advance(500) // unchanged → nothing
        source.tree = screen("Hoş geldin, Ayşe")
        advance(500) // delta
        advance(1_000) // unchanged
        assertEquals(0, uploads.size, "chunk still open")
        advance(3_000) // 5 s after the chunk started it is closed and uploaded
        val up = uploads.single()
        assertEquals("POST", up.request.method)
        assertTrue(up.request.url.startsWith("https://in.example.test/v1/r?"))
        assertEquals("gzip", up.request.headers["Content-Encoding"])
        assertEquals("Bearer pk_replay", up.request.headers["Authorization"])
        assertEquals(session, up.query["s"])
        assertEquals("0", up.query["n"])
        assertEquals((t0 + 500).toString(), up.query["ts"])
        assertEquals("1000", up.query["d"])
        assertEquals(core.snapshot.value!!.anonId, up.query["a"])
        assertNull(up.query["u"], "no user id before identify")

        val chunk = up.chunk
        assertEquals(session, chunk.session)
        assertEquals(0, chunk.seq)
        assertEquals(t0 + 500, chunk.startTs)
        assertEquals(listOf(LarEncoder.KIND_FULL_SNAPSHOT, LarEncoder.KIND_DELTA), chunk.kinds)
        assertEquals(listOf(0L, 1_000L), chunk.frames.map { it.dt })
        val full = chunk.frames[0]
        assertEquals(listOf(390, 844, 4), listOf(full.w, full.h, full.nodes.size))
        assertEquals(listOf(ReplayNode(2, 1, NodeType.TEXT, 16, 60, 200, 24, NodeFlags.HAS_TEXT, null, "Hoş geldin, Ayşe")), chunk.frames[1].nodes)
        assertEquals(1, recorder.stats.fullSnapshots)
        assertEquals(1, recorder.stats.deltas)
        assertTrue(spool().isEmpty(), "uploaded chunks leave the spool")
    }

    @Test
    fun maskingHappensBeforeAnythingIsStoredOrSent() = replayTest {
        replayStatus = { throw NetworkDown() }
        start()
        advance(500)
        source.tree = screen("Hoş geldin", password = "correct horse")
        advance(5_000)
        val secrets = listOf("hunter2", "correct horse")
        for (bytes in env.fs.files.values + uploads.map { it.raw }) {
            val text = bytes.decodeToString()
            for (s in secrets) assertFalse(text.contains(s), "input content must never be stored or sent")
        }
        val input = uploads.first().chunk.frames[0].nodes.first { it.type == NodeType.INPUT }
        assertEquals("•••••••", input.text)
        assertEquals(NodeFlags.HAS_TEXT or NodeFlags.MASKED or NodeFlags.FOCUSED, input.flags)
    }

    @Test
    fun atMostTwoFramesPerSecond() = replayTest {
        replayConfig = ReplayConfig(frameIntervalMs = 10)
        start()
        var i = 0
        repeat(100) {
            source.tree = screen("title ${i++}")
            advance(100)
        }
        assertTrue(source.captures <= 20, "captures in 10 s: ${source.captures}")
        advance(10_000)
        val frames = uploads.flatMap { it.chunk.frames }
        assertTrue(frames.size <= 20, "frames in 10 s: ${frames.size}")
        assertTrue(frames.size >= 15)
    }

    @Test
    fun unchangedScreenProducesOnlyAFullSnapshotEverySixtySeconds() = replayTest {
        start()
        advance(3 * 60_000 + 10_000)
        val chunks = uploads.map { it.chunk }
        assertEquals(listOf(0L, 1L, 2L, 3L), chunks.map { it.seq })
        assertTrue(chunks.all { it.kinds == listOf(LarEncoder.KIND_FULL_SNAPSHOT) })
        val starts = chunks.map { it.startTs }
        assertEquals(listOf(60_000L, 60_000L, 60_000L), starts.zipWithNext().map { (a, b) -> b - a })
        assertEquals(0, recorder.stats.deltas)
    }

    @Test
    fun everyChunkStartsWithAFullSnapshotAndIsClosedAfterFiveSeconds() = replayTest {
        start()
        var i = 0
        repeat(60) { // 30 s of constant change
            source.tree = screen("title ${i++}")
            advance(500)
        }
        advance(6_000)
        val chunks = uploads.map { it.chunk }
        assertTrue(chunks.size >= 6, "chunks: ${chunks.size}")
        assertEquals(chunks.indices.map { it.toLong() }, chunks.map { it.seq }, "seq counts from 0 without gaps")
        for (c in chunks) {
            assertEquals(LarEncoder.KIND_FULL_SNAPSHOT, c.kinds.first(), "chunk ${c.seq} is playable on its own")
            assertTrue(c.durationMs < 5_000, "chunk ${c.seq} spans ${c.durationMs} ms")
            assertTrue(c.kinds.drop(1).all { it == LarEncoder.KIND_DELTA })
        }
        // the durations announced in the query match the frames
        assertEquals(chunks.map { it.durationMs.toString() }, uploads.map { it.query["d"] })
        // replaying all chunks in order ends at the last tree
        val player = Player()
        for (c in chunks) for (f in c.frames) if (f.kind == 0) player.full(f.nodes) else player.delta(f.removed, f.nodes)
        assertEquals("title ${i - 1}", player.flatten().first { it.id == 2L }.text)
    }

    @Test
    fun chunkIsClosedWhenItReachesSixtyFourKilobytes() = replayTest {
        start()
        var i = 0
        repeat(8) {
            source.tree = screen("big ${i++}", extra = 900) // ≈ 30 KB per full snapshot
            advance(500)
            source.tree = TreeCapture(390, 844, source.tree!!.nodes.reversed().sortedBy { if (it.id == 1L) 0 else 1 }) // reorder → full
            advance(500)
        }
        advance(6_000)
        val sizes = uploads.map { it.raw.size }
        assertTrue(uploads.size >= 4, "uploads: ${uploads.size}")
        assertTrue(sizes.any { it >= 64 * 1024 }, "size limit reached: $sizes")
        assertTrue(sizes.all { it < 64 * 1024 + 40_000 }, "closed right after the frame that crossed the limit: $sizes")
        assertTrue(uploads.all { it.chunk.durationMs < 5_000 })
        assertTrue(uploads.all { it.chunk.kinds.first() == LarEncoder.KIND_FULL_SNAPSHOT })
    }

    @Test
    fun screenChangeAndViewportChangeForceAFullSnapshot() = replayTest {
        start()
        recorder.screen("Login")
        advance(500)
        advance(500)
        recorder.screen("Home") // same tree, new screen
        advance(500)
        source.tree = screen("Hoş geldin", width = 844, height = 390) // rotation
        advance(500)
        advance(5_000)
        val frames = uploads.flatMap { it.chunk.frames }
        assertEquals(
            listOf(
                LarEncoder.KIND_SCREEN_CHANGE, LarEncoder.KIND_FULL_SNAPSHOT,
                LarEncoder.KIND_SCREEN_CHANGE, LarEncoder.KIND_FULL_SNAPSHOT,
                LarEncoder.KIND_VIEWPORT, LarEncoder.KIND_FULL_SNAPSHOT,
            ),
            frames.map { it.kind },
        )
        assertEquals(listOf("Login", "Home"), frames.filter { it.kind == LarEncoder.KIND_SCREEN_CHANGE }.map { it.name })
        assertEquals(listOf(844, 390), frames[4].let { listOf(it.w, it.h) })
        assertEquals(listOf(844, 390), frames[5].let { listOf(it.w, it.h) })
    }

    @Test
    fun screenNameFromTheSourceIsUsedWhenTheAppDoesNotNameScreens() = replayTest {
        source.tree = TreeCapture(390, 844, screen("a").nodes, screen = "MainActivity")
        start()
        advance(500)
        source.tree = TreeCapture(390, 844, screen("a").nodes, screen = "DetailActivity")
        advance(500)
        advance(5_000)
        assertEquals(listOf("MainActivity", "DetailActivity"), uploads.flatMap { it.chunk.frames }.filter { it.kind == 4 }.map { it.name })
    }

    @Test
    fun touchAndScrollFrames() = replayTest {
        start()
        recorder.touch(LarEncoder.TOUCH_DOWN, 10, 10)
        settle()
        assertEquals(0, recorder.stats.fullSnapshots, "input before the first snapshot is ignored")
        advance(500)
        advance(240)
        recorder.touch(LarEncoder.TOUCH_DOWN, 195, 324)
        advance(16)
        recorder.touch(LarEncoder.TOUCH_MOVE, 196, 320)
        advance(16)
        recorder.touch(LarEncoder.TOUCH_MOVE, 197, 318) // throttled: closer than 50 ms to the previous move
        advance(44)
        recorder.touch(LarEncoder.TOUCH_UP, 196, 320)
        advance(100)
        recorder.scroll(6, 0, -128)
        advance(5_000)
        val first = uploads.first().chunk
        assertEquals(listOf(0, 2, 2, 2, 3), first.kinds)
        assertEquals(listOf(0L, 240L, 16L, 60L, 100L), first.frames.map { it.dt })
        assertEquals(listOf(0, 1, 2), first.frames.subList(1, 4).map { it.phase })
        assertEquals(listOf(195, 324), first.frames[1].let { listOf(it.x, it.y) })
        assertEquals(listOf(6L, 0L, -128L), first.frames[4].let { listOf(it.nodeId, it.dx.toLong(), it.dy.toLong()) })

        // a touch long after the last chunk was closed starts a new chunk with the known tree
        advance(20_000)
        recorder.touch(LarEncoder.TOUCH_DOWN, 1, 2)
        advance(6_000)
        assertEquals(listOf(0, 2), uploads.last().chunk.kinds)
        assertEquals(1, uploads.last().chunk.seq)
    }

    @Test
    fun sessionSamplingFollowsTheSpecFormula() = replayTest {
        replayConfig = ReplayConfig(sampleRate = 0.5)
        start()
        var recorded = 0
        var skipped = 0
        repeat(24) {
            val before = uploads.size
            val expected = Murmur3.bucket(session) < 5_000
            source.tree = screen("session $it")
            advance(6_000)
            val got = uploads.drop(before)
            assertEquals(expected, got.isNotEmpty(), "session $session")
            assertTrue(got.all { u -> u.query["s"] == session })
            if (expected) recorded++ else skipped++
            Analytics.reset() // new anon id and a new session
            settle()
        }
        assertTrue(recorded > 0 && skipped > 0, "recorded=$recorded skipped=$skipped")
    }

    @Test
    fun sampleRateZeroRecordsNothing() = replayTest {
        replayConfig = ReplayConfig(sampleRate = 0.0)
        start()
        advance(30_000)
        assertEquals(0, source.captures, "the UI is not even walked")
        assertEquals(0, uploads.size)
    }

    @Test
    fun newSessionRestartsTheSequenceAndIdentityTravelsInTheQuery() = replayTest {
        start()
        Analytics.identify("user 42/ö")
        settle()
        advance(500)
        advance(5_000)
        val first = uploads.single()
        assertEquals("user%2042%2F%C3%B6", first.query["u"])
        val s1 = session
        Analytics.reset()
        settle()
        assertNotEquals(s1, session)
        source.tree = screen("after reset")
        advance(500)
        advance(5_000)
        val second = uploads.last()
        assertEquals(session, second.query["s"])
        assertEquals("0", second.query["n"])
        assertNull(second.query["u"])
        assertNotEquals(first.query["a"], second.query["a"])
        assertEquals(LarEncoder.KIND_FULL_SNAPSHOT, second.chunk.kinds.first())
    }

    @Test
    fun failedUploadsStayInTheSpoolAndAreRetriedAlsoAfterARestart() = replayTest {
        replayStatus = { HttpResponse(503) }
        start()
        advance(500)
        advance(5_000)
        assertEquals(1, uploads.size)
        assertEquals(1, spool().size)
        advance(2_000)
        assertEquals(2, uploads.size, "retried with backoff")
        val s = session
        kill()

        replayStatus = { HttpResponse(200) }
        advance(60_000)
        start() // same session (restart within 30 min): the spooled chunk goes out first
        assertEquals(s, session)
        assertEquals(listOf("0"), uploads.drop(2).map { it.query["n"] })
        assertEquals(uploads[0].raw.toList(), uploads[2].raw.toList(), "same (s, n), same bytes")
        assertTrue(spool().isEmpty())

        source.tree = screen("after restart")
        advance(500)
        advance(5_000)
        assertEquals("1", uploads.last().query["n"], "chunk numbers are not reused within a session")
        assertEquals(s, uploads.last().query["s"])
    }

    @Test
    fun forbiddenDisablesReplayAndClearsTheSpool() = replayTest {
        replayStatus = { HttpResponse(403) }
        start()
        advance(500)
        advance(5_000)
        assertEquals(1, uploads.size)
        assertTrue(spool().isEmpty())
        val captures = source.captures
        source.tree = screen("more")
        advance(60_000)
        assertEquals(1, uploads.size)
        assertEquals(captures, source.captures, "no more captures after a 403")
    }

    @Test
    fun spoolIsBoundedAndBadChunksAreDropped() = replayTest {
        replayConfig = ReplayConfig(maxSpoolBytes = 600)
        replayStatus = { throw NetworkDown() }
        start()
        var i = 0
        repeat(40) {
            source.tree = screen("title ${i++}")
            advance(500)
        }
        advance(6_000)
        val bytes = spool().sumOf { env.fs.files[core.dir + "/replay/" + it]!!.size }
        assertTrue(bytes <= 600, "spool bytes: $bytes")
        assertTrue(recorder.stats.chunksDropped > 0)

        replayStatus = { HttpResponse(400) }
        advance(AnalyticsCore.BACKOFF_MAX_MS)
        assertTrue(spool().isEmpty(), "400 drops the chunk instead of retrying forever")
    }

    @Test
    fun backgroundClosesTheChunkAndPausesCapture() = replayTest {
        start()
        advance(500)
        env.onBackground!!()
        settle()
        advance(500)
        assertEquals(1, uploads.size, "the open chunk is closed and sent when the app goes to the background")
        val captures = source.captures
        advance(60_000)
        assertEquals(captures, source.captures)
        env.onForeground!!()
        settle()
        source.tree = screen("back")
        advance(500)
        advance(5_000)
        assertEquals(2, uploads.size)
        assertEquals(LarEncoder.KIND_FULL_SNAPSHOT, uploads.last().chunk.kinds.first())
    }

    @Test
    fun optOutStopsRecordingAndDeletesSpooledChunks() = replayTest {
        replayStatus = { throw NetworkDown() }
        start()
        advance(500)
        advance(5_000)
        assertEquals(1, spool().size)
        Analytics.optOut()
        settle()
        advance(1_000)
        assertTrue(spool().isEmpty())
        val attempts = uploads.size
        source.tree = screen("ignored")
        advance(60_000)
        assertEquals(attempts, uploads.size)
        assertTrue(spool().isEmpty())
    }

    @Test
    fun aFailingSourceDoesNotBreakTheSdk() = replayTest {
        start()
        val failing = object : ViewTreeSource {
            override suspend fun capture(): TreeCapture? = throw IllegalStateException("view hierarchy changed")
        }
        val second = ReplayRecorder(core, ReplayConfig(), failing)
        second.start()
        advance(10_000)
        second.stop()
        Analytics.capture("still works")
        Analytics.flush()
        assertEquals(0, core.stats.internalErrors)
        assertTrue(env.http.batches().any { b -> b.names.contains("still works") })
    }
}
