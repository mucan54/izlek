package dev.izlek.analytics.platform

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.FileOutputStream
import java.io.IOException
import java.io.InputStream
import java.io.RandomAccessFile
import java.net.HttpRetryException
import java.net.HttpURLConnection
import java.net.URI
import java.security.SecureRandom
import java.util.zip.GZIPOutputStream

// Actuals shared by the JVM and Android targets (java.* only).

private const val CONNECT_TIMEOUT_MS = 10_000
private const val READ_TIMEOUT_MS = 30_000
private const val MAX_RESPONSE_BYTES = 2 * 1024 * 1024

internal actual suspend fun httpRequest(
    method: String,
    url: String,
    headers: Map<String, String>,
    body: ByteArray?,
): HttpResponse = withContext(Dispatchers.IO) {
    val conn = URI.create(url).toURL().openConnection() as HttpURLConnection
    try {
        conn.requestMethod = method
        conn.connectTimeout = CONNECT_TIMEOUT_MS
        conn.readTimeout = READ_TIMEOUT_MS
        conn.useCaches = false
        conn.instanceFollowRedirects = false
        for ((k, v) in headers) conn.setRequestProperty(k, v)
        if (body != null) {
            // No streaming mode on purpose: with a streamed body the JDK turns 401/407 answers
            // into HttpRetryException instead of reporting the status. Bodies are at most ~1 MB.
            conn.doOutput = true
            conn.outputStream.use { it.write(body) }
        }
        val status = try {
            conn.responseCode
        } catch (e: HttpRetryException) {
            return@withContext HttpResponse(e.responseCode())
        }
        val responseHeaders = HashMap<String, String>()
        for ((k, v) in conn.headerFields) {
            if (k != null && v != null) responseHeaders[k.lowercase()] = v.joinToString(",")
        }
        val stream: InputStream? = try {
            if (status >= 400) conn.errorStream else conn.inputStream
        } catch (e: IOException) {
            null
        }
        val bytes = stream?.use { readLimited(it) } ?: ByteArray(0)
        HttpResponse(status, responseHeaders, bytes)
    } finally {
        conn.disconnect()
    }
}

private fun readLimited(input: InputStream): ByteArray {
    val out = ByteArrayOutputStream()
    val buf = ByteArray(8192)
    while (out.size() < MAX_RESPONSE_BYTES) {
        val n = input.read(buf)
        if (n < 0) break
        out.write(buf, 0, n)
    }
    return out.toByteArray()
}

internal actual fun gzip(bytes: ByteArray): ByteArray {
    val out = ByteArrayOutputStream(bytes.size / 2 + 32)
    GZIPOutputStream(out).use { it.write(bytes) }
    return out.toByteArray()
}

private val secureRandom by lazy { SecureRandom() }

internal actual fun randomBytes(n: Int): ByteArray = ByteArray(n).also { secureRandom.nextBytes(it) }

internal actual fun currentTimeMillis(): Long = System.currentTimeMillis()

internal actual fun platformFileSystem(): FileSystem = JavaFileSystem

internal object JavaFileSystem : FileSystem {
    private inline fun <T> io(what: String, block: () -> T): T = try {
        block()
    } catch (e: IOException) {
        throw FileIoException("$what: ${e.message}", e)
    } catch (e: SecurityException) {
        throw FileIoException("$what: ${e.message}", e)
    }

    override fun mkdirs(path: String) {
        val f = File(path)
        if (!f.isDirectory && !f.mkdirs() && !f.isDirectory) throw FileIoException("mkdirs failed: $path")
    }

    override fun list(dir: String): List<String> = io("list") { File(dir).list()?.toList() ?: emptyList() }

    override fun readBytes(path: String): ByteArray? = io("read") {
        val f = File(path)
        if (!f.isFile) null else f.readBytes()
    }

    override fun append(path: String, bytes: ByteArray) = io("append") {
        FileOutputStream(path, true).use { it.write(bytes) }
    }

    override fun writeAtomic(path: String, bytes: ByteArray) = io("write") {
        val tmp = File("$path.tmp")
        FileOutputStream(tmp).use {
            it.write(bytes)
            it.fd.sync()
        }
        renameFile(tmp, File(path))
    }

    override fun rename(from: String, to: String) = io("rename") { renameFile(File(from), File(to)) }

    private fun renameFile(from: File, to: File) {
        // File.renameTo is rename(2) on POSIX systems (atomic, replaces the target). On systems
        // where it refuses to replace an existing file, fall back to delete + rename.
        if (from.renameTo(to)) return
        if (to.exists() && to.delete() && from.renameTo(to)) return
        throw IOException("rename failed: ${from.name} -> ${to.name}")
    }

    override fun delete(path: String) {
        val f = File(path)
        if (!f.delete() && f.exists()) throw FileIoException("delete failed: $path")
    }

    override fun truncate(path: String, size: Long) = io("truncate") {
        RandomAccessFile(path, "rw").use { if (it.length() > size) it.setLength(size) }
    }
}
