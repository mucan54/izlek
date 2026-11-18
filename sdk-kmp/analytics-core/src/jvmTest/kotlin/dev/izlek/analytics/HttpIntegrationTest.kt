package dev.izlek.analytics

import com.sun.net.httpserver.HttpExchange
import com.sun.net.httpserver.HttpServer
import dev.izlek.analytics.internal.Wire
import dev.izlek.analytics.platform.httpGet
import dev.izlek.analytics.platform.httpPost
import dev.izlek.analytics.testkit.DecodedBatch
import dev.izlek.analytics.testkit.WireDecoder
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import java.io.ByteArrayInputStream
import java.net.InetSocketAddress
import java.nio.file.Files
import java.util.concurrent.ConcurrentLinkedQueue
import java.util.concurrent.CopyOnWriteArrayList
import java.util.zip.GZIPInputStream
import kotlin.test.AfterTest
import kotlin.test.BeforeTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertNull
import kotlin.test.assertTrue

/**
 * End to end on the JVM: the public API, the real file system, the real `HttpURLConnection`
 * transport and real gzip against an HTTP server running inside the test.
 */
class HttpIntegrationTest {
    private class Received(val method: String, val path: String, val query: String?, val headers: Map<String, String>, val body: ByteArray) {
        fun batch(): DecodedBatch {
            val raw = if (headers["content-encoding"] == "gzip") GZIPInputStream(ByteArrayInputStream(body)).readBytes() else body
            assertTrue(WireDecoder.frameCheck(raw).ok, "server-side frame check")
            return WireDecoder.decode(raw)
        }
    }

    private lateinit var server: HttpServer
    private val received = CopyOnWriteArrayList<Received>()
    private val statuses = ConcurrentLinkedQueue<Int>()
    private val host get() = "http://127.0.0.1:${server.address.port}"

