package dev.izlek.analytics

import dev.izlek.analytics.core.AnalyticsCore
import dev.izlek.analytics.core.Segment
import dev.izlek.analytics.internal.Wire
import dev.izlek.analytics.internal.toHex
import dev.izlek.analytics.platform.HttpResponse
import dev.izlek.analytics.testkit.DecodedBatch
import dev.izlek.analytics.testkit.NetworkDown
import dev.izlek.analytics.testkit.WireDecoder
import kotlinx.coroutines.delay
import kotlin.test.Test
import kotlin.test.assertContentEquals
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNotEquals
import kotlin.test.assertNull
import kotlin.test.assertTrue

/** Queue + sender behaviour against a fake server (design §3.5, §4.3, §4.4, §17.8). */
class QueueSenderTest {

    private fun DecodedBatch.uuids() = records.map { it.uuid.toHex() }

    // --- triggers -------------------------------------------------------------------------------

    @Test
    fun recordsGoToActiveFileAndAreSentWhenFlushAtIsReached() = sdkTest {
        config = config.copy(flushAt = 3)
        start()
        capture("a", mapOf("n" to 1))
        capture("b")
        assertEquals(0, requests.size)
        assertEquals(listOf("active", "state"), files())

        capture("c")
        assertEquals(1, requests.size)
        val req = requests[0]
        assertEquals("POST", req.method)
        assertEquals("https://in.example.test/v1/e", req.url)
        assertEquals("Bearer pk_test", req.headers["Authorization"])
        assertEquals("application/octet-stream", req.headers["Content-Type"])
        assertNull(req.headers["Content-Encoding"])
        assertTrue(WireDecoder.frameCheck(req.body!!).ok)

        val batch = req.batch()
        assertEquals(listOf("a", "b", "c"), batch.names)
        assertEquals(1L, batch.records[0].prop("n"))
        assertEquals(Analytics.SDK_NAME, batch.sdk)
        assertEquals(core.snapshot.value!!.anonId, batch.anonId)
        assertEquals(36, batch.anonId.length)
        assertEquals("", batch.userId)
        assertEquals(core.snapshot.value!!.sessionId, batch.sessionIdHex)
        assertEquals("test", batch.context("platform"))
        assertEquals("1.0.0", batch.context("app_version"))
        assertNull(batch.context("sdk"), "sdk travels in the header field only")
        assertNull(batch.context("dropped_count"))
        assertEquals(env.wallNow(), batch.sentAt)
        // 200 → file deleted
        assertEquals(listOf("state"), files())
        assertEquals(1, env.backgroundTasks.size)
        assertEquals(0, env.openBackgroundTasks)
    }

    @Test
    fun recordsCarryUuidV7AndCaptureTime() = sdkTest {
        config = config.copy(flushAt = 2)
        start()
        val t0 = env.wallNow()
        capture("a")
        advance(1500)
        capture("b")
        val (a, b) = batches().single().records
        assertEquals(t0, a.ts)
        assertEquals(t0 + 1500, b.ts)
        for (r in listOf(a, b)) {
            assertEquals(7, (r.uuid[6].toInt() and 0xF0) shr 4, "version")
            assertEquals(2, (r.uuid[8].toInt() and 0xC0) shr 6, "variant")
        }
        assertTrue(a.uuid.toHex() < b.uuid.toHex(), "uuids are time ordered")
    }

    @Test
    fun flushTimerRunsOnlyInForeground() = sdkTest {
        config = config.copy(flushIntervalMs = 30_000)
        start()
        capture("a")
        advance(29_999)
        assertEquals(0, requests.size)
        advance(1)
        assertEquals(listOf("a"), names())

        // going to the background flushes immediately ...
        capture("b")
        env.onBackground!!()
        settle()
        assertEquals(listOf("a", "b"), names())
        // ... and stops the timer
        capture("c")
        advance(10 * 60_000)
        assertEquals(2, requests.size)
        assertEquals(listOf("active", "state"), files())

        env.onForeground!!()
        settle()
        advance(30_000)
        assertEquals(listOf("a", "b", "c"), names())
    }

