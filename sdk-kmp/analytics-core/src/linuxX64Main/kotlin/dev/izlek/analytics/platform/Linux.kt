@file:OptIn(ExperimentalForeignApi::class)

package dev.izlek.analytics.platform

import dev.izlek.analytics.PlatformContext
import kotlinx.cinterop.ExperimentalForeignApi
import kotlinx.cinterop.addressOf
import kotlinx.cinterop.alloc
import kotlinx.cinterop.convert
import kotlinx.cinterop.memScoped
import kotlinx.cinterop.ptr
import kotlinx.cinterop.usePinned
import platform.posix.CLOCK_REALTIME
import platform.posix.clock_gettime
import platform.posix.fclose
import platform.posix.fopen
import platform.posix.fread
import platform.posix.timespec

// Verification-only target: no HTTP transport and no lifecycle.

internal actual suspend fun httpRequest(
    method: String,
    url: String,
    headers: Map<String, String>,
    body: ByteArray?,
): HttpResponse = throw FileIoException("no HTTP transport on linuxX64 (verification-only target)")

internal actual fun storageDir(context: PlatformContext): String = context.storageDir

internal actual fun lifecycle(context: PlatformContext, onBackground: () -> Unit, onForeground: () -> Unit) {}

internal actual fun isAppInForeground(context: PlatformContext): Boolean = true

internal actual fun deviceContextProvider(context: PlatformContext): () -> Map<String, Any?> {
    val m = mapOf<String, Any?>("platform" to "linux", "os" to "Linux", "device_type" to "desktop")
    return { m }
}

internal actual fun randomBytes(n: Int): ByteArray {
    val out = ByteArray(n)
    if (n == 0) return out
    val f = fopen("/dev/urandom", "rb") ?: throw IllegalStateException("cannot open /dev/urandom")
    try {
        val read = out.usePinned { fread(it.addressOf(0), 1.convert(), n.convert(), f) }.toInt()
        if (read != n) throw IllegalStateException("short read from /dev/urandom")
    } finally {
        fclose(f)
    }
    return out
}

internal actual fun currentTimeMillis(): Long = memScoped {
    val ts = alloc<timespec>()
    clock_gettime(CLOCK_REALTIME, ts.ptr)
    ts.tv_sec * 1000L + ts.tv_nsec / 1_000_000L
}

internal actual fun beginBackgroundTask(name: String): () -> Unit = {}
