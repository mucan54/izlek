package dev.izlek.analytics

import dev.izlek.analytics.core.Segment
import dev.izlek.analytics.platform.FileIoException
import dev.izlek.analytics.platform.PlatformDeps
import dev.izlek.analytics.platform.currentTimeMillis
import dev.izlek.analytics.platform.gzip
import dev.izlek.analytics.platform.platformFileSystem
import dev.izlek.analytics.platform.randomBytes
import dev.izlek.analytics.testkit.TestEnv
import kotlinx.coroutines.test.runTest
import kotlin.test.Test
import kotlin.test.assertContentEquals
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertNull
import kotlin.test.assertTrue

/** A fresh, empty directory that the test may write to. */
internal expect fun createTempDir(): String

/**
 * Exercises the real platform actuals of the running target (java.io on JVM/Android unit tests,
 * POSIX + zlib on native). HTTP is covered separately (jvmTest).
 */
class PlatformLayerTest {

    @Test
    fun fileSystemContract() {
        val fs = platformFileSystem()
        val dir = createTempDir() + "/nested/analytics"
        assertEquals(emptyList(), fs.list(dir), "missing directory lists as empty")
        fs.mkdirs(dir)
        fs.mkdirs(dir) // idempotent
        val f = "$dir/active"
        assertNull(fs.readBytes(f))

        fs.append(f, byteArrayOf(1, 2, 3))
        fs.append(f, byteArrayOf(4, 5))
        fs.append(f, ByteArray(0))
        assertContentEquals(byteArrayOf(1, 2, 3, 4, 5), fs.readBytes(f))

        fs.truncate(f, 2)
        assertContentEquals(byteArrayOf(1, 2), fs.readBytes(f))
        fs.append(f, byteArrayOf(9))
        assertContentEquals(byteArrayOf(1, 2, 9), fs.readBytes(f))

        fs.rename(f, "$dir/pending-1")
        assertNull(fs.readBytes(f))
        assertContentEquals(byteArrayOf(1, 2, 9), fs.readBytes("$dir/pending-1"))

        fs.writeAtomic("$dir/state", "one".encodeToByteArray())
        fs.writeAtomic("$dir/state", "two".encodeToByteArray())
        assertEquals("two", fs.readBytes("$dir/state")!!.decodeToString())
        assertEquals(listOf("pending-1", "state"), fs.list(dir).sorted(), "no temporary file is left behind")

        // rename replaces an existing target
        fs.append("$dir/x", byteArrayOf(7))
        fs.rename("$dir/x", "$dir/pending-1")
        assertContentEquals(byteArrayOf(7), fs.readBytes("$dir/pending-1"))

        val big = ByteArray(300_000) { (it * 31).toByte() }
        fs.writeAtomic("$dir/big", big)
        assertContentEquals(big, fs.readBytes("$dir/big"))

        fs.delete("$dir/big")
        fs.delete("$dir/big") // deleting a missing file is fine
        fs.delete("$dir/state")
        fs.delete("$dir/pending-1")
        assertEquals(emptyList(), fs.list(dir))

        assertFailsWith<FileIoException> { fs.rename("$dir/missing", "$dir/other") }
        assertFailsWith<FileIoException> { fs.append("$dir/no-such-dir/file", byteArrayOf(1)) }
    }

    private fun crc32(data: ByteArray): Long {
        var crc = 0xFFFFFFFFL
        for (b in data) {
            crc = crc xor (b.toLong() and 0xFF)
            repeat(8) { crc = if (crc and 1L != 0L) (crc ushr 1) xor 0xEDB88320L else crc ushr 1 }
        }
        return crc xor 0xFFFFFFFFL
    }

    private fun u32le(b: ByteArray, at: Int): Long =
        (0 until 4).fold(0L) { acc, i -> acc or ((b[at + i].toLong() and 0xFF) shl (8 * i)) }

    @Test
    fun gzipProducesAGzipMemberWithMatchingTrailer() {
        val inputs = listOf(
            ByteArray(0),
            "hello".encodeToByteArray(),
            ByteArray(200_000) { (it % 7).toByte() },
            randomBytes(5_000),
        )
        for (input in inputs) {
            val gz = gzip(input)
            assertTrue(gz.size >= 18, "gzip member has header and trailer")
            assertEquals(0x1f, gz[0].toInt() and 0xFF)
            assertEquals(0x8b, gz[1].toInt() and 0xFF)
            assertEquals(8, gz[2].toInt(), "deflate")
            assertEquals(crc32(input), u32le(gz, gz.size - 8), "CRC-32 of the uncompressed data")
            assertEquals(input.size.toLong(), u32le(gz, gz.size - 4), "ISIZE")
        }
        assertTrue(gzip(inputs[2]).size < 2_000, "compressible data is actually compressed")
    }

    @Test
    fun randomAndClock() {
        assertEquals(0, randomBytes(0).size)
        val a = randomBytes(16)
        val b = randomBytes(16)
        assertEquals(16, a.size)
        assertFalse(a.contentEquals(b))
        val now = currentTimeMillis()
        assertTrue(now > 1_700_000_000_000L, "unix milliseconds: $now")
    }

    /** The queue on the real file layer: survive a restart, repair a torn tail. */
    @Test
    fun queueOnRealFileSystem() = runTest {
        val env = TestEnv(testScheduler)
        val storage = createTempDir()
        fun deps(): PlatformDeps {
            val d = env.deps(storage)
            return PlatformDeps(
                storageDir = storage, fs = platformFileSystem(), http = d.http, nowMs = d.nowMs,
                monotonicMs = d.monotonicMs, randomBytes = ::randomBytes, gzip = ::gzip,
                deviceContext = d.deviceContext, registerLifecycle = d.registerLifecycle,
                beginBackgroundTask = d.beginBackgroundTask, dispatcher = d.dispatcher,
            )
        }
        val config = Config("pk_real_fs", "https://in.example.test", flushAt = 50, flushIntervalMs = 0, captureLifecycle = false, gzip = false)
        try {
            val core = Analytics.start(config, deps())
            testScheduler.runCurrent()
            Analytics.capture("a", mapOf("n" to 1))
            Analytics.capture("b")
            testScheduler.runCurrent()
            assertEquals(0, env.http.requests.size)
            Analytics.shutdown()

            val fs = platformFileSystem()
            val active = "${core.dir}/active"
            val good = fs.readBytes(active)!!
            assertEquals(2, Segment.scan(good)!!.recordCount)
            fs.append(active, byteArrayOf(0x40, 1, 2, 3)) // torn record: announces 64 bytes, has 3

            Analytics.start(config, deps())
            testScheduler.runCurrent()
            assertEquals(listOf("a", "b"), env.http.batches().single().names)
            assertEquals(listOf("state"), fs.list(core.dir))
        } finally {
            Analytics.shutdown()
        }
    }
}
