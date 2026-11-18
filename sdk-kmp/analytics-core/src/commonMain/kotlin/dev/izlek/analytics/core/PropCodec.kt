package dev.izlek.analytics.core

import dev.izlek.analytics.InternalIzlekApi
import dev.izlek.analytics.internal.ByteWriter
import dev.izlek.analytics.internal.Wire

/** Properties of one record, encoded on the caller's thread (`count` props in `bytes`). */
@InternalIzlekApi
public class EncodedProps(
    public val count: Int,
    public val bytes: ByteArray,
    /** Keys present in [bytes]; super properties with the same key are not written. */
    public val keys: Set<String>,
) {
    public companion object {
        public val EMPTY: EncodedProps = EncodedProps(0, ByteArray(0), emptySet())
    }
}

@InternalIzlekApi
public object PropCodec {
    /**
     * Encodes [props] as the tail of a `PropList`. Properties that break the limits of
     * `spec/wire.md` §3 (key > 128 bytes, string/json value > 8192 bytes) or whose value cannot be
     * converted are left out; at most 200 properties are kept.
     */
    public fun encode(props: Map<String, Any?>, extraKey: String? = null, extraValue: Any? = null): EncodedProps {
        if (props.isEmpty() && extraKey == null) return EncodedProps.EMPTY
        val w = ByteWriter(64 + props.size * 24)
        val keys = HashSet<String>(props.size + 1)
        var count = 0
        for ((k, v) in props) {
            if (count >= Wire.MAX_PROPS - (if (extraKey != null) 1 else 0)) break
            if (k == extraKey) continue
            val ok = try {
                Wire.writeProp(w, k, v, enforceLimits = true)
            } catch (e: Exception) {
                // A value whose toString()/iteration throws: skip it. The writer may hold a
                // partial property only if the failure happened inside Json.write, which runs
                // before anything is written.
                false
            }
            if (ok) {
                keys.add(k)
                count++
            }
        }
        if (extraKey != null && Wire.writeProp(w, extraKey, extraValue, enforceLimits = true)) {
            keys.add(extraKey)
            count++
        }
        return EncodedProps(count, w.toByteArray(), keys)
    }

    /**
     * Deep copy into plain immutable-by-convention values (`String`, `Boolean`, `Long`, `Double`,
     * `List`, `Map`, `null`) so that the SDK never touches caller-owned mutable objects from its
     * own thread. Used for person and super properties, which are kept in memory and on disk.
     */
    public fun normalize(props: Map<String, Any?>): Map<String, Any?> {
        val out = LinkedHashMap<String, Any?>(props.size)
        for ((k, v) in props) {
            try {
                out[k] = normalizeValue(v, 0)
            } catch (e: Exception) {
                // skip values that cannot be read
            }
        }
        return out
    }

    private fun normalizeValue(v: Any?, depth: Int): Any? {
        if (depth > 32) return null
        return when (v) {
            null -> null
            is Boolean, is String -> v
            is Long -> v
            is Int -> v.toLong()
            is Short -> v.toLong()
            is Byte -> v.toLong()
            is UInt -> v.toLong()
            is UShort -> v.toLong()
            is UByte -> v.toLong()
            is ULong -> if (v <= Long.MAX_VALUE.toULong()) v.toLong() else v.toDouble()
            is Double -> v
            is Float -> v.toDouble()
            is Number -> v.toDouble()
            is Char -> v.toString()
            is Enum<*> -> v.name
            is Map<*, *> -> {
                val m = LinkedHashMap<String, Any?>(v.size)
                for ((k, x) in v) m[k.toString()] = normalizeValue(x, depth + 1)
                m
            }
            is Iterable<*> -> v.map { normalizeValue(it, depth + 1) }
            is Sequence<*> -> v.map { normalizeValue(it, depth + 1) }.toList()
            is Array<*> -> v.map { normalizeValue(it, depth + 1) }
            is IntArray -> v.map { it.toLong() }
            is LongArray -> v.toList()
            is ShortArray -> v.map { it.toLong() }
            is ByteArray -> v.map { it.toLong() }
            is DoubleArray -> v.toList()
            is FloatArray -> v.map { it.toDouble() }
            is BooleanArray -> v.toList()
            else -> v.toString()
        }
    }
}
