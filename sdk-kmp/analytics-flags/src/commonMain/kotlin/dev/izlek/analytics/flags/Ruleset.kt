package dev.izlek.analytics.flags

import dev.izlek.analytics.internal.Json

/** Parsed `flags/<token>.json` (spec/flags.md §1). Immutable. */
internal class Ruleset(val version: Long, val flags: Map<String, Flag>) {
    companion object {
        /** Returns null when [text] is not a ruleset document. Unknown fields are ignored. */
        fun parse(text: String): Ruleset? {
            val root = Json.parseOrNull(text) as? Map<*, *> ?: return null
            val list = root["flags"] as? List<*> ?: return null
            val version = (root["v"] as? Number)?.toLong() ?: 0L
            val flags = LinkedHashMap<String, Flag>()
            for (item in list) {
                val f = item as? Map<*, *> ?: continue
                val key = f["key"] as? String ?: continue
                flags[key] = Flag(
                    key = key,
                    enabled = f["enabled"] == true,
                    salt = f["salt"] as? String ?: "",
                    rules = (f["rules"] as? List<*>).orEmpty().mapNotNull { parseRule(it) },
                    default = normalizeDefault(f["default"]),
                )
            }
            return Ruleset(version, flags)
        }

        /** `false` ≡ `null`; anything that is not `true` or a string is treated as `null`. */
        private fun normalizeDefault(v: Any?): Any? = when (v) {
            true -> true
            is String -> v
            else -> null
        }

        private fun parseRule(v: Any?): Rule? {
            val r = v as? Map<*, *> ?: return null
            val whenValue = r["when"]
            val condition = if (whenValue == null) null else parseCond(whenValue, 0)
            val variants = (r["variants"] as? List<*>).orEmpty().mapNotNull { item ->
                val m = item as? Map<*, *> ?: return@mapNotNull null
                val key = m["key"] as? String ?: return@mapNotNull null
                Variant(key, (m["w"] as? Number)?.toDouble() ?: 0.0)
            }
            return Rule(
                condition = condition,
                rollout = (r["rollout"] as? Number)?.toDouble() ?: 10_000.0,
                variants = variants,
                stableOnAnonId = r["stable_on"] == "anon_id",
            )
        }

        private fun parseCond(v: Any?, depth: Int): Cond {
            val m = v as? Map<*, *> ?: return Cond.Never
            if (depth > 32) return Cond.Never
            // Same precedence as the reference implementation: and, or, not, then a property test.
            if (m.containsKey("and")) {
                val items = m["and"] as? List<*> ?: return Cond.Never
                return Cond.And(items.map { parseCond(it, depth + 1) })
            }
            if (m.containsKey("or")) {
                val items = m["or"] as? List<*> ?: return Cond.Never
                return Cond.Or(items.map { parseCond(it, depth + 1) })
            }
            if (m.containsKey("not")) return Cond.Not(parseCond(m["not"], depth + 1))
            val prop = m["prop"] as? String ?: return Cond.Never
            val op = m["op"] as? String ?: return Cond.Never
            return Cond.Test(prop, op, m["value"])
        }
    }
}

internal class Flag(
    val key: String,
    val enabled: Boolean,
    val salt: String,
    val rules: List<Rule>,
    /** `null`, `true` or a variant key. */
    val default: Any?,
)

internal class Rule(
    val condition: Cond?,
    val rollout: Double,
    val variants: List<Variant>,
    val stableOnAnonId: Boolean,
)

internal class Variant(val key: String, val weight: Double)

internal sealed class Cond {
    class And(val items: List<Cond>) : Cond()
    class Or(val items: List<Cond>) : Cond()
    class Not(val item: Cond) : Cond()
    class Test(val prop: String, val op: String, val value: Any?) : Cond()

    /** A malformed condition: never matches. */
    object Never : Cond()
}
