package dev.izlek.analytics.internal

import dev.izlek.analytics.InternalIzlekApi

/** Growable byte buffer with the primitive encodings of `spec/wire.md` §1. */
@InternalIzlekApi
public class ByteWriter(initialCapacity: Int = 256) {
    private var buf = ByteArray(if (initialCapacity < 16) 16 else initialCapacity)

    public var size: Int = 0
        private set

    private fun ensure(extra: Int) {
        val need = size + extra
        if (need > buf.size) buf = buf.copyOf(maxOf(buf.size * 2, need))
    }

    public fun u8(v: Int) {
        ensure(1)
        buf[size++] = v.toByte()
    }

    public fun bytes(src: ByteArray, offset: Int = 0, length: Int = src.size - offset) {
        if (length <= 0) return
        ensure(length)
        src.copyInto(buf, size, offset, offset + length)
        size += length
    }

    /** Unsigned LEB128, shortest form. The value is treated as `u64`. */
    public fun varint(v: Long) {
        ensure(10)
        var x = v
        while (true) {
            val b = (x and 0x7F).toInt()
            x = x ushr 7
            if (x != 0L) {
                buf[size++] = (b or 0x80).toByte()
            } else {
                buf[size++] = b.toByte()
                return
            }
        }
    }

    public fun zigzag(v: Long): Unit = varint((v shl 1) xor (v shr 63))

    public fun string(s: String) {
        val b = s.encodeToByteArray()
        varint(b.size.toLong())
        bytes(b)
    }

    /** IEEE-754 little-endian; NaN is written in its canonical form. */
    public fun f64le(d: Double) {
        val bits = d.toBits()
        ensure(8)
        for (i in 0 until 8) buf[size++] = (bits ushr (8 * i)).toByte()
    }

    public fun u32le(v: Int) {
        ensure(4)
        for (i in 0 until 4) buf[size++] = (v ushr (8 * i)).toByte()
    }

    public fun toByteArray(): ByteArray = buf.copyOf(size)
}

/** Bounds-checked reader. All read methods throw [ByteFormatException] on malformed input. */
@InternalIzlekApi
public class ByteReader(private val buf: ByteArray, public var pos: Int = 0, private val end: Int = buf.size) {

    public val remaining: Int get() = end - pos

    public fun u8(): Int {
        if (pos >= end) throw ByteFormatException("unexpected end")
        return buf[pos++].toInt() and 0xFF
    }

    /** Accepts non-minimal encodings; rejects more than 10 bytes or values above `u64`. */
    public fun varint(): Long {
        var result = 0L
        var shift = 0
        var count = 0
        while (true) {
            if (pos >= end) throw ByteFormatException("unterminated varint")
            val b = buf[pos++].toInt() and 0xFF
            count++
            if (count > 10) throw ByteFormatException("varint too long")
            if (count == 10 && b > 1) throw ByteFormatException("varint overflows u64")
            result = result or ((b and 0x7F).toLong() shl shift)
            if (b and 0x80 == 0) return result
            shift += 7
        }
    }

    public fun zigzag(): Long {
        val v = varint()
        return (v ushr 1) xor -(v and 1)
    }

    /** A varint that must fit a non-negative Int (lengths, counts). */
    public fun length(): Int {
        val v = varint()
        if (v < 0 || v > Int.MAX_VALUE) throw ByteFormatException("length out of range")
        return v.toInt()
    }

    public fun bytes(n: Int): ByteArray {
        if (n < 0 || n > remaining) throw ByteFormatException("unexpected end")
        val out = buf.copyOfRange(pos, pos + n)
        pos += n
        return out
    }

    public fun skip(n: Int) {
        if (n < 0 || n > remaining) throw ByteFormatException("unexpected end")
        pos += n
    }

    public fun string(strictUtf8: Boolean = false): String {
        val n = length()
        if (n > remaining) throw ByteFormatException("string overruns buffer")
        val s = try {
            buf.decodeToString(pos, pos + n, throwOnInvalidSequence = strictUtf8)
        } catch (e: CharacterCodingException) {
            throw ByteFormatException("invalid utf-8")
        }
        pos += n
        return s
    }

    public fun f64le(): Double {
        if (remaining < 8) throw ByteFormatException("unexpected end")
        var bits = 0L
        for (i in 0 until 8) bits = bits or ((buf[pos + i].toLong() and 0xFF) shl (8 * i))
        pos += 8
        return Double.fromBits(bits)
    }

    public fun u32le(): Int {
        if (remaining < 4) throw ByteFormatException("unexpected end")
        var v = 0
        for (i in 0 until 4) v = v or ((buf[pos + i].toInt() and 0xFF) shl (8 * i))
        pos += 4
        return v
    }
}

@InternalIzlekApi
public class ByteFormatException(message: String) : Exception(message)

private const val HEX = "0123456789abcdef"

@InternalIzlekApi
public fun ByteArray.toHex(): String {
    val out = CharArray(size * 2)
    for (i in indices) {
        val v = this[i].toInt() and 0xFF
        out[i * 2] = HEX[v ushr 4]
        out[i * 2 + 1] = HEX[v and 0x0F]
    }
    return out.concatToString()
}

/** Returns null when [this] is not an even-length hex string. */
@InternalIzlekApi
public fun String.hexToBytesOrNull(): ByteArray? {
    if (length % 2 != 0) return null
    val out = ByteArray(length / 2)
    for (i in out.indices) {
        val hi = hexDigit(this[i * 2])
        val lo = hexDigit(this[i * 2 + 1])
        if (hi < 0 || lo < 0) return null
        out[i] = ((hi shl 4) or lo).toByte()
    }
    return out
}

private fun hexDigit(c: Char): Int = when (c) {
    in '0'..'9' -> c - '0'
    in 'a'..'f' -> c - 'a' + 10
    in 'A'..'F' -> c - 'A' + 10
    else -> -1
}
