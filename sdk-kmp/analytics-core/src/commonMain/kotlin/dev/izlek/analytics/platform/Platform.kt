package dev.izlek.analytics.platform

import dev.izlek.analytics.InternalIzlekApi
import dev.izlek.analytics.PlatformContext
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlin.time.TimeSource

// ---------------------------------------------------------------------------------------------
// Platform layer (design §4.5). Everything the common logic needs from the outside world goes
// through [PlatformDeps], so tests can replace time, randomness, HTTP and the file system.
// ---------------------------------------------------------------------------------------------

@InternalIzlekApi
public class HttpResponse(
    public val status: Int,
    /** Header names are lower-cased. */
    public val headers: Map<String, String> = emptyMap(),
    public val body: ByteArray = ByteArray(0),
)

@InternalIzlekApi
public fun interface HttpTransport {
    /**
     * Performs one request and returns whatever status the server answered with.
     * Throws (any exception) when no HTTP response was received: DNS, connect, TLS, timeout.
     */
    public suspend fun request(method: String, url: String, headers: Map<String, String>, body: ByteArray?): HttpResponse
}

@InternalIzlekApi
public class FileIoException(message: String, cause: Throwable? = null) : Exception(message, cause)

/**
 * The small file API the queue needs. Paths are absolute strings with `/` separators.
 * Every method may throw [FileIoException]; callers must treat any exception as an I/O failure.
 */
@InternalIzlekApi
public interface FileSystem {
    /** Creates the directory and missing parents. */
    public fun mkdirs(path: String)

    /** File names (not paths) directly inside [dir]; empty when the directory does not exist. */
    public fun list(dir: String): List<String>

    /** Whole file content, or null when the file does not exist. */
    public fun readBytes(path: String): ByteArray?

    /** Appends to the file, creating it when missing. A failed append may leave a partial tail. */
    public fun append(path: String, bytes: ByteArray)

    /** Replaces the file content atomically (temporary file + rename). */
    public fun writeAtomic(path: String, bytes: ByteArray)

    /** Atomic rename; replaces [to] when it exists. */
    public fun rename(from: String, to: String)

    /** Deletes a file; a missing file is not an error. */
    public fun delete(path: String)

    /** Shrinks the file to [size] bytes. */
    public fun truncate(path: String, size: Long)
}

/** Everything platform specific, bundled so that it can be faked as a whole or piece by piece. */
@InternalIzlekApi
public class PlatformDeps(
    /** Application data directory; the SDK works below `<storageDir>/analytics/<token-hash>/`. */
    public val storageDir: String,
    public val fs: FileSystem,
    public val http: HttpTransport,
    /** Wall clock, unix ms. May jump in either direction. */
    public val nowMs: () -> Long,
    /** Monotonic clock in ms with an arbitrary origin; used for retry timing. */
    public val monotonicMs: () -> Long,
    public val randomBytes: (Int) -> ByteArray,
    public val gzip: (ByteArray) -> ByteArray,
    /** Automatic context (design §4.7); evaluated when a batch header is built. */
    public val deviceContext: () -> Map<String, Any?>,
    /** Registers background/foreground callbacks. They may be invoked on any thread. */
    public val registerLifecycle: (onBackground: () -> Unit, onForeground: () -> Unit) -> Unit,
    /** Asks the OS for extra background time; the returned function ends the request. */
    public val beginBackgroundTask: (name: String) -> (() -> Unit),
    /** Serial dispatcher that owns all SDK state. */
    public val dispatcher: CoroutineDispatcher,
    /** Whether the application is visible when the SDK starts. */
    public val initialForeground: Boolean = true,
)

@InternalIzlekApi
public fun defaultPlatformDeps(context: PlatformContext): PlatformDeps {
    val started = TimeSource.Monotonic.markNow()
    val fs = platformFileSystem()
    return PlatformDeps(
        storageDir = storageDir(context),
        fs = fs,
        http = { method, url, headers, body -> httpRequest(method, url, headers, body) },
        nowMs = ::currentTimeMillis,
        monotonicMs = { started.elapsedNow().inWholeMilliseconds },
        randomBytes = ::randomBytes,
        gzip = ::gzip,
        deviceContext = deviceContextProvider(context),
        registerLifecycle = { onBackground, onForeground -> lifecycle(context, onBackground, onForeground) },
        beginBackgroundTask = ::beginBackgroundTask,
        dispatcher = Dispatchers.Default.limitedParallelism(1),
        initialForeground = isAppInForeground(context),
    )
}

// --- expect/actual surface -------------------------------------------------------------------

/**
 * One HTTP exchange (`HttpURLConnection` on JVM/Android, `NSURLSession` on iOS).
 * Returns the status code, response headers and body; throws on transport failures.
 */
internal expect suspend fun httpRequest(method: String, url: String, headers: Map<String, String>, body: ByteArray?): HttpResponse

/** `POST` returning only the status code, as in design §4.5. */
internal suspend fun httpPost(url: String, headers: Map<String, String>, body: ByteArray): Int =
    httpRequest("POST", url, headers, body).status

/** `GET` used by the flag ruleset download (needs `ETag` and the body). */
internal suspend fun httpGet(url: String, headers: Map<String, String>): HttpResponse =
    httpRequest("GET", url, headers, null)

/** `context.filesDir` / user directory / `NSApplicationSupportDirectory`. */
internal expect fun storageDir(context: PlatformContext): String

/** gzip (RFC 1952) of [bytes]. */
internal expect fun gzip(bytes: ByteArray): ByteArray

/** Application background/foreground notifications. */
internal expect fun lifecycle(context: PlatformContext, onBackground: () -> Unit, onForeground: () -> Unit)

/** Whether the application is in the foreground right now (best effort; true when unknown). */
internal expect fun isAppInForeground(context: PlatformContext): Boolean

/**
 * Returns a provider for the automatic context. Static values are collected once, on the
 * calling thread (the thread that called `Analytics.setup`); the provider only adds cheap
 * dynamic values such as `network`.
 */
internal expect fun deviceContextProvider(context: PlatformContext): () -> Map<String, Any?>

/** Cryptographically strong random bytes (`SecureRandom` / `SecRandomCopyBytes`). */
internal expect fun randomBytes(n: Int): ByteArray

/** Wall clock in unix milliseconds. */
internal expect fun currentTimeMillis(): Long

internal expect fun platformFileSystem(): FileSystem

/** iOS: `beginBackgroundTask`; elsewhere a no-op. Returns the function that ends the task. */
internal expect fun beginBackgroundTask(name: String): () -> Unit