    @Test
    fun manualFlushSendsAndReturns() = sdkTest {
        start()
        Analytics.capture("a")
        Analytics.flush()
        assertEquals(listOf("a"), names())
        assertEquals(listOf("state"), files())

        // flush with nothing queued and flush while the network is down both return
        Analytics.flush()
        env.http.handler = { throw NetworkDown() }
        Analytics.capture("b")
        Analytics.flush()
        assertEquals(2, requests.size)
        assertEquals(1, pendingFiles().size)
    }

    @Test
    fun pendingFilesAreSentAtStartup() = sdkTest {
        start()
        capture("a")
        capture("b")
        assertEquals(0, requests.size)
        val anon = core.snapshot.value!!.anonId
        restart()
        // the left-over active segment was closed and sent by the new "process"
        assertEquals(listOf("a", "b"), names())
        assertEquals(anon, batches().single().anonId)
        assertEquals(anon, core.snapshot.value!!.anonId, "anon_id is persisted")
        assertEquals(listOf("state"), files())
    }

    @Test
    fun oneRequestAtATimeOldestFirst() = sdkTest {
        config = config.copy(flushAt = 2)
        var concurrent = 0
        var maxConcurrent = 0
        env.http.handler = {
            concurrent++
            maxConcurrent = maxOf(maxConcurrent, concurrent)
            delay(500)
            concurrent--
            HttpResponse(200)
        }
        start()
        captureMany("e", 7)
        Analytics.flush()
        assertEquals(1, maxConcurrent)
        assertEquals((0 until 7).map { "e$it" }, names())
        assertEquals(0, pendingFiles().size)
    }

    @Test
    fun segmentsNeverExceedFiveHundredRecords() = sdkTest {
        config = config.copy(flushAt = 10_000)
        env.http.handler = { throw NetworkDown() }
        start()
        captureMany("e", 1001)
        env.http.handler = { HttpResponse(200) }
        advance(AnalyticsCore.BACKOFF_MAX_MS)
        Analytics.flush()
        val sizes = batches().filter { it.records.isNotEmpty() }.map { it.records.size }
        assertTrue(sizes.all { it <= Wire.MAX_BATCH_RECORDS }, "$sizes")
        assertEquals(1001, records().map { it.uuid.toHex() }.toSet().size)
    }

    // --- persistence and corruption -----------------------------------------------------------

    @Test
    fun tornTailOfActiveFileIsTruncatedAtStartup() = sdkTest {
        start()
        capture("a")
        capture("b")
        kill()
        val path = "$dir/active"
        val good = env.fs.files[path]!!
        val scan = Segment.scan(good)!!
        // a half-written third record: a copy of the last frame without its final bytes
        val lastFrame = good.copyOfRange(scan.boundaries[1], scan.boundaries[2])
        env.fs.files[path] = good + lastFrame.copyOf(lastFrame.size - 4)

        start()
        assertEquals(listOf("a", "b"), names())
        assertTrue(WireDecoder.frameCheck(requests.single().plainBody()).ok)
        assertTrue(batches().single().badRecords.isEmpty())
    }

    @Test
    fun tornTailOfPendingFileIsTruncatedAtStartup() = sdkTest {
        config = config.copy(flushAt = 2)
        env.http.handler = { throw NetworkDown() }
        start()
        capture("a")
        capture("b")
        val name = pendingFiles().single()
        kill()
        env.fs.files["$dir/$name"] = env.fs.files["$dir/$name"]!! + byteArrayOf(0x30, 0x00, 0x01, 0x02)
        env.http.handler = { HttpResponse(200) }
        start()
        assertEquals(listOf("a", "b"), requests.last().batch().names)
        assertTrue(WireDecoder.frameCheck(requests.last().plainBody()).ok)
        assertEquals(listOf("state"), files())
    }

