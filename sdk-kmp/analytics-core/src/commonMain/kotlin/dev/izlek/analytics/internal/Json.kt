package dev.izlek.analytics.internal

import dev.izlek.analytics.InternalIzlekApi

/**
 * Minimal JSON reader/writer (no kotlinx.serialization).
 *
 * Parsed model: `Map<String, Any?>` (insertion ordered), `List<Any?>`, `String`, `Boolean`,
 * `Long` (integers that fit), `Double` (everything else), `null`.
 */
@InternalIzlekApi
public object Json {
    private const val MAX_DEPTH = 64

    public class ParseException(message: String) : Exception(message)

    /** @throws ParseException on malformed input. */
    public fun parse(text: String): Any? {
        val p = Parser(text)
        p.skipWs()
        val v = p.value(0)
        p.skipWs()
        if (p.pos != text.length) throw ParseException("trailing characters at ${p.pos}")
        return v
    }

    public fun parseOrNull(text: String): Any? = try {
        parse(text)
    } catch (e: ParseException) {
        null
    }

    /** Compact JSON text. Unsupported values are written with `toString()`; NaN/Infinity become `null`. */
    public fun write(value: Any?): String {
        val sb = StringBuilder()
        writeTo(sb, value, 0)
        return sb.toString()
    }

    public fun writeTo(sb: StringBuilder, value: Any?, depth: Int) {
        if (depth > MAX_DEPTH) {
            sb.append("null")
            return
        }
        when (value) {
            null -> sb.append("null")
            is Boolean -> sb.append(if (value) "true" else "false")
            is String -> writeString(sb, value)
            is Byte, is Short, is Int, is Long -> sb.append(value.toString())
            is UByte, is UShort, is UInt, is ULong -> sb.append(value.toString())
            is Float -> writeDouble(sb, value.toDouble())
            is Double -> writeDouble(sb, value)
            is Number -> writeDouble(sb, value.toDouble())
            is Char -> writeString(sb, value.toString())
            is Map<*, *> -> {
                sb.append('{')
                var first = true
                for ((k, v) in value) {
                    if (!first) sb.append(',')
                    first = false
                    writeString(sb, k.toString())
                    sb.append(':')
                    writeTo(sb, v, depth + 1)
                }
                sb.append('}')
            }
            is Iterable<*> -> writeSeq(sb, value.iterator(), depth)
            is Sequence<*> -> writeSeq(sb, value.iterator(), depth)
            is Array<*> -> writeSeq(sb, value.iterator(), depth)
            is IntArray -> writeSeq(sb, value.iterator(), depth)
            is LongArray -> writeSeq(sb, value.iterator(), depth)
            is DoubleArray -> writeSeq(sb, value.iterator(), depth)
            is FloatArray -> writeSeq(sb, value.iterator(), depth)
            is BooleanArray -> writeSeq(sb, value.iterator(), depth)
            is ShortArray -> writeSeq(sb, value.iterator(), depth)
            is ByteArray -> writeSeq(sb, value.iterator(), depth)
            is Enum<*> -> writeString(sb, value.name)
            else -> writeString(sb, value.toString())
        }
    }

    private fun writeSeq(sb: StringBuilder, it: Iterator<*>, depth: Int) {
        sb.append('[')
        var first = true
        while (it.hasNext()) {
            if (!first) sb.append(',')
            first = false
            writeTo(sb, it.next(), depth + 1)
        }
        sb.append(']')
    }

    private fun writeDouble(sb: StringBuilder, d: Double) {
        if (d.isNaN() || d.isInfinite()) sb.append("null") else sb.append(d.toString())
    }

    public fun writeString(sb: StringBuilder, s: String) {
        sb.append('"')
        for (c in s) {
            when (c) {
                '"' -> sb.append("\\\"")
                '\\' -> sb.append("\\\\")
                '\n' -> sb.append("\\n")
                '\r' -> sb.append("\\r")
                '\t' -> sb.append("\\t")
                '\b' -> sb.append("\\b")
                '\u000C' -> sb.append("\\f")
                else -> if (c.code < 0x20) {
                    sb.append("\\u").append(c.code.toString(16).padStart(4, '0'))
                } else {
                    sb.append(c)
                }
            }
        }
        sb.append('"')
    }

    private class Parser(private val s: String) {
        var pos = 0

        fun skipWs() {
            while (pos < s.length) {
                val c = s[pos]
                if (c == ' ' || c == '\n' || c == '\r' || c == '\t') pos++ else break
            }
        }

