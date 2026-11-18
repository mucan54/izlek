package dev.izlek.analytics

import dev.izlek.analytics.core.AnalyticsCore
import dev.izlek.analytics.core.Segment
import dev.izlek.analytics.testkit.DecodedBatch
import dev.izlek.analytics.testkit.DecodedRecord
import dev.izlek.analytics.testkit.TestEnv
import kotlinx.coroutines.test.TestResult
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.runTest

/** Drives the real SDK (through the public [Analytics] object) on fakes and virtual time. */
class Sdk(private val scope: TestScope) {
    val env = TestEnv(scope.testScheduler)

    /** Lifecycle events and the flush timer are off by default so tests see only their own events. */
    var config = Config(
        token = "pk_test",
        host = "https://in.example.test/",
        flushAt = 20,
        flushIntervalMs = 0,
        captureLifecycle = false,
        gzip = false,
    )

    lateinit var core: AnalyticsCore

    val now: Long get() = scope.testScheduler.currentTime

    fun start() {
        core = Analytics.start(config, env.deps())
        settle()
    }

    /** Simulates process death: all SDK coroutines stop, the disk stays as it is. */
    fun kill() {
        Analytics.shutdown()
    }

    fun restart() {
        kill()
        start()
    }

    fun settle() {
        scope.testScheduler.runCurrent()
    }

    fun advance(ms: Long) {
        scope.testScheduler.advanceTimeBy(ms)
        scope.testScheduler.runCurrent()
    }

    fun capture(name: String, props: Map<String, Any?> = emptyMap()) {
        Analytics.capture(name, props)
        settle()
    }

    fun captureMany(prefix: String, count: Int) {
        for (i in 0 until count) Analytics.capture("$prefix$i")
        settle()
    }

    val dir: String get() = core.dir
    fun files(): List<String> = env.fs.names(dir)
    fun pendingFiles(): List<String> = files().filter { it.startsWith("pending-") }
    fun file(name: String): ByteArray? = env.fs.files["$dir/$name"]

    val requests get() = env.http.requests
    fun batches(): List<DecodedBatch> = env.http.batches()
    fun records(): List<DecodedRecord> = batches().flatMap { it.records }
    fun names(): List<String> = records().map { it.name }

    /** Every queue file on the fake disk must be a well-formed segment without a torn tail. */
    fun assertQueueFilesWellFormed() {
        for (n in files()) {
            if (n != "active" && !n.startsWith("pending-")) continue
            val bytes = file(n)!!
            val scan = Segment.scan(bytes) ?: throw AssertionError("$n: bad preamble")
            if (scan.validLength != bytes.size) throw AssertionError("$n: torn tail (${bytes.size - scan.validLength} bytes)")
        }
    }
}

fun sdkTest(block: suspend Sdk.() -> Unit): TestResult = runTest {
    val sdk = Sdk(this)
    try {
        sdk.block()
    } finally {
        Analytics.shutdown()
    }
}
