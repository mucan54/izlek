package dev.izlek.analytics.testkit

import dev.izlek.analytics.platform.FileIoException
import dev.izlek.analytics.platform.FileSystem
import dev.izlek.analytics.platform.HttpResponse
import dev.izlek.analytics.platform.HttpTransport
import dev.izlek.analytics.platform.PlatformDeps
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.TestCoroutineScheduler

/**
 * In-memory [FileSystem] with failure injection.
 *
 * Survives "process restarts" in tests: keep the instance and start a new SDK core on it.
 */
public class FakeFileSystem : FileSystem {
    public val files: LinkedHashMap<String, ByteArray> = LinkedHashMap()

    /** Every append / writeAtomic throws before writing anything. */
    public var failWrites: Boolean = false

    /** With [failWrites]: an append first writes this many bytes, then throws (torn write). */
    public var tornAppendBytes: Int = 0

    /** Disk size in bytes; a write that does not fit writes what fits (append) and throws. */
    public var capacityBytes: Long = Long.MAX_VALUE

    public var failRenames: Boolean = false
    public var failReads: Boolean = false
    public var failTruncates: Boolean = false

    public var appendCalls: Int = 0
    public var atomicWrites: Int = 0

    public fun usedBytes(): Long = files.values.sumOf { it.size.toLong() }

    public fun names(dir: String): List<String> = list(dir).sorted()

    override fun mkdirs(path: String) {}

    override fun list(dir: String): List<String> {
        val prefix = dir.trimEnd('/') + "/"
        return files.keys.filter { it.startsWith(prefix) && !it.substring(prefix.length).contains('/') }
            .map { it.substring(prefix.length) }
    }

    override fun readBytes(path: String): ByteArray? {
        if (failReads) throw FileIoException("injected read failure")
        return files[path]?.copyOf()
    }

    override fun append(path: String, bytes: ByteArray) {
        appendCalls++
        val old = files[path] ?: ByteArray(0)
        if (failWrites) {
            if (tornAppendBytes > 0) files[path] = old + bytes.copyOf(minOf(tornAppendBytes, bytes.size))
            throw FileIoException("injected write failure")
        }
        val free = capacityBytes - usedBytes()
        if (bytes.size > free) {
            if (free > 0) files[path] = old + bytes.copyOf(free.toInt())
            throw FileIoException("no space left on device")
        }
        files[path] = old + bytes
    }

    override fun writeAtomic(path: String, bytes: ByteArray) {
        atomicWrites++
        if (failWrites) throw FileIoException("injected write failure")
        val free = capacityBytes - usedBytes() + (files[path]?.size ?: 0)
        if (bytes.size > free) throw FileIoException("no space left on device")
        files[path] = bytes.copyOf()
    }

    override fun rename(from: String, to: String) {
        if (failRenames) throw FileIoException("injected rename failure")
        val data = files.remove(from) ?: throw FileIoException("rename: no such file $from")
        files[to] = data
    }

    override fun delete(path: String) {
        files.remove(path)
    }

    override fun truncate(path: String, size: Long) {
        if (failTruncates) throw FileIoException("injected truncate failure")
        val data = files[path] ?: throw FileIoException("truncate: no such file $path")
        if (data.size > size) files[path] = data.copyOf(size.toInt())
    }
}

/** Scriptable HTTP server stand-in. */
public class FakeHttp : HttpTransport {
    public class Request(
        public val method: String,
        public val url: String,
        public val headers: Map<String, String>,
        public val body: ByteArray?,
        /** Virtual time of the request. */
        public val atMs: Long,
    ) {
        /** Body with the fake gzip marker removed. */
        public fun plainBody(): ByteArray {
            val b = body ?: ByteArray(0)
            return if (headers["Content-Encoding"] == "gzip") TestEnv.fakeGunzip(b) else b
        }

        public fun batch(): DecodedBatch = WireDecoder.decode(plainBody())
    }

    public val requests: ArrayList<Request> = ArrayList()

