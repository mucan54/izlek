package dev.izlek.analytics.internal

import dev.izlek.analytics.InternalIzlekApi

/** MurmurHash3 x86_32 (`spec/flags.md` §3, vectors: `spec/vectors/murmur3.json`). */
@InternalIzlekApi
public object Murmur3 {
    private const val C1 = -0x3361d2af // 0xcc9e2d51
    private const val C2 = 0x1b873593

    /** The hash as a signed Int holding the unsigned 32-bit result. */
    public fun hash32(data: ByteArray, seed: Int = 0): Int {
        var h = seed
        val n = data.size
        val blocks = n / 4
        for (i in 0 until blocks) {
            val o = i * 4
            var k = (data[o].toInt() and 0xFF) or
                ((data[o + 1].toInt() and 0xFF) shl 8) or
                ((data[o + 2].toInt() and 0xFF) shl 16) or
                ((data[o + 3].toInt() and 0xFF) shl 24)
            k *= C1
            k = k.rotateLeft(15)
            k *= C2
            h = h xor k
            h = h.rotateLeft(13)
            h = h * 5 + -0x19ab949c // 0xe6546b64
        }
        val t = blocks * 4
        var k = 0
        val rem = n and 3
        if (rem >= 3) k = k xor ((data[t + 2].toInt() and 0xFF) shl 16)
        if (rem >= 2) k = k xor ((data[t + 1].toInt() and 0xFF) shl 8)
        if (rem >= 1) {
            k = k xor (data[t].toInt() and 0xFF)
            k *= C1
            k = k.rotateLeft(15)
            k *= C2
            h = h xor k
        }
        h = h xor n
        h = h xor (h ushr 16)
        h *= -0x7a143595 // 0x85ebca6b
        h = h xor (h ushr 13)
        h *= -0x3d4d51cb // 0xc2b2ae35
        h = h xor (h ushr 16)
        return h
    }

    /** Unsigned 32-bit hash of the UTF-8 bytes of [text]. */
    public fun hash32(text: String): Long = hash32(text.encodeToByteArray()).toLong() and 0xFFFFFFFFL

    /** `murmur3_32(utf8(text)) mod 10000`. */
    public fun bucket(text: String): Int = (hash32(text) % 10000L).toInt()
}