        fun value(depth: Int): Any? {
            if (depth > MAX_DEPTH) throw ParseException("nesting too deep")
            if (pos >= s.length) throw ParseException("unexpected end")
            return when (val c = s[pos]) {
                '{' -> obj(depth)
                '[' -> arr(depth)
                '"' -> str()
                't' -> lit("true", true)
                'f' -> lit("false", false)
                'n' -> lit("null", null)
                else -> if (c == '-' || c in '0'..'9') num() else throw ParseException("unexpected '$c' at $pos")
            }
        }

        private fun lit(word: String, v: Any?): Any? {
            if (!s.startsWith(word, pos)) throw ParseException("bad literal at $pos")
            pos += word.length
            return v
        }

        private fun obj(depth: Int): Map<String, Any?> {
            val out = LinkedHashMap<String, Any?>()
            pos++
            skipWs()
            if (pos < s.length && s[pos] == '}') {
                pos++
                return out
            }
            while (true) {
                skipWs()
                if (pos >= s.length || s[pos] != '"') throw ParseException("expected key at $pos")
                val k = str()
                skipWs()
                if (pos >= s.length || s[pos] != ':') throw ParseException("expected ':' at $pos")
                pos++
                skipWs()
                out[k] = value(depth + 1)
                skipWs()
                if (pos >= s.length) throw ParseException("unexpected end")
                when (s[pos++]) {
                    ',' -> continue
                    '}' -> return out
                    else -> throw ParseException("expected ',' or '}' at ${pos - 1}")
                }
            }
        }

        private fun arr(depth: Int): List<Any?> {
            val out = ArrayList<Any?>()
            pos++
            skipWs()
            if (pos < s.length && s[pos] == ']') {
                pos++
                return out
            }
            while (true) {
                skipWs()
                out.add(value(depth + 1))
                skipWs()
                if (pos >= s.length) throw ParseException("unexpected end")
                when (s[pos++]) {
                    ',' -> continue
                    ']' -> return out
                    else -> throw ParseException("expected ',' or ']' at ${pos - 1}")
                }
            }
        }

        private fun str(): String {
            pos++ // opening quote
            val sb = StringBuilder()
            while (true) {
                if (pos >= s.length) throw ParseException("unterminated string")
                val c = s[pos++]
                when {
                    c == '"' -> return sb.toString()
                    c == '\\' -> {
                        if (pos >= s.length) throw ParseException("unterminated escape")
                        when (val e = s[pos++]) {
                            '"' -> sb.append('"')
                            '\\' -> sb.append('\\')
                            '/' -> sb.append('/')
                            'b' -> sb.append('\b')
                            'f' -> sb.append('\u000C')
                            'n' -> sb.append('\n')
                            'r' -> sb.append('\r')
                            't' -> sb.append('\t')
                            'u' -> {
                                if (pos + 4 > s.length) throw ParseException("bad \\u escape")
                                var code = 0
                                for (i in 0 until 4) {
                                    val d = when (val h = s[pos + i]) {
                                        in '0'..'9' -> h - '0'
                                        in 'a'..'f' -> h - 'a' + 10
                                        in 'A'..'F' -> h - 'A' + 10
                                        else -> throw ParseException("bad \\u escape")
                                    }
                                    code = (code shl 4) or d
                                }
                                pos += 4
                                sb.append(code.toChar())
                            }
                            else -> throw ParseException("bad escape '\\$e'")
                        }
                    }
                    c.code < 0x20 -> throw ParseException("control character in string")
                    else -> sb.append(c)
                }
            }
        }

        private fun num(): Any {
            val start = pos
            var integral = true
            if (s[pos] == '-') pos++
            val digitsStart = pos
            while (pos < s.length && s[pos] in '0'..'9') pos++
            if (pos == digitsStart) throw ParseException("bad number at $start")
            if (pos < s.length && s[pos] == '.') {
                integral = false
                pos++
                val f = pos
                while (pos < s.length && s[pos] in '0'..'9') pos++
                if (pos == f) throw ParseException("bad number at $start")
            }
            if (pos < s.length && (s[pos] == 'e' || s[pos] == 'E')) {
                integral = false
                pos++
                if (pos < s.length && (s[pos] == '+' || s[pos] == '-')) pos++
                val e = pos
                while (pos < s.length && s[pos] in '0'..'9') pos++
                if (pos == e) throw ParseException("bad number at $start")
            }
            val text = s.substring(start, pos)
            if (integral) {
                val l = text.toLongOrNull()
                if (l != null) return l
            }
            return text.toDoubleOrNull() ?: throw ParseException("bad number at $start")
        }
    }
}
