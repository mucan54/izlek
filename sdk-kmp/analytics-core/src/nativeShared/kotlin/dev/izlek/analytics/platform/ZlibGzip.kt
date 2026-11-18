@file:OptIn(ExperimentalForeignApi::class)

package dev.izlek.analytics.platform

import kotlinx.cinterop.ExperimentalForeignApi
import kotlinx.cinterop.addressOf
import kotlinx.cinterop.alloc
import kotlinx.cinterop.convert
import kotlinx.cinterop.memScoped
import kotlinx.cinterop.ptr
import kotlinx.cinterop.reinterpret
import kotlinx.cinterop.sizeOf
import kotlinx.cinterop.usePinned
import platform.zlib.Z_DEFAULT_COMPRESSION
import platform.zlib.Z_DEFAULT_STRATEGY
import platform.zlib.Z_DEFLATED
import platform.zlib.Z_FINISH
import platform.zlib.Z_OK
import platform.zlib.Z_STREAM_END
import platform.zlib.ZLIB_VERSION
import platform.zlib.deflate
import platform.zlib.deflateBound
import platform.zlib.deflateEnd
import platform.zlib.deflateInit2_
import platform.zlib.z_stream

/** gzip (RFC 1952) with zlib: `deflateInit2` with windowBits 15 + 16 selects the gzip wrapper. */
internal actual fun gzip(bytes: ByteArray): ByteArray = memScoped {
    val stream = alloc<z_stream>()
    stream.zalloc = null
    stream.zfree = null
    stream.opaque = null
    val init = deflateInit2_(
        stream.ptr, Z_DEFAULT_COMPRESSION, Z_DEFLATED, 15 + 16, 8, Z_DEFAULT_STRATEGY,
        ZLIB_VERSION, sizeOf<z_stream>().toInt(),
    )
    if (init != Z_OK) throw IllegalStateException("deflateInit2 failed: $init")
    try {
        // deflateBound covers the deflate stream and the gzip wrapper chosen at init time.
        val bound = deflateBound(stream.ptr, bytes.size.convert()).toInt() + 32
        val out = ByteArray(bound)
        // zlib needs a non-null input pointer even for empty input.
        val input = if (bytes.isEmpty()) ByteArray(1) else bytes
        val produced = input.usePinned { src ->
            out.usePinned { dst ->
                stream.next_in = src.addressOf(0).reinterpret()
                stream.avail_in = bytes.size.convert()
                stream.next_out = dst.addressOf(0).reinterpret()
                stream.avail_out = out.size.convert()
                val rc = deflate(stream.ptr, Z_FINISH)
                if (rc != Z_STREAM_END) throw IllegalStateException("deflate failed: $rc")
                out.size - stream.avail_out.toInt()
            }
        }
        out.copyOf(produced)
    } finally {
        deflateEnd(stream.ptr)
    }
}
