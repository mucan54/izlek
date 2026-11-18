package dev.izlek.analytics.flags

import dev.izlek.analytics.internal.Json
import dev.izlek.analytics.internal.Murmur3
import dev.izlek.analytics.testkit.VectorJson
import dev.izlek.analytics.testkit.VectorJson.list
import dev.izlek.analytics.testkit.VectorJson.obj
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNotNull
import kotlin.test.assertNull
import kotlin.test.assertTrue

/** spec/vectors/flags.json: every case must evaluate to the expected value. */
class FlagVectorTest {

    @Test
    fun allVectorCasesEvaluateAsExpected() {
        val doc = VectorJson.flags()
        val ruleset = assertNotNull(Ruleset.parse(Json.write(doc["ruleset"])))
        assertEquals(31, ruleset.version)
        val cases = list(doc["cases"]).map { obj(it) }
        assertTrue(cases.size >= 117, "flags.json cases present: ${cases.size}")
        var checked = 0
        for ((i, c) in cases.withIndex()) {
            val flag = assertNotNull(ruleset.flags[c["flag"] as String], "case $i: flag exists")
            val actual = FlagEvaluator.evaluate(flag, obj(c["props"]), c["user_id"] as String, c["anon_id"] as String)
            assertEquals(c["expect"], actual, "case $i: ${Json.write(c)}")
            checked++
        }
        assertEquals(cases.size, checked)
    }

    /** Applications pass Int/Float/etc., not the Long/Double a JSON parser produces. */
    @Test
    fun vectorCasesHoldForOtherNumericTypes() {
        val doc = VectorJson.flags()
        val ruleset = Ruleset.parse(Json.write(doc["ruleset"]))!!
        for (c in list(doc["cases"]).map { obj(it) }) {
            val props = obj(c["props"]).mapValues { (_, v) ->
                when {
                    v is Long && v in Int.MIN_VALUE..Int.MAX_VALUE -> v.toInt()
                    v is Double && v.toFloat().toDouble() == v -> v.toFloat()
                    else -> v
                }
            }
            val actual = FlagEvaluator.evaluate(ruleset.flags[c["flag"] as String]!!, props, c["user_id"] as String, c["anon_id"] as String)
            assertEquals(c["expect"], actual, Json.write(c))
        }
    }

    /** `b >= rollout` excludes: the bucket equal to the rollout value is the first one left out. */
    @Test
    fun rolloutBoundaryIsExclusive() {
        val ruleset = Ruleset.parse(
            """{"v":1,"flags":[
                {"key":"f","enabled":true,"salt":"s","rules":[{"rollout":5000}],"default":"out"},
                {"key":"v","enabled":true,"salt":"s","rules":[{"rollout":10000,"variants":[{"key":"a","w":5000},{"key":"b","w":5000}]}],"default":null}
            ]}""",
        )!!
        fun idWithBucket(prefix: String, bucket: Int): String =
            (0 until 2_000_000).asSequence().map { "id-$it" }.first { Murmur3.bucket(prefix + it) == bucket }
        val f = ruleset.flags["f"]!!
        assertEquals(true, FlagEvaluator.evaluate(f, emptyMap(), "", idWithBucket("s:f:", 4999)))
        assertEquals("out", FlagEvaluator.evaluate(f, emptyMap(), "", idWithBucket("s:f:", 5000)))
        assertEquals(true, FlagEvaluator.evaluate(f, emptyMap(), "", idWithBucket("s:f:", 0)))
        assertEquals("out", FlagEvaluator.evaluate(f, emptyMap(), "", idWithBucket("s:f:", 9999)))
        // variant ranges: [0, 5000) → a, [5000, 10000) → b
        val v = ruleset.flags["v"]!!
        assertEquals("a", FlagEvaluator.evaluate(v, emptyMap(), "", idWithBucket("s:v:v:", 4999)))
        assertEquals("b", FlagEvaluator.evaluate(v, emptyMap(), "", idWithBucket("s:v:v:", 5000)))
    }

    private fun eval(cond: String, props: Map<String, Any?>): Boolean {
        val ruleset = Ruleset.parse("""{"v":1,"flags":[{"key":"f","enabled":true,"salt":"s","rules":[{"when":$cond}],"default":null}]}""")!!
        return FlagEvaluator.evaluate(ruleset.flags["f"]!!, props, "", "anon") == true
    }

