package dev.izlek.analytics.core

import dev.izlek.analytics.InternalIzlekApi
import dev.izlek.analytics.internal.ByteFormatException
import dev.izlek.analytics.internal.ByteReader
import dev.izlek.analytics.internal.ByteWriter
import dev.izlek.analytics.internal.Wire

/**
 * Identity a segment belongs to. All records of one segment share it (design §4.6); it is
 * copied into the batch header when the segment is sent.
 */
@InternalIzlekApi
public class SegmentMeta(
    /** Wall-clock time the segment was started; used for `maxEventAgeMs`. */
    public val createdMs: Long,
    public val anonId: String,
    public val userId: String,
    public val sessionId: ByteArray,
) {
    public fun sameIdentity(other: SegmentMeta): Boolean =
        anonId == other.anonId && userId == other.userId && sessionId.contentEquals(other.sessionId)
}

/**
 * On-disk segment (`active` and `pending-*` files):
 *
 * ```
 * Segment  := Preamble Record*
 * Preamble := varint(len) version:u8(=1) created_ms:varint anon_id:string user_id:string session_id:bytes16
 * Record   := varint(len) RecordBody            ; exactly the LA1 record framing
 * ```
 *
 * The request body is `Magic + Header + <segment bytes after the preamble>`: the records are
 * sent as stored, the header is built at send time from the preamble and the current context.
 */
@InternalIzlekApi
public object Segment {
    private const val VERSION = 1

    public class Scan(
        public val meta: SegmentMeta,
        /** Offset of the first record. */
        public val recordsOffset: Int,
        /** Offset just past the last complete record. Bytes beyond it are a torn tail. */
        public val validLength: Int,
        /** Start offset of every record, plus [validLength] as the last element. */
        public val boundaries: IntArray,
    ) {
        public val recordCount: Int get() = boundaries.size - 1
    }

    public fun preamble(meta: SegmentMeta): ByteArray {
        val w = ByteWriter(64)
        w.u8(VERSION)
        w.varint(meta.createdMs)
        w.string(meta.anonId)
        w.string(meta.userId)
        w.bytes(meta.sessionId)
        return Wire.frame(w.toByteArray())
    }

    /**
     * Walks a segment file. Returns null when the preamble is missing or damaged (the file is
     * unusable). Scanning stops at the first incomplete or malformed record.
     */
    public fun scan(bytes: ByteArray): Scan? {
        val r = ByteReader(bytes)
        val meta: SegmentMeta
        try {
            val len = r.length()
            if (len < 1 || len > Wire.MAX_FRAME_BYTES || len > r.remaining) return null
            val end = r.pos + len
            val p = ByteReader(bytes, r.pos, end)
            if (p.u8() != VERSION) return null
            val created = p.varint()
            val anon = p.string(strictUtf8 = true)
            val user = p.string(strictUtf8 = true)
            val session = p.bytes(16)
            meta = SegmentMeta(created, anon, user, session)
            r.pos = end
        } catch (e: ByteFormatException) {
            return null
        }
        val recordsOffset = r.pos
        var bounds = IntArray(16)
        var n = 0
        var valid = recordsOffset
        while (r.remaining > 0) {
            val start = r.pos
            val len = try {
                r.length()
            } catch (e: ByteFormatException) {
                break
            }
            if (len < 1 || len > Wire.MAX_FRAME_BYTES || len > r.remaining) break
            if (!isWellFormedRecord(bytes, r.pos, r.pos + len)) break
            r.pos += len
            if (n == bounds.size) bounds = bounds.copyOf(n * 2)
            bounds[n++] = start
            valid = r.pos
        }
        val out = IntArray(n + 1)
        bounds.copyInto(out, 0, 0, n)
        out[n] = valid
        return Scan(meta, recordsOffset, valid, out)
    }

    /** Structural check of a `RecordBody` (does not validate UTF-8). */
    private fun isWellFormedRecord(bytes: ByteArray, from: Int, to: Int): Boolean {
        val r = ByteReader(bytes, from, to)
        return try {
            r.u8()
            r.skip(16)
            r.varint()
            r.skip(r.length())
            val count = r.length()
            for (i in 0 until count) {
                r.skip(r.length())
                when (r.u8()) {
                    Wire.TAG_NULL, Wire.TAG_FALSE, Wire.TAG_TRUE -> {}
                    Wire.TAG_INT -> r.varint()
                    Wire.TAG_FLOAT -> r.skip(8)
                    Wire.TAG_STRING, Wire.TAG_JSON -> r.skip(r.length())
                    else -> return false
                }
            }
            true
        } catch (e: ByteFormatException) {
            false
        }
    }
}