    @Test
    fun garbageAndLeftoverFilesAreCleanedAtStartup() = sdkTest {
        config = config.copy(flushAt = 2)
        env.http.handler = { throw NetworkDown() }
        start()
        capture("a")
        capture("b")
        val parent = pendingFiles().single()
        kill()
        // an interrupted 413 split (children written, parent not yet deleted), a stale temp file,
        // an unreadable segment and a zero-record segment
        val bytes = env.fs.files["$dir/$parent"]!!
        env.fs.files["$dir/$parent-0"] = bytes
        env.fs.files["$dir/$parent-1"] = bytes
        env.fs.files["$dir/state.tmp"] = byteArrayOf(1, 2, 3)
        env.fs.files["$dir/pending-000000000099-00000000000000000000000000000000"] = byteArrayOf(0x7f, 1, 2)
        env.fs.files["$dir/active"] = bytes.copyOf(Segment.scan(bytes)!!.recordsOffset)
        env.http.handler = { HttpResponse(200) }
        val before = requests.size
        start()
        // exactly one delivery: the parent; the half-written children were discarded
        assertEquals(listOf(listOf("a", "b")), batches().drop(before).map { it.names })
        assertEquals(listOf("state"), files())
    }

    // --- retry contract (design §3.5) ----------------------------------------------------------

    @Test
    fun retriesCarryTheSameBatchIdAndRecords() = sdkTest {
        config = config.copy(flushAt = 2)
        env.http.enqueue(NetworkDown(), 500, HttpResponse(503), 502)
        start()
        capture("a")
        capture("b")
        advance(60_000)
        assertEquals(5, requests.size)
        val batches = batches()
        assertEquals(1, batches.map { it.batchIdHex }.toSet().size, "same batch_id on every attempt")
        assertEquals(1, batches.map { it.uuids() }.toSet().size)
        assertEquals(listOf("a", "b"), batches.last().names)
        // the id is the UUIDv7 embedded in the pending file name
        assertEquals(7, (batches[0].batchId[6].toInt() and 0xF0) shr 4)
        assertTrue(batches.zipWithNext().all { (x, y) -> y.sentAt > x.sentAt }, "sent_at is the attempt time")
        assertEquals(listOf("state"), files())
    }

    @Test
    fun batchIdSurvivesProcessRestart() = sdkTest {
        config = config.copy(flushAt = 2)
        env.http.handler = { throw NetworkDown() }
        start()
        capture("a")
        capture("b")
        val file = pendingFiles().single()
        restart()
        env.http.handler = { HttpResponse(200) }
        restart()
        val ids = batches().map { it.batchIdHex }.toSet()
        assertEquals(1, ids.size)
        assertTrue(file.endsWith(ids.single()), "batch_id is derived from the file name")
    }

    @Test
    fun backoffDoublesFromOneSecondToFiveMinutes() = sdkTest {
        env.fixedRandom = 0 // jitter factor 0 → delay = base / 2
        env.http.handler = { throw NetworkDown() }
        start()
        capture("a")
        Analytics.flush()
        advance(30 * 60_000)
        val gaps = requests.zipWithNext().map { (x, y) -> y.atMs - x.atMs }
        assertEquals(
            listOf<Long>(500, 1_000, 2_000, 4_000, 8_000, 16_000, 32_000, 64_000, 128_000, 150_000, 150_000),
            gaps.take(11),
        )
        assertTrue(gaps.drop(11).all { it == 150_000L })
    }

    @Test
    fun backoffJitterStaysWithinHalfToFullBase() = sdkTest {
        env.http.handler = { throw NetworkDown() }
        start()
        capture("a")
        Analytics.flush()
        advance(60 * 60_000)
        val gaps = requests.zipWithNext().map { (x, y) -> y.atMs - x.atMs }
        assertTrue(gaps.size > 12)
        var base = AnalyticsCore.BACKOFF_MIN_MS
        for (g in gaps) {
            assertTrue(g in base / 2..base, "gap $g for base $base")
            base = minOf(base * 2, AnalyticsCore.BACKOFF_MAX_MS)
        }
        assertTrue(gaps.toSet().size > 6, "delays are jittered")
    }

