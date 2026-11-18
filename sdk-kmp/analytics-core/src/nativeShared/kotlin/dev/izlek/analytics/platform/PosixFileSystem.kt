@file:OptIn(ExperimentalForeignApi::class)

package dev.izlek.analytics.platform

import kotlinx.cinterop.ExperimentalForeignApi
import kotlinx.cinterop.addressOf
import kotlinx.cinterop.convert
import kotlinx.cinterop.pointed
import kotlinx.cinterop.toKString
import kotlinx.cinterop.usePinned
import platform.posix.EEXIST
import platform.posix.ENOENT
import platform.posix.closedir
import platform.posix.errno
import platform.posix.fclose
import platform.posix.fflush
import platform.posix.fileno
import platform.posix.fopen
import platform.posix.fread
import platform.posix.fsync
import platform.posix.fwrite
import platform.posix.mkdir
import platform.posix.opendir
import platform.posix.readdir
import platform.posix.remove
import platform.posix.strerror

// POSIX implementation of the SDK file layer. Compiled into the iOS source set and into the
// linuxX64 verification target (see buildSrc/izlek.kmp-library.gradle.kts).

internal actual fun platformFileSystem(): FileSystem = PosixFileSystem

internal object PosixFileSystem : FileSystem {

    private fun fail(what: String, path: String): Nothing {
        val code = errno
        throw FileIoException("$what failed for $path: ${strerror(code)?.toKString() ?: code.toString()}")
    }

    override fun mkdirs(path: String) {
        var i = 1
        while (i <= path.length) {
            if (i == path.length || path[i] == '/') {
                val part = path.substring(0, i)
                if (mkdir(part, 0x1C0.convert()) != 0 && errno != EEXIST) {
                    // Parents that exist but are not writable report other errors (EACCES,
                    // EROFS, EISDIR); only the final component decides.
                    if (i == path.length) fail("mkdir", part)
                }
            }
            i++
        }
    }

    override fun list(dir: String): List<String> {
        val d = opendir(dir) ?: return emptyList()
        val out = ArrayList<String>()
        try {
            while (true) {
                val entry = readdir(d) ?: break
                val name = entry.pointed.d_name.toKString()
                if (name != "." && name != "..") out.add(name)
            }
        } finally {
            closedir(d)
        }
        return out
    }

    override fun readBytes(path: String): ByteArray? {
        val f = fopen(path, "rb")
        if (f == null) {
            if (errno == ENOENT) return null
            fail("open", path)
        }
        try {
            var out = ByteArray(0)
            val chunk = ByteArray(16 * 1024)
            while (true) {
                val n = chunk.usePinned { fread(it.addressOf(0), 1.convert(), chunk.size.convert(), f) }.toInt()
                if (n <= 0) break
                val grown = out.copyOf(out.size + n)
                chunk.copyInto(grown, out.size, 0, n)
                out = grown
            }
            return out
        } finally {
            fclose(f)
        }
    }

    override fun append(path: String, bytes: ByteArray) {
        write(path, "ab", bytes, sync = false)
    }

    override fun writeAtomic(path: String, bytes: ByteArray) {
        val tmp = "$path.tmp"
        write(tmp, "wb", bytes, sync = true)
        rename(tmp, path)
    }

    private fun write(path: String, mode: String, bytes: ByteArray, sync: Boolean) {
        val f = fopen(path, mode) ?: fail("open", path)
        var ok = true
        if (bytes.isNotEmpty()) {
            val written = bytes.usePinned { fwrite(it.addressOf(0), 1.convert(), bytes.size.convert(), f) }.toLong()
            if (written != bytes.size.toLong()) ok = false
        }
        if (fflush(f) != 0) ok = false
        if (ok && sync && fsync(fileno(f)) != 0) ok = false
        val code = errno
        if (fclose(f) != 0) ok = false
        if (!ok) throw FileIoException("write failed for $path: ${strerror(code)?.toKString() ?: code.toString()}")
    }

    override fun rename(from: String, to: String) {
        if (platform.posix.rename(from, to) != 0) fail("rename", from)
    }

    override fun delete(path: String) {
        if (remove(path) != 0 && errno != ENOENT) fail("delete", path)
    }

    override fun truncate(path: String, size: Long) {
        if (platform.posix.truncate(path, size.convert()) != 0) fail("truncate", path)
    }
}