    @Test
    fun operatorEdgeCases() {
        // missing / null property
        for (op in listOf("eq", "in", "gt", "gte", "lt", "lte", "contains", "regex", "is_set", "semver_gte", "semver_lt")) {
            assertFalse(eval("""{"prop":"x","op":"$op","value":"1"}""", emptyMap()), op)
            assertFalse(eval("""{"prop":"x","op":"$op","value":"1"}""", mapOf("x" to null)), op)
        }
        assertTrue(eval("""{"prop":"x","op":"ne","value":1}""", emptyMap()))
        assertTrue(eval("""{"prop":"x","op":"not_in","value":[1]}""", emptyMap()))
        assertFalse(eval("""{"prop":"x","op":"unknown_op","value":1}""", mapOf("x" to 1)))

        // loose equality
        assertTrue(eval("""{"prop":"x","op":"eq","value":3}""", mapOf("x" to 3.0)))
        assertTrue(eval("""{"prop":"x","op":"eq","value":"3"}""", mapOf("x" to 3.0)))
        assertTrue(eval("""{"prop":"x","op":"eq","value":"true"}""", mapOf("x" to true)))
        assertTrue(eval("""{"prop":"x","op":"eq","value":true}""", mapOf("x" to "true")))
        assertFalse(eval("""{"prop":"x","op":"eq","value":1}""", mapOf("x" to true)))
        assertTrue(eval("""{"prop":"x","op":"eq","value":"1.5"}""", mapOf("x" to 1.5)))
        assertFalse(eval("""{"prop":"x","op":"eq","value":"03"}""", mapOf("x" to 3)))
        assertTrue(eval("""{"prop":"x","op":"in","value":["a",2]}""", mapOf("x" to "2")))
        assertFalse(eval("""{"prop":"x","op":"in","value":"not-a-list"}""", mapOf("x" to "n")))
        assertTrue(eval("""{"prop":"x","op":"not_in","value":"not-a-list"}""", mapOf("x" to "n")))

        // numeric comparison: plain decimals only, booleans are not numbers
        assertTrue(eval("""{"prop":"x","op":"gt","value":"9.5"}""", mapOf("x" to "10")))
        assertTrue(eval("""{"prop":"x","op":"lte","value":-1}""", mapOf("x" to "-1.0")))
        assertFalse(eval("""{"prop":"x","op":"gt","value":0}""", mapOf("x" to "1e3")))
        assertFalse(eval("""{"prop":"x","op":"gt","value":0}""", mapOf("x" to " 5")))
        assertFalse(eval("""{"prop":"x","op":"gt","value":0}""", mapOf("x" to ".5")))
        assertFalse(eval("""{"prop":"x","op":"gte","value":0}""", mapOf("x" to true)))
        assertFalse(eval("""{"prop":"x","op":"lt","value":true}""", mapOf("x" to 0)))

        // contains / semver
        assertTrue(eval("""{"prop":"x","op":"contains","value":5}""", mapOf("x" to 1500)))
        assertFalse(eval("""{"prop":"x","op":"contains","value":"ALI"}""", mapOf("x" to "khalid")))
        assertTrue(eval("""{"prop":"x","op":"semver_gte","value":"3.2"}""", mapOf("x" to "V3.2.0+build.5")))
        assertTrue(eval("""{"prop":"x","op":"semver_gte","value":"3.9.9"}""", mapOf("x" to "3.10")))
        assertTrue(eval("""{"prop":"x","op":"semver_lt","value":"10.0.0"}""", mapOf("x" to "9.99.99")))
        assertTrue(eval("""{"prop":"x","op":"semver_gte","value":2}""", mapOf("x" to 3)))
        assertFalse(eval("""{"prop":"x","op":"semver_gte","value":"1.0"}""", mapOf("x" to "1.2.3.4")))
        assertFalse(eval("""{"prop":"x","op":"semver_lt","value":"1.0"}""", mapOf("x" to "1..2")))
        assertTrue(eval("""{"prop":"x","op":"semver_gte","value":"99999999999999999999.0.0"}""", mapOf("x" to "100000000000000000000")))

        // virtual keys
        assertTrue(eval("""{"prop":"anon_id","op":"eq","value":"anon"}""", emptyMap()))
        assertFalse(eval("""{"prop":"user_id","op":"is_set"}""", mapOf("user_id" to "shadowed-by-virtual-key")))

        // malformed conditions never match
        assertFalse(eval("""{"and":"nope"}""", emptyMap()))
        assertFalse(eval("""{"prop":"x"}""", mapOf("x" to 1)))
        assertFalse(eval(""""string"""", emptyMap()))
        assertTrue(eval("""{"not":{"prop":"x"}}""", mapOf("x" to 1)))
    }

    /** Only the common subset of spec/flags.md §4 is used here; it must behave the same on every target. */
    @Test
    fun regexCommonSubset() {
        fun re(pattern: String, input: Any?) =
            eval("""{"prop":"x","op":"regex","value":${Json.write(pattern)}}""", mapOf("x" to input))
        assertTrue(re("@example\\.com$", "a@example.com"))
        assertFalse(re("@example\\.com$", "a@example.com.tr"))
        assertTrue(re("^ab+c?$", "abbb"))
        assertFalse(re("^ab+c?$", "ac"))
        assertTrue(re("b{2,3}", "abbbb"))
        assertFalse(re("^b{2,3}$", "bbbb"))
        assertTrue(re("^(tr|de)-[A-Z]{2}$", "tr-TR"))
        assertFalse(re("^(tr|de)-[A-Z]{2}$", "en-US"))
        assertTrue(re("[0-9]+\\.[0-9]*", "v3.14"))
        assertTrue(re("^[^@ ]+@[a-z.]+$", "user@mail.example"))
        assertTrue(re("a.c", "xxabcxx"), "unanchored search")
        assertFalse(re("a.c", "ac"))
        assertTrue(re("^1[05]$", 15), "numbers are matched through str()")
        assertTrue(re("çğ+ı", "açğğı"))
        // invalid patterns never match (and never throw)
        assertFalse(re("(", "("))
        assertFalse(re("[a-", "a"))
        assertFalse(re("*a", "a"))
    }

    @Test
    fun strFormatting() {
        assertEquals("3", FlagEvaluator.toStr(3.0))
        assertEquals("-3", FlagEvaluator.toStr(-3.0f))
        assertEquals("0", FlagEvaluator.toStr(-0.0))
        assertEquals("1.5", FlagEvaluator.toStr(1.5))
        assertEquals("0.001", FlagEvaluator.toStr(0.001))
        assertEquals("0.0001", FlagEvaluator.toStr(0.0001))
        assertEquals("1.5e-05", FlagEvaluator.toStr(0.000015))
        assertEquals("1e-07", FlagEvaluator.toStr(1e-7))
        assertEquals("-1234567.125", FlagEvaluator.toStr(-1234567.125))
        assertEquals("9007199254740993", FlagEvaluator.toStr(9007199254740993L), "longs keep full precision")
        assertEquals("100000000000000000000", FlagEvaluator.toStr(1e20))
        assertEquals("99999999999999991611392", FlagEvaluator.toStr(1e23), "exact value, no exponent")
        assertEquals("-9223372036854775808", FlagEvaluator.toStr(-9.223372036854775808E18))
        assertEquals("true", FlagEvaluator.toStr(true))
        assertEquals("""[1,"a"]""", FlagEvaluator.toStr(listOf(1, "a")))
    }

    @Test
    fun rulesetParsing() {
        assertNull(Ruleset.parse("not json"))
        assertNull(Ruleset.parse("[]"))
        assertNull(Ruleset.parse("""{"v":1}"""))
        val r = Ruleset.parse(
            """{"v":7,"future_field":1,"flags":[
                {"key":"a","enabled":true,"salt":"s","default":false,"rules":[{"rollout":10000,"new_rule_field":true}]},
                {"key":"b","enabled":false,"default":true},
                {"no_key":true}, 42
            ]}""",
        )!!
        assertEquals(7, r.version)
        assertEquals(setOf("a", "b"), r.flags.keys)
        assertNull(r.flags["a"]!!.default, "false ≡ null")
        assertEquals(true, FlagEvaluator.evaluate(r.flags["a"]!!, emptyMap(), "", "x"))
        assertEquals(true, FlagEvaluator.evaluate(r.flags["b"]!!, emptyMap(), "", "x"), "disabled → default")
    }
}