    @Test
    fun backoffResetsAfterSuccess() = sdkTest {
        env.fixedRandom = 0
        env.http.enqueue(500, 500, 500, 200, 500)
        start()
        capture("a")
        Analytics.flush()
        advance(10_000)
        assertEquals(4, requests.size)
        capture("b")
        Analytics.flush()
        val t = requests.last().atMs
        advance(10_000)
        assertEquals(6, requests.size)
        assertEquals(500, requests.last().atMs - t, "first retry delay again")
    }

    @Test
    fun status400DropsTheBatchAndReportsDroppedCount() = sdkTest {
        config = config.copy(flushAt = 2)
        env.http.enqueue(400)
        start()
        capture("a")
        capture("b")
        assertEquals(1, requests.size)
        assertEquals(listOf("state"), files(), "batch dropped, no retry")
        advance(3_600_000)
        assertEquals(1, requests.size)

        capture("c")
        capture("d")
        val second = requests[1].batch()
        assertEquals(listOf("c", "d"), second.names)
        assertEquals(2L, second.context("dropped_count"))

        capture("e")
        capture("f")
        assertNull(requests[2].batch().context("dropped_count"), "reported once")
        assertEquals(2, core.stats.droppedRecords)
    }

    @Test
    fun droppedCountIsReportedAgainUntilABatchSucceeds() = sdkTest {
        config = config.copy(flushAt = 1)
        env.http.enqueue(400, 500, 200, 200)
        start()
        capture("a") // dropped (400)
        capture("b") // 500 then 200
        advance(5_000)
        capture("c")
        val counts = batches().map { it.context("dropped_count") }
        assertEquals(listOf(null, 1L, 1L, null), counts)
    }

    @Test
    fun status401And403DropTheBatchAndPauseSendingForOneHour() = sdkTest {
        for (status in listOf(401, 403)) {
            config = config.copy(flushAt = 2)
            env.http.requests.clear()
            env.http.enqueue(status)
            start()
            capture("a")
            capture("b")
            assertEquals(1, requests.size)
            assertEquals(0, pendingFiles().size, "$status drops the batch")

            capture("c")
            capture("d")
            Analytics.flush()
            advance(AnalyticsCore.AUTH_PAUSE_MS - 2)
            assertEquals(1, requests.size, "no request during the pause after $status")
            assertEquals(listOf("active", "state"), files(), "records wait on disk")
            advance(2)
            assertEquals(2, requests.size)
            val batch = requests[1].batch()
            assertEquals(listOf("c", "d"), batch.names)
            assertEquals(2L, batch.context("dropped_count"))
            kill()
            env.fs.files.clear()
        }
    }

    @Test
    fun status413SplitsThePendingFileAtARecordBoundary() = sdkTest {
        config = config.copy(flushAt = 5)
        val accepted = ArrayList<DecodedBatch>()
        var failChildOnce = true
        env.http.handler = { r ->
            val b = r.batch()
            when {
                b.records.size > 2 -> HttpResponse(413)
                failChildOnce -> {
                    failChildOnce = false
                    HttpResponse(500)
                }
                else -> {
                    accepted.add(b)
                    HttpResponse(200)
                }
            }
        }
        start()
        captureMany("e", 5)
        // first attempt: all five records → 413 → two children
        assertEquals(5, requests[0].batch().records.size)
        advance(5_000)
        assertEquals(listOf("e0", "e1", "e2", "e3", "e4"), accepted.flatMap { it.names }, "everything delivered once, in order")
        assertTrue(accepted.all { it.records.size <= 2 })
        assertEquals(accepted.size, accepted.map { it.batchIdHex }.toSet().size, "halves have distinct batch ids")
        assertFalse(requests[0].batch().batchIdHex in accepted.map { it.batchIdHex })
        // the child that got a 500 was retried with the same id
        val all = batches()
        val failed = all.first { it.records.size <= 2 }
        assertEquals(2, all.count { it.batchIdHex == failed.batchIdHex })
        assertTrue(all.all { WireDecoder.frameCheck(requests[all.indexOf(it)].plainBody()).ok })
        assertEquals(listOf("state"), files())
        assertEquals(0, core.stats.droppedRecords)
    }

