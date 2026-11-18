package dev.izlek.analytics.internal

import dev.izlek.analytics.InternalIzlekApi

/** LA1 wire encoding (`spec/wire.md`). Only the encoder ships in the SDK. */
@InternalIzlekApi
public object Wire {
    public val MAGIC: ByteArray = byteArrayOf(0x4C, 0x41, 0x31, 0x00)

    public const val KIND_EVENT: Int = 0
    public const val KIND_IDENTIFY: Int = 1
    public const val KIND_SET_PERSON: Int = 2
    public const val KIND_RESET: Int = 3

    public const val TAG_NULL: Int = 0
    public const val TAG_FALSE: Int = 1
    public const val TAG_TRUE: Int = 2
    public const val TAG_INT: Int = 3
    public const val TAG_FLOAT: Int = 4
    public const val TAG_STRING: Int = 5
    public const val TAG_JSON: Int = 6

    // Limits (spec/wire.md §3).
    public const val MAX_FRAME_BYTES: Int = 65536
    public const val MAX_BATCH_BYTES: Int = 1_048_576
    public const val MAX_BATCH_RECORDS: Int = 500
    public const val MAX_PROPS: Int = 200
    public const val MAX_KEY_BYTES: Int = 128
    public const val MAX_VALUE_BYTES: Int = 8192
    public const val MAX_NAME_BYTES: Int = 128
    public const val MAX_ID_BYTES: Int = 200

    /**
     * Writes one `Prop` (key, tag, value).
     *
     * Mapping from Kotlin values: `null`, `Boolean`, integral numbers → int64, floating point →
     * float64, `String`/`Char`/enum → string, maps/collections/arrays → `json`; anything else is
     * written as `toString()`.
     *
     * With [enforceLimits] the property is skipped (and `false` returned) when the key is longer
     * than 128 bytes or a string/json value is longer than 8192 bytes; the shipper would drop the
     * record or the property otherwise.
     */
    public fun writeProp(w: ByteWriter, key: String, value: Any?, enforceLimits: Boolean = false): Boolean {
        val k = key.encodeToByteArray()
        if (enforceLimits && k.size > MAX_KEY_BYTES) return false
        var text: ByteArray? = null
        val tag: Int
        var long = 0L
        var double = 0.0
        when (value) {
            null -> tag = TAG_NULL
            is Boolean -> tag = if (value) TAG_TRUE else TAG_FALSE
            is Long -> { tag = TAG_INT; long = value }
            is Int -> { tag = TAG_INT; long = value.toLong() }
            is Short -> { tag = TAG_INT; long = value.toLong() }
            is Byte -> { tag = TAG_INT; long = value.toLong() }
            is UInt -> { tag = TAG_INT; long = value.toLong() }
            is UShort -> { tag = TAG_INT; long = value.toLong() }
            is UByte -> { tag = TAG_INT; long = value.toLong() }
            is ULong -> if (value <= Long.MAX_VALUE.toULong()) {
                tag = TAG_INT; long = value.toLong()
            } else {
                tag = TAG_FLOAT; double = value.toDouble()
            }
            is Double -> { tag = TAG_FLOAT; double = value }
            is Float -> { tag = TAG_FLOAT; double = value.toDouble() }
            is Number -> { tag = TAG_FLOAT; double = value.toDouble() }
            is String -> { tag = TAG_STRING; text = value.encodeToByteArray() }
            is Char -> { tag = TAG_STRING; text = value.toString().encodeToByteArray() }
            is Enum<*> -> { tag = TAG_STRING; text = value.name.encodeToByteArray() }
            is JsonText -> { tag = TAG_JSON; text = value.text.encodeToByteArray() }
            is Map<*, *>, is Iterable<*>, is Sequence<*>, is Array<*>,
            is IntArray, is LongArray, is DoubleArray, is FloatArray, is BooleanArray, is ShortArray, is ByteArray,
            -> { tag = TAG_JSON; text = Json.write(value).encodeToByteArray() }
            else -> { tag = TAG_STRING; text = value.toString().encodeToByteArray() }
        }
        if (enforceLimits && text != null && text.size > MAX_VALUE_BYTES) return false
        w.varint(k.size.toLong())
        w.bytes(k)
        w.u8(tag)
        when (tag) {
            TAG_INT -> w.zigzag(long)
            TAG_FLOAT -> w.f64le(double)
            TAG_STRING, TAG_JSON -> {
                w.varint(text!!.size.toLong())
                w.bytes(text)
            }
        }
        return true
    }

    /** `PropList` from an ordered list of pairs (duplicate keys are written as given). */
    public fun writePropList(w: ByteWriter, props: List<Pair<String, Any?>>) {
        w.varint(props.size.toLong())
        for ((k, v) in props) writeProp(w, k, v)
    }

    /** `RecordBody` with an already encoded `PropList` tail ([propCount] props in [propBytes]). */
    public fun recordBody(kind: Int, uuid: ByteArray, ts: Long, name: String, propCount: Int, propBytes: ByteArray): ByteArray {
        require(uuid.size == 16)
        val w = ByteWriter(32 + name.length + propBytes.size)
        w.u8(kind)
        w.bytes(uuid)
        w.varint(ts)
        w.string(name)
        w.varint(propCount.toLong())
        w.bytes(propBytes)
        return w.toByteArray()
    }

    public fun recordBody(kind: Int, uuid: ByteArray, ts: Long, name: String, props: List<Pair<String, Any?>>): ByteArray {
        val p = ByteWriter(64)
        for ((k, v) in props) writeProp(p, k, v)
        return recordBody(kind, uuid, ts, name, props.size, p.toByteArray())
    }

    public fun headerBody(
        batchId: ByteArray,
        sentAt: Long,
        sdk: String,
        anonId: String,
        userId: String,
        sessionId: ByteArray,
        context: List<Pair<String, Any?>>,
    ): ByteArray {
        require(batchId.size == 16 && sessionId.size == 16)
        val w = ByteWriter(128)
        w.bytes(batchId)
        w.varint(sentAt)
        w.string(sdk)
        w.string(anonId)
        w.string(userId)
        w.bytes(sessionId)
        writePropList(w, context)
        return w.toByteArray()
    }

    /** `varint(len) body`. */
    public fun frame(body: ByteArray): ByteArray {
        val w = ByteWriter(body.size + 3)
        w.varint(body.size.toLong())
        w.bytes(body)
        return w.toByteArray()
    }

    /** A complete batch: `Magic Header Record*` where [frames] are already framed records. */
    public fun batch(headerBody: ByteArray, frames: ByteArray, offset: Int = 0, length: Int = frames.size - offset): ByteArray {
        val w = ByteWriter(MAGIC.size + headerBody.size + 3 + length)
        w.bytes(MAGIC)
        w.varint(headerBody.size.toLong())
        w.bytes(headerBody)
        w.bytes(frames, offset, length)
        return w.toByteArray()
    }
}

/** A property value that is already JSON text (array or object); written with the `json` tag as is. */
@InternalIzlekApi
public class JsonText(public val text: String)