    @BeforeTest
    fun startServer() {
        server = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/v1/e") { ex ->
            record(ex)
            val status = statuses.poll() ?: 200
            if (status == 429) ex.responseHeaders.add("Retry-After", "1")
            ex.sendResponseHeaders(status, -1)
            ex.close()
        }
        server.createContext("/flags/") { ex ->
            record(ex)
            if (ex.requestHeaders.getFirst("If-None-Match") == "\"v31\"") {
                ex.sendResponseHeaders(304, -1)
            } else {
                val body = """{"v":31,"flags":[]}""".toByteArray()
                ex.responseHeaders.add("ETag", "\"v31\"")
                ex.sendResponseHeaders(200, body.size.toLong())
                ex.responseBody.write(body)
            }
            ex.close()
        }
        server.start()
    }

    private fun record(ex: HttpExchange) {
        val headers = ex.requestHeaders.entries.associate { it.key.lowercase() to it.value.joinToString(",") }
        received.add(Received(ex.requestMethod, ex.requestURI.path, ex.requestURI.query, headers, ex.requestBody.readBytes()))
    }

    @AfterTest
    fun stop() {
        Analytics.shutdown()
        server.stop(0)
    }

    private fun tempDir(): String = Files.createTempDirectory("izlek-it").toString()

    @Test
    fun eventsArriveThroughTheRealTransport() = runBlocking {
        Analytics.setup(
            Config(token = "pk_integration", host = host, flushAt = 100),
            PlatformContext(storageDir = tempDir(), appVersion = "9.9.9", appBuild = "99"),
        )
        Analytics.register(mapOf("plan" to "pro"))
        Analytics.capture("purchase", mapOf("amount" to 12.5, "items" to listOf(1, 2, 3), "currency" to "TRY", "ok" to true, "none" to null))
        Analytics.screen("Home")
        Analytics.identify("user-42", mapOf("age" to 34))
        Analytics.capture("after_identify", mapOf("text" to "çğıöşü İ 🚀"))
        withTimeout(20_000) { Analytics.flush() }

        assertTrue(received.all { it.method == "POST" && it.path == "/v1/e" })
        assertTrue(received.all { it.headers["authorization"] == "Bearer pk_integration" })
        assertTrue(received.all { it.headers["content-encoding"] == "gzip" })
        assertTrue(received.all { it.headers["content-type"] == "application/octet-stream" })
        val batches = received.map { it.batch() }
        assertEquals(2, batches.size, "identify is a segment boundary")
        val (anonymous, identified) = batches
        assertEquals(listOf("app_installed", "app_opened", "purchase", "screen"), anonymous.names)
        assertEquals("", anonymous.userId)
        assertEquals("user-42", identified.userId)
        assertEquals(anonymous.anonId, identified.anonId)
        assertEquals("kmp/0.1.0", anonymous.sdk)
        assertEquals("jvm", anonymous.context("platform"))
        assertEquals("9.9.9", anonymous.context("app_version"))
        assertEquals("99", anonymous.context("app_build"))
        assertTrue(anonymous.context("locale") is String)
        assertTrue(kotlin.math.abs(anonymous.sentAt - System.currentTimeMillis()) < 60_000)

        val purchase = anonymous.records[2]
        assertEquals(12.5, purchase.prop("amount"))
        assertEquals("[1,2,3]", purchase.prop("items"))
        assertEquals(Wire.TAG_JSON, purchase.props.first { it.key == "items" }.tag)
        assertEquals("TRY", purchase.prop("currency"))
        assertEquals(true, purchase.prop("ok"))
        assertTrue(purchase.has("none"))
        assertNull(purchase.prop("none"))
        assertEquals("pro", purchase.prop("plan"))
        assertEquals("Home", anonymous.records[3].prop("screen"))

        assertEquals(Wire.KIND_IDENTIFY, identified.records[0].kind)
        assertEquals("user-42", identified.records[0].prop("user_id"))
        assertEquals("çğıöşü İ 🚀", identified.records[1].prop("text"))
        assertTrue(batches.all { it.badRecords.isEmpty() })
    }

    @Test
    fun serverErrorsAreRetriedWithTheSameBatchAndRestartsResend() = runBlocking {
        val dir = tempDir()
        val config = Config(token = "pk_retry", host = host, flushAt = 2, captureLifecycle = false)
        statuses.addAll(listOf(503, 429))
        Analytics.setup(config, PlatformContext(storageDir = dir))
        Analytics.capture("a")
        Analytics.capture("b")
        withTimeout(20_000) {
            Analytics.flush()
            while (received.size < 3) delay(50) // 503 → backoff (≤ 1 s) → 429 → Retry-After 1 s → 200
        }
        val batches = received.map { it.batch() }
        assertEquals(1, batches.map { it.batchIdHex }.toSet().size)
        assertEquals(listOf("a", "b"), batches.last().names)

        // unsent data survives the process: stop without flushing, start again
        Analytics.capture("c")
        withTimeout(20_000) { while (Analytics.coreOrNull()!!.snapshot.value == null) delay(10) }
        delay(300) // let the single-threaded dispatcher write the record
        Analytics.shutdown()
        received.clear()
        Analytics.setup(config, PlatformContext(storageDir = dir))
        withTimeout(20_000) {
            Analytics.flush()
            while (received.isEmpty()) delay(50)
        }
        assertEquals(listOf("c"), received.single().batch().names)
    }

    @Test
    fun unreachableServerDoesNotThrowAndKeepsTheData() = runBlocking {
        val dir = tempDir()
        val config = Config(token = "pk_down", host = "http://127.0.0.1:1", flushAt = 1, captureLifecycle = false)
        Analytics.setup(config, PlatformContext(storageDir = dir))
        Analytics.capture("kept")
        withTimeout(30_000) { Analytics.flush() }
        val core = Analytics.coreOrNull()!!
        assertEquals(-1, core.stats.lastStatus)
        assertEquals(0, core.stats.internalErrors)
        Analytics.shutdown()

        Analytics.setup(config.copy(host = host), PlatformContext(storageDir = dir))
        withTimeout(20_000) {
            Analytics.flush()
            while (received.isEmpty()) delay(50)
        }
        assertEquals(listOf("kept"), received.single().batch().names)
    }

    @Test
    fun authErrorsAreSeenAsStatusCodesByTheRealTransport() = runBlocking {
        // The JDK client has special handling for 401; the SDK must still see the plain status
        // so that it can drop the batch and pause (design §3.5).
        val dir = tempDir()
        val config = Config(token = "pk_revoked", host = host, flushAt = 1, captureLifecycle = false)
        statuses.addAll(listOf(401))
        Analytics.setup(config, PlatformContext(storageDir = dir))
        Analytics.capture("rejected")
        withTimeout(20_000) { Analytics.flush() }
        val core = Analytics.coreOrNull()!!
        assertEquals(401, core.stats.lastStatus)
        assertEquals(1, core.stats.droppedRecords)
        assertEquals(1, received.size)
        Analytics.capture("held back for an hour")
        withTimeout(20_000) { Analytics.flush() }
        delay(1_500)
        assertEquals(1, received.size, "sending is paused after 401")

        for (status in listOf(401, 403, 407, 413, 500)) {
            statuses.add(status)
            assertEquals(status, httpPost("$host/v1/e", mapOf("Authorization" to "Bearer x"), ByteArray(2_000)), "status $status")
        }
    }

    @Test
    fun platformHttpFunctions() = runBlocking {
        assertEquals(200, httpPost("$host/v1/e", mapOf("Authorization" to "Bearer x"), byteArrayOf(1, 2, 3)))
        assertEquals(3, received.single().body.size)

        val first = httpGet("$host/flags/pk_x.json", emptyMap())
        assertEquals(200, first.status)
        assertEquals("\"v31\"", first.headers["etag"])
        assertEquals("""{"v":31,"flags":[]}""", first.body.decodeToString())

        val second = httpGet("$host/flags/pk_x.json", mapOf("If-None-Match" to "\"v31\""))
        assertEquals(304, second.status)
        assertEquals(0, second.body.size)

        assertEquals(404, httpGet("$host/nope", emptyMap()).status)
        assertFailsWith<Exception> { httpGet("http://127.0.0.1:1/x", emptyMap()) }
        Unit
    }
}