    @Test
    fun status413ForASingleRecordDropsIt() = sdkTest {
        config = config.copy(flushAt = 1)
        env.http.enqueue(413)
        start()
        capture("big")
        assertEquals(listOf("state"), files())
        capture("next")
        assertEquals(1L, requests[1].batch().context("dropped_count"))
    }

    @Test
    fun status429WaitsForRetryAfter() = sdkTest {
        config = config.copy(flushAt = 1)
        env.http.enqueue(HttpResponse(429, mapOf("retry-after" to "7")), HttpResponse(429, mapOf("retry-after" to "120")))
        start()
        capture("a")
        advance(6_999)
        assertEquals(1, requests.size)
        advance(1)
        assertEquals(2, requests.size)
        advance(119_999)
        assertEquals(2, requests.size)
        advance(1)
        assertEquals(3, requests.size)
        assertEquals(1, batches().map { it.batchIdHex }.toSet().size)
        assertEquals(listOf("state"), files())
    }

    @Test
    fun status503HonoursRetryAfterWhenLongerThanBackoff() = sdkTest {
        config = config.copy(flushAt = 1)
        env.http.enqueue(HttpResponse(503, mapOf("retry-after" to "30")))
        start()
        capture("a")
        advance(29_999)
        assertEquals(1, requests.size)
        advance(1)
        assertEquals(2, requests.size)
    }

    @Test
    fun unexpectedStatusCodesAreRetried() = sdkTest {
        config = config.copy(flushAt = 1)
        env.http.enqueue(404, 302, 408, 204)
        start()
        capture("a")
        advance(60_000)
        assertEquals(5, requests.size)
        assertEquals(listOf("state"), files())
    }

    // --- limits (design §4.3) --------------------------------------------------------------------

    @Test
    fun queueSizeLimitEvictsOldestPendingAndReportsDroppedCount() = sdkTest {
        config = config.copy(flushAt = 5, maxQueueBytes = 40_000)
        env.http.handler = { throw NetworkDown() }
        start()
        val total = 2_000
        captureMany("event_number_", total)
        assertTrue(env.fs.files.filterKeys { it.startsWith("$dir/pending-") || it == "$dir/active" }.values.sumOf { it.size } <= 40_000)
        assertTrue(pendingFiles().size >= 3, "eviction works on whole segments: ${pendingFiles().size}")
        val dropped = core.stats.droppedRecords
        assertTrue(dropped > 0)
        assertQueueFilesWellFormed()

        val failedAttempts = requests.size
        env.http.handler = { HttpResponse(200) }
        advance(AnalyticsCore.BACKOFF_MAX_MS)
        Analytics.flush()
        val ok = batches().drop(failedAttempts)
        assertEquals(dropped, ok.first().context("dropped_count"))
        assertTrue(ok.drop(1).all { it.context("dropped_count") == null })
        val delivered = ok.flatMap { it.names }
        assertEquals(total.toLong(), delivered.size + dropped)
        // the survivors are the newest events, still in order
        assertEquals((total - delivered.size until total).map { "event_number_$it" }, delivered)
    }

    @Test
    fun pendingFilesOlderThanMaxEventAgeAreDropped() = sdkTest {
        config = config.copy(flushAt = 2, maxEventAgeMs = 60_000)
        env.http.handler = { throw NetworkDown() }
        start()
        capture("old1")
        capture("old2")
        assertEquals(1, pendingFiles().size)
        advance(61_000)
        val failedAttempts = requests.size
        env.http.handler = { HttpResponse(200) }
        capture("new1")
        capture("new2")
        advance(AnalyticsCore.BACKOFF_MAX_MS)
        val delivered = batches().drop(failedAttempts)
        assertEquals(listOf(listOf("new1", "new2")), delivered.map { it.names }, "the expired segment is never sent again")
        assertEquals(2L, delivered.single().context("dropped_count"))
        assertEquals(listOf("state"), files())
    }

