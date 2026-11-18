package dev.izlek.analytics.flags

import dev.izlek.analytics.internal.Json
import dev.izlek.analytics.internal.Murmur3
import kotlin.math.abs
import kotlin.math.floor

/**
 * Local flag evaluation, exactly as `spec/flags.md` §3–§4 (reference: `spec/vectors/gen.py`,
 * vectors: `spec/vectors/flags.json`).
 */
internal object FlagEvaluator {

    /** Returns `null`, `true` or a variant key. */
    fun evaluate(flag: Flag, props: Map<String, Any?>, userId: String, anonId: String): Any? {
        if (!flag.enabled) return flag.default
        val lookup: (String) -> Any? = { key ->
            when (key) {
                "user_id" -> userId.ifEmpty { null }
                "anon_id" -> anonId
                else -> props[key]
            }
        }
        for (rule in flag.rules) {
            if (rule.condition != null && !matches(rule.condition, lookup)) continue
            val id = if (rule.stableOnAnonId) anonId else userId.ifEmpty { anonId }
            val bucket = Murmur3.bucket(flag.salt + ":" + flag.key + ":" + id)
            if (bucket >= rule.rollout) return flag.default // the first matching rule decides
            if (rule.variants.isEmpty()) return true
            val v = Murmur3.bucket(flag.salt + ":v:" + flag.key + ":" + id)
            var cumulative = 0.0
            for (variant in rule.variants) {
                cumulative += variant.weight
                if (v < cumulative) return variant.key
            }
            return rule.variants.last().key // weights do not add up to 10000
        }
        return flag.default
    }

    fun matches(cond: Cond, lookup: (String) -> Any?): Boolean = when (cond) {
        is Cond.And -> cond.items.all { matches(it, lookup) }
        is Cond.Or -> cond.items.any { matches(it, lookup) }
        is Cond.Not -> !matches(cond.item, lookup)
        is Cond.Test -> test(lookup(cond.prop), cond.op, cond.value)
        Cond.Never -> false
    }

    private fun test(p: Any?, op: String, v: Any?): Boolean {
        if (p == null) return op == "ne" || op == "not_in"
        return when (op) {
            "is_set" -> true
            "eq" -> looseEq(p, v)
            "ne" -> !looseEq(p, v)
            "in" -> v is List<*> && v.any { looseEq(p, it) }
            "not_in" -> !(v is List<*> && v.any { looseEq(p, it) })
            "gt", "gte", "lt", "lte" -> {
                val a = toNum(p)
                val b = toNum(v)
                if (a == null || b == null) {
                    false
                } else {
                    when (op) {
                        "gt" -> a > b
                        "gte" -> a >= b
                        "lt" -> a < b
                        else -> a <= b
                    }
                }
            }
            "contains" -> toStr(p).contains(toStr(v))
            "regex" -> regexFind(toStr(v), toStr(p))
            "semver_gte", "semver_lt" -> {
                val a = semver(p)
                val b = semver(v)
                if (a == null || b == null) {
                    false
                } else {
                    val c = compareSemver(a, b)
                    if (op == "semver_gte") c >= 0 else c < 0
                }
            }
            else -> false
        }
    }

    // --- value coercions (spec/flags.md §4) ------------------------------------------------------

    private fun isNum(x: Any?): Boolean = x is Number || x is UInt || x is ULong || x is UShort || x is UByte

    private fun numValue(x: Any?): Double = when (x) {
        is Number -> x.toDouble()
        is UInt -> x.toDouble()
        is ULong -> x.toDouble()
        is UShort -> x.toDouble()
        is UByte -> x.toDouble()
        else -> Double.NaN
    }

    private fun isNumericString(s: String): Boolean {
        // ^-?[0-9]+(\.[0-9]+)?$
        var i = 0
        if (i < s.length && s[i] == '-') i++
        val intStart = i
        while (i < s.length && s[i] in '0'..'9') i++
        if (i == intStart) return false
        if (i == s.length) return true
        if (s[i] != '.') return false
        i++
        val fracStart = i
        while (i < s.length && s[i] in '0'..'9') i++
        return i > fracStart && i == s.length
    }

    /** Number, or a string that looks like a plain decimal; booleans are not numbers. */
    fun toNum(x: Any?): Double? = when {
        isNum(x) -> numValue(x)
        x is String && isNumericString(x) -> x.toDoubleOrNull()
        else -> null
    }

    private fun looseEq(p: Any?, v: Any?): Boolean =
        if (isNum(p) && isNum(v)) numValue(p) == numValue(v) else toStr(p) == toStr(v)

    /**
     * `str(x)`: strings as they are; booleans as `true`/`false`; integer-valued numbers as plain
     * decimal digits; other numbers in their shortest round-trip form.
     */
    fun toStr(x: Any?): String = when {
        x is String -> x
        x is Boolean -> if (x) "true" else "false"
        x is Long || x is Int || x is Short || x is Byte -> x.toString()
        x is UInt || x is ULong || x is UShort || x is UByte -> x.toString()
        isNum(x) -> doubleToStr(numValue(x))
        else -> Json.write(x)
    }

    private const val TWO_POW_63 = 9.223372036854775807E18