    /**
     * Scripted answers, consumed in order: an `Int` status, an [HttpResponse], or a `Throwable`
     * (network error). When the script is empty [handler] decides.
     */
    public val script: ArrayDeque<Any> = ArrayDeque()

    public var handler: suspend (Request) -> HttpResponse = { HttpResponse(200) }

    public var now: () -> Long = { 0 }

    public fun enqueue(vararg answers: Any) {
        script.addAll(answers)
    }

    override suspend fun request(method: String, url: String, headers: Map<String, String>, body: ByteArray?): HttpResponse {
        val r = Request(method, url, headers, body, now())
        requests.add(r)
        val scripted = script.removeFirstOrNull() ?: return handler(r)
        return when (scripted) {
            is Int -> HttpResponse(scripted)
            is HttpResponse -> scripted
            is Throwable -> throw scripted
            else -> error("unsupported scripted answer: $scripted")
        }
    }

    public fun batches(): List<DecodedBatch> = requests.filter { it.url.endsWith("/v1/e") }.map { it.batch() }
}

public class NetworkDown : Exception("network unreachable (injected)")

/**
 * Deterministic environment for SDK tests: virtual time, fake wall clock that can jump, in-memory
 * disk, scripted HTTP.
 */
public class TestEnv(public val scheduler: TestCoroutineScheduler) {
    public val fs: FakeFileSystem = FakeFileSystem()
    public val http: FakeHttp = FakeHttp()

    /** Wall clock = [wallBaseMs] + virtual time + [wallOffsetMs]. */
    public var wallBaseMs: Long = 1_795_046_400_000L
    public var wallOffsetMs: Long = 0

    public var context: Map<String, Any?> = linkedMapOf("platform" to "test", "app_version" to "1.0.0", "app_build" to "1")

    /** Bytes handed out by `randomBytes` when set; otherwise a deterministic PRNG is used. */
    public var fixedRandom: Byte? = null

    public var initialForeground: Boolean = true

    public var onBackground: (() -> Unit)? = null
    public var onForeground: (() -> Unit)? = null

    public val backgroundTasks: ArrayList<String> = ArrayList()
    public var openBackgroundTasks: Int = 0

    private var seed = 0x2545F4914F6CDD1DL

    init {
        http.now = { scheduler.currentTime }
    }

    public fun wallNow(): Long = wallBaseMs + scheduler.currentTime + wallOffsetMs

    public fun jumpClock(deltaMs: Long) {
        wallOffsetMs += deltaMs
    }

    private fun random(n: Int): ByteArray {
        val fixed = fixedRandom
        return ByteArray(n) {
            if (fixed != null) {
                fixed
            } else {
                seed = seed xor (seed shl 13)
                seed = seed xor (seed ushr 7)
                seed = seed xor (seed shl 17)
                seed.toByte()
            }
        }
    }

    /** New [PlatformDeps] on the same disk/network: use one per simulated process. */
    public fun deps(storageDir: String = "/data"): PlatformDeps = PlatformDeps(
        storageDir = storageDir,
        fs = fs,
        http = http,
        nowMs = ::wallNow,
        monotonicMs = { scheduler.currentTime },
        randomBytes = ::random,
        gzip = ::fakeGzip,
        deviceContext = { context },
        registerLifecycle = { bg, fg ->
            onBackground = bg
            onForeground = fg
        },
        beginBackgroundTask = { name ->
            backgroundTasks.add(name)
            openBackgroundTasks++
            val end: () -> Unit = { openBackgroundTasks-- }
            end
        },
        dispatcher = StandardTestDispatcher(scheduler),
        initialForeground = initialForeground,
    )

    public companion object {
        private val MARK = byteArrayOf(0x1f, 0x8b.toByte(), 0x7f)

        /** Stand-in for gzip in common tests: a 3-byte marker followed by the input. */
        public fun fakeGzip(bytes: ByteArray): ByteArray = MARK + bytes

        public fun fakeGunzip(bytes: ByteArray): ByteArray {
            require(bytes.size >= 3 && bytes.copyOf(3).contentEquals(MARK)) { "not fake-gzipped" }
            return bytes.copyOfRange(3, bytes.size)
        }
    }
}