    @Test
    fun oversizedRecordsAndNamesAreDroppedNotSent() = sdkTest {
        config = config.copy(flushAt = 1)
        start()
        capture("n".repeat(129))
        val props = LinkedHashMap<String, Any?>()
        for (i in 0 until 20) props["k$i"] = "v".repeat(8_000)
        capture("huge", props) // > 64 KiB once encoded
        assertEquals(0, requests.size)
        capture("ç".repeat(64)) // exactly 128 bytes
        assertEquals(listOf("ç".repeat(64)), names())
        assertEquals(2L, batches().single().context("dropped_count"))
    }

    // --- failure injection (design §17.5) ------------------------------------------------------

    @Test
    fun processKilledMidSendResendsTheSameBatch() = sdkTest {
        config = config.copy(flushAt = 2)
        val received = ArrayList<DecodedBatch>()
        env.http.handler = { r ->
            received.add(r.batch()) // the server got (and stored) the batch ...
            delay(Long.MAX_VALUE / 2) // ... but the process dies before the response arrives
            HttpResponse(200)
        }
        start()
        capture("a")
        capture("b")
        assertEquals(1, received.size)
        assertEquals(1, pendingFiles().size, "file is kept until a 200 is seen")
        kill()

        env.http.handler = { r ->
            received.add(r.batch())
            HttpResponse(200)
        }
        start()
        assertEquals(2, received.size)
        assertEquals(received[0].batchIdHex, received[1].batchIdHex, "server-side idempotency key matches")
        assertEquals(received[0].uuids(), received[1].uuids())
        assertEquals(listOf("state"), files())
        advance(3_600_000)
        assertEquals(2, received.size)
    }

    @Test
    fun diskFullNeverCrashesOrCorruptsTheQueue() = sdkTest {
        start()
        capture("before1")
        capture("before2")
        // the disk fills up: 10 bytes left, so the next record is written partially
        env.fs.capacityBytes = env.fs.usedBytes() + 10
        captureMany("lost", 5)
        Analytics.identify("user-1")
        Analytics.register(mapOf("k" to "v"))
        settle()
        assertEquals(0, core.stats.internalErrors)
        assertEquals(0, core.stats.callerErrors)
        assertTrue(core.stats.droppedRecords >= 5)
        val dropped = core.stats.droppedRecords

        // space is available again
        env.fs.capacityBytes = Long.MAX_VALUE
        capture("after1")
        capture("after2")
        assertQueueFilesWellFormed()
        Analytics.flush()
        val names = records().filter { it.kind == Wire.KIND_EVENT }.map { it.name }
        assertEquals(listOf("before1", "before2", "after1", "after2"), names)
        assertTrue(batches().all { it.badRecords.isEmpty() })
        assertTrue(requests.all { WireDecoder.frameCheck(it.plainBody()).ok })
        assertEquals(dropped, batches().first().context("dropped_count"))
        assertEquals("v", records().last().prop("k"), "state changes made while the disk was full are kept in memory")
    }

    @Test
    fun tornAppendIsRepairedBeforeTheNextRecord() = sdkTest {
        start()
        capture("a")
        env.fs.failWrites = true
        env.fs.tornAppendBytes = 7
        capture("torn")
        val torn = file("active")!!
        assertEquals(7, torn.size - Segment.scan(torn)!!.validLength, "partial record on disk")
        env.fs.failWrites = false
        capture("b")
        assertQueueFilesWellFormed()
        Analytics.flush()
        assertEquals(listOf("a", "b"), names())
    }