    internal fun doubleToStr(d: Double): String {
        if (d.isNaN() || d.isInfinite()) return d.toString()
        if (d == floor(d)) {
            return if (abs(d) < TWO_POW_63) d.toLong().toString() else exactIntegerString(d)
        }
        return shortestDecimal(d)
    }

    /** Exact decimal expansion of an integer-valued double with magnitude ≥ 2^63. */
    private fun exactIntegerString(d: Double): String {
        val bits = d.toBits()
        val exponent = ((bits ushr 52) and 0x7FF).toInt() - 1075
        var mantissa = (bits and 0xFFFFFFFFFFFFFL) or 0x10000000000000L
        // decimal digits, least significant first
        val digits = ArrayList<Int>()
        while (mantissa != 0L) {
            digits.add((mantissa % 10).toInt())
            mantissa /= 10
        }
        repeat(exponent) {
            var carry = 0
            for (i in digits.indices) {
                val x = digits[i] * 2 + carry
                digits[i] = x % 10
                carry = x / 10
            }
            if (carry != 0) digits.add(carry)
        }
        val sb = StringBuilder()
        if (d < 0) sb.append('-')
        for (i in digits.indices.reversed()) sb.append(digits[i])
        return sb.toString()
    }

    /**
     * Re-formats the platform's shortest representation (`1.5E-5`, `0.001`, `123.456`) the way
     * the reference implementation prints floats: positional notation for exponents in
     * (-5, 16), otherwise `d.ddde-XX`.
     */
    private fun shortestDecimal(d: Double): String {
        val text = abs(d).toString()
        val ePos = text.indexOfFirst { it == 'E' || it == 'e' }
        val mantissa = if (ePos >= 0) text.substring(0, ePos) else text
        var exp10 = if (ePos >= 0) text.substring(ePos + 1).toInt() else 0
        val dot = mantissa.indexOf('.')
        var digits = if (dot >= 0) mantissa.substring(0, dot) + mantissa.substring(dot + 1) else mantissa
        // value = 0.<digits> × 10^pointPos
        var pointPos = (if (dot >= 0) dot else mantissa.length) + exp10
        val leading = digits.indexOfFirst { it != '0' }
        if (leading > 0) {
            digits = digits.substring(leading)
            pointPos -= leading
        }
        digits = digits.trimEnd('0')
        exp10 = pointPos - 1
        val sb = StringBuilder()
        if (d < 0) sb.append('-')
        if (exp10 < -4 || exp10 >= 16) {
            sb.append(digits[0])
            if (digits.length > 1) sb.append('.').append(digits, 1, digits.length)
            sb.append('e').append(if (exp10 < 0) '-' else '+')
            sb.append(abs(exp10).toString().padStart(2, '0'))
        } else if (pointPos <= 0) {
            sb.append("0.")
            repeat(-pointPos) { sb.append('0') }
            sb.append(digits)
        } else if (pointPos >= digits.length) {
            sb.append(digits)
            repeat(pointPos - digits.length) { sb.append('0') }
            sb.append(".0")
        } else {
            sb.append(digits, 0, pointPos).append('.').append(digits, pointPos, digits.length)
        }
        return sb.toString()
    }

    // --- regex ---------------------------------------------------------------------------------------

    /** Unanchored search; an invalid pattern never matches. */
    private fun regexFind(pattern: String, input: String): Boolean {
        val regex = cachedRegex(pattern) ?: return false
        return try {
            regex.containsMatchIn(input)
        } catch (t: Throwable) {
            false
        }
    }

    private fun cachedRegex(pattern: String): Regex? {
        val snapshot = regexSnapshot
        if (snapshot.containsKey(pattern)) return snapshot[pattern]
        val compiled = try {
            Regex(pattern)
        } catch (t: Throwable) {
            null
        }
        // Copy-on-write so that concurrent readers never see a map being modified.
        if (snapshot.size < 64) regexSnapshot = snapshot + (pattern to compiled)
        return compiled
    }

    @kotlin.concurrent.Volatile
    private var regexSnapshot: Map<String, Regex?> = emptyMap()

    // --- semver ----------------------------------------------------------------------------------------

    /** `MAJOR[.MINOR[.PATCH]]` as digit strings without leading zeros, or null. */
    private fun semver(x: Any?): List<String>? {
        var s = toStr(x).trim()
        if (s.startsWith('v') || s.startsWith('V')) s = s.substring(1)
        val cut = s.indexOfFirst { it == '-' || it == '+' }
        if (cut >= 0) s = s.substring(0, cut)
        val parts = s.split('.')
        if (parts.size !in 1..3) return null
        val out = ArrayList<String>(3)
        for (p in parts) {
            if (p.isEmpty() || !p.all { it in '0'..'9' }) return null
            out.add(p.trimStart('0').ifEmpty { "0" })
        }
        while (out.size < 3) out.add("0")
        return out
    }

    private fun compareSemver(a: List<String>, b: List<String>): Int {
        for (i in 0 until 3) {
            val x = a[i]
            val y = b[i]
            if (x.length != y.length) return x.length.compareTo(y.length)
            val c = x.compareTo(y)
            if (c != 0) return c
        }
        return 0
    }
}
