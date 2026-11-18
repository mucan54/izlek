package dev.izlek.analytics.testkit

import dev.izlek.analytics.internal.ByteFormatException
import dev.izlek.analytics.internal.ByteReader
import dev.izlek.analytics.internal.Wire
import dev.izlek.analytics.internal.toHex

/** One decoded property. [value] is `null`, `Boolean`, `Long`, `Double` or `String` (string and json tags). */
public class DecodedProp(public val key: String, public val tag: Int, public val value: Any?) {
    override fun toString(): String = "$key=$value"
}

public class DecodedRecord(
    public val kind: Int,
    public val uuid: ByteArray,
    public val ts: Long,
    public val name: String,
    public val props: List<DecodedProp>,
) {
    /** Last value wins (spec/wire.md §2). */
    public fun prop(key: String): Any? = props.lastOrNull { it.key == key }?.value

    public fun has(key: String): Boolean = props.any { it.key == key }

    override fun toString(): String = "Record(kind=$kind, name=$name, ts=$ts, props=$props)"
}

public class DecodedBatch(
    public val batchId: ByteArray,
    public val sentAt: Long,
    public val sdk: String,
    public val anonId: String,
    public val userId: String,
    public val sessionId: ByteArray,
    public val context: List<DecodedProp>,
    /** Records with a known kind that decoded cleanly, in order. */
    public val records: List<DecodedRecord>,
    /** Indices (among all framed records) that failed to decode. */
    public val badRecords: List<Int>,
) {
    public val batchIdHex: String get() = batchId.toHex()
    public val sessionIdHex: String get() = sessionId.toHex()

    public fun context(key: String): Any? = context.lastOrNull { it.key == key }?.value

    public val names: List<String> get() = records.map { it.name }
}

/** Reference-style LA1 decoder used by tests (frame check of the hot path + full decode). */
public object WireDecoder {
    public class FrameCheck(public val ok: Boolean, public val httpStatus: Int)

    public class HeaderException(message: String) : Exception(message)

    /** Frame validation exactly as the ingestion hot path does it (spec/wire.md §3). */
    public fun frameCheck(bytes: ByteArray): FrameCheck {
        if (bytes.size < 4 || !bytes.copyOfRange(0, 4).contentEquals(Wire.MAGIC)) return FrameCheck(false, 400)
        val r = ByteReader(bytes, 4)
        try {
            val headerLen = r.length()
            if (headerLen < 16 || headerLen > Wire.MAX_FRAME_BYTES || headerLen > r.remaining) return FrameCheck(false, 400)
            r.skip(headerLen)
            var count = 0
            while (r.remaining > 0) {
                val len = r.length()
                if (len < 1 || len > Wire.MAX_FRAME_BYTES || len > r.remaining) return FrameCheck(false, 400)
                r.skip(len)
                count++
            }
            if (count > Wire.MAX_BATCH_RECORDS) return FrameCheck(false, 413)
        } catch (e: ByteFormatException) {
            return FrameCheck(false, 400)
        }
        return FrameCheck(true, 200)
    }

    /**
     * Full decode of a frame-valid batch.
     * @throws HeaderException when the frame or the header is invalid.
     */
    public fun decode(bytes: ByteArray): DecodedBatch {
        if (!frameCheck(bytes).ok) throw HeaderException("frame check failed")
        val r = ByteReader(bytes, 4)
        val headerLen = r.length()
        val h = ByteReader(bytes, r.pos, r.pos + headerLen)
        r.skip(headerLen)
        val batchId: ByteArray
        val sentAt: Long
        val sdk: String
        val anonId: String
        val userId: String
        val sessionId: ByteArray
        val context: List<DecodedProp>
        try {
            batchId = h.bytes(16)
            sentAt = h.varint()
            sdk = h.string(strictUtf8 = true)
            anonId = h.string(strictUtf8 = true)
            userId = h.string(strictUtf8 = true)
            sessionId = h.bytes(16)
            context = propList(h)
        } catch (e: ByteFormatException) {
            throw HeaderException(e.message ?: "bad header")
        }
        val records = ArrayList<DecodedRecord>()
        val bad = ArrayList<Int>()
        var index = 0
        while (r.remaining > 0) {
            val len = r.length()
            val b = ByteReader(bytes, r.pos, r.pos + len)
            r.skip(len)
            try {
                val kind = b.u8()
                if (kind <= 3) {
                    val uuid = b.bytes(16)
                    val ts = b.varint()
                    val name = b.string(strictUtf8 = true)
                    val props = propList(b)
                    records.add(DecodedRecord(kind, uuid, ts, name, props))
                }
                // unknown kinds are skipped, trailing bytes are ignored
            } catch (e: ByteFormatException) {
                bad.add(index)
            }
            index++
        }
        return DecodedBatch(batchId, sentAt, sdk, anonId, userId, sessionId, context, records, bad)
    }

    private fun propList(r: ByteReader): List<DecodedProp> {
        val count = r.length()
        val out = ArrayList<DecodedProp>()
        for (i in 0 until count) {
            val key = r.string(strictUtf8 = true)
            val tag = r.u8()
            val value: Any? = when (tag) {
                Wire.TAG_NULL -> null
                Wire.TAG_FALSE -> false
                Wire.TAG_TRUE -> true
                Wire.TAG_INT -> r.zigzag()
                Wire.TAG_FLOAT -> r.f64le()
                Wire.TAG_STRING, Wire.TAG_JSON -> r.string(strictUtf8 = true)
                else -> throw ByteFormatException("unknown tag $tag")
            }
            out.add(DecodedProp(key, tag, value))
        }
        return out
    }
}