    @Test
    fun startupOnAnUnwritableDiskDoesNotCrash() = sdkTest {
        env.fs.failWrites = true
        config = config.copy(captureLifecycle = true)
        start()
        capture("a")
        Analytics.flush()
        assertEquals(0, core.stats.internalErrors)
        assertEquals(0, requests.size)
        val anon = core.snapshot.value!!.anonId
        env.fs.failWrites = false
        capture("b")
        Analytics.flush()
        assertEquals(listOf("b"), names())
        assertEquals(anon, batches().single().anonId)
        assertTrue(file("state")!!.decodeToString().contains(anon), "state is written once the disk works")
    }

    @Test
    fun renameFailureKeepsRecordsForLater() = sdkTest {
        config = config.copy(flushAt = 2)
        start()
        env.fs.failRenames = true
        capture("a")
        capture("b")
        capture("c")
        assertEquals(0, requests.size)
        env.fs.failRenames = false
        capture("d")
        assertEquals(listOf("a", "b", "c", "d"), names())
    }

    @Test
    fun wallClockJumpingForwardAndBackward() = sdkTest {
        config = config.copy(flushAt = 2)
        env.http.handler = { throw NetworkDown() }
        start()
        val session = core.snapshot.value!!.sessionId
        capture("a")
        capture("b")
        assertEquals(1, requests.size)
        val firstAttempt = requests[0].atMs

        // the user sets the clock two days back while a retry is pending
        env.jumpClock(-2L * 24 * 3600 * 1000)
        advance(499)
        assertEquals(1, requests.size)
        advance(501)
        assertEquals(2, requests.size, "retry timing follows the monotonic clock, not the wall clock")
        assertTrue(requests[1].atMs - firstAttempt <= 1_000)
        capture("c")
        assertEquals(session, core.snapshot.value!!.sessionId, "a backward jump does not end the session")
        assertEquals(1, pendingFiles().size, "files from the 'future' are kept")

        // ... and then three days forward
        env.jumpClock(3L * 24 * 3600 * 1000)
        capture("d")
        assertNotEquals(session, core.snapshot.value!!.sessionId, "a forward jump beyond 30 min starts a new session")
        assertEquals(0, core.stats.droppedRecords, "nothing is older than maxEventAgeMs")

        env.http.handler = { HttpResponse(200) }
        advance(AnalyticsCore.BACKOFF_MAX_MS)
        Analytics.flush()
        assertEquals(listOf("a", "b", "c", "d"), batches().flatMap { it.names }.distinct())
        val ts = records().associate { it.name to it.ts }
        assertTrue(ts["c"]!! < ts["a"]!!, "record timestamps are the device clock as it was")
        assertTrue(ts["d"]!! > ts["a"]!!)
        // uuids stay unique even though the clock moved backwards
        assertEquals(4, records().map { it.uuid.toHex() }.toSet().size)
        assertEquals(0, core.stats.internalErrors)
        assertEquals(listOf("state"), files())
    }

    @Test
    fun clockJumpBeyondMaxEventAgeEvictsPendingFiles() = sdkTest {
        config = config.copy(flushAt = 1)
        env.http.handler = { throw NetworkDown() }
        start()
        capture("a")
        env.jumpClock(8L * 24 * 3600 * 1000)
        env.http.handler = { HttpResponse(200) }
        capture("b")
        advance(AnalyticsCore.BACKOFF_MAX_MS)
        // Documented behaviour: age is judged by the device wall clock.
        assertEquals(listOf("b"), batches().last().names)
        assertEquals(1L, batches().last().context("dropped_count"))
    }

    @Test
    fun gzipIsAppliedWhenConfigured() = sdkTest {
        config = config.copy(flushAt = 1, gzip = true)
        start()
        capture("a")
        val req = requests.single()
        assertEquals("gzip", req.headers["Content-Encoding"])
        assertContentEquals(byteArrayOf(0x1f, 0x8b.toByte()), req.body!!.copyOf(2))
        assertEquals(listOf("a"), req.batch().names)
    }
}
