package dev.izlek.analytics

import dev.izlek.analytics.core.PropCodec
import dev.izlek.analytics.internal.ByteWriter
import dev.izlek.analytics.internal.Json
import dev.izlek.analytics.internal.JsonText
import dev.izlek.analytics.internal.Murmur3
import dev.izlek.analytics.internal.Wire
import dev.izlek.analytics.internal.hexToBytesOrNull
import dev.izlek.analytics.internal.toHex
import dev.izlek.analytics.testkit.DecodedBatch
import dev.izlek.analytics.testkit.DecodedProp
import dev.izlek.analytics.testkit.VectorJson
import dev.izlek.analytics.testkit.VectorJson.list
import dev.izlek.analytics.testkit.VectorJson.obj
import dev.izlek.analytics.testkit.WireDecoder
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertTrue

/** spec/vectors/wire.json and murmur3.json (the JSON is embedded at build time; see analytics-testkit). */
class WireVectorTest {

    private fun hex(v: Any?): ByteArray = (v as String).hexToBytesOrNull()!!

    /** Vector property → Kotlin value as an application would pass it. */
    private fun value(p: Map<String, Any?>, jsonAsStructure: Boolean): Any? = when (p["t"]) {
        "null" -> null
        "bool" -> p["v"] as Boolean
        "int" -> (p["v"] as String).toLong()
        "float" -> when (val s = p["v"] as String) {
            "NaN" -> Double.NaN
            "Infinity" -> Double.POSITIVE_INFINITY
            "-Infinity" -> Double.NEGATIVE_INFINITY
            else -> s.toDouble()
        }
        "string" -> p["v"] as String
        "json" -> if (jsonAsStructure) Json.parse(p["v"] as String) else JsonText(p["v"] as String)
        else -> error("unknown type ${p["t"]}")
    }

    private fun props(v: Any?, jsonAsStructure: Boolean): List<Pair<String, Any?>> =
        list(v).map { obj(it) }.map { (it["k"] as String) to value(it, jsonAsStructure) }

    private fun encode(batch: Map<String, Any?>, jsonAsStructure: Boolean): ByteArray {
        val header = Wire.headerBody(
            batchId = hex(batch["batch_id"]),
            sentAt = batch["sent_at"] as Long,
            sdk = batch["sdk"] as String,
            anonId = batch["anon_id"] as String,
            userId = batch["user_id"] as String,
            sessionId = hex(batch["session_id"]),
            context = props(batch["context"], jsonAsStructure),
        )
        val frames = ByteWriter()
        for (r in list(batch["records"]).map { obj(it) }) {
            val body = Wire.recordBody(
                kind = (r["kind"] as Long).toInt(),
                uuid = hex(r["uuid"]),
                ts = r["ts"] as Long,
                name = r["name"] as String,
                props = props(r["props"], jsonAsStructure),
            )
            frames.bytes(Wire.frame(body))
        }
        return Wire.batch(header, frames.toByteArray())
    }

    @Test
    fun validVectorsEncodeByteForByte() {
        val valid = list(VectorJson.wire()["valid"]).map { obj(it) }
        assertTrue(valid.size >= 6, "expected the valid[] vectors to be present")
        for (case in valid) {
            val batch = obj(case["batch"])
            // json values given as text ...
            assertEquals(case["hex"], encode(batch, jsonAsStructure = false).toHex(), "encode ${case["name"]}")
            // ... and as Kotlin collections going through the SDK's own JSON writer.
            assertEquals(case["hex"], encode(batch, jsonAsStructure = true).toHex(), "encode (collections) ${case["name"]}")
        }
    }

    /** The path `Analytics.capture(name, Map)` takes: PropCodec on the caller thread, then the record body. */
    @Test
    fun validVectorsEncodeThroughPropCodec() {
        var checked = 0
        for (case in list(VectorJson.wire()["valid"]).map { obj(it) }) {
            for (r in list(obj(case["batch"])["records"]).map { obj(it) }) {
                val pairs = props(r["props"], jsonAsStructure = true)
                if (pairs.map { it.first }.toSet().size != pairs.size) continue // duplicate keys cannot be a Map
                val encoded = PropCodec.encode(pairs.toMap(LinkedHashMap()))
                val viaCodec = Wire.recordBody((r["kind"] as Long).toInt(), hex(r["uuid"]), r["ts"] as Long, r["name"] as String, encoded.count, encoded.bytes)
                val direct = Wire.recordBody((r["kind"] as Long).toInt(), hex(r["uuid"]), r["ts"] as Long, r["name"] as String, props(r["props"], false))
                assertEquals(direct.toHex(), viaCodec.toHex(), "${case["name"]}/${r["name"]}")
                checked++
            }
        }
        assertTrue(checked >= 6)
    }

    private fun assertDecodesTo(expected: Map<String, Any?>, actual: DecodedBatch, name: Any?) {
        assertEquals(expected["batch_id"], actual.batchIdHex, "$name batch_id")
        assertEquals(expected["sent_at"], actual.sentAt, "$name sent_at")
        assertEquals(expected["sdk"], actual.sdk, "$name sdk")
        assertEquals(expected["anon_id"], actual.anonId, "$name anon_id")
        assertEquals(expected["user_id"], actual.userId, "$name user_id")
        assertEquals(expected["session_id"], actual.sessionIdHex, "$name session_id")
        assertProps(list(expected["context"]), actual.context, "$name context")
        val records = list(expected["records"]).map { obj(it) }
        assertEquals(records.size, actual.records.size, "$name record count")
        for ((i, r) in records.withIndex()) {
            val a = actual.records[i]
            assertEquals((r["kind"] as Long).toInt(), a.kind, "$name[$i] kind")
            assertEquals(r["uuid"], a.uuid.toHex(), "$name[$i] uuid")
            assertEquals(r["ts"], a.ts, "$name[$i] ts")
            assertEquals(r["name"], a.name, "$name[$i] name")
            assertProps(list(r["props"]), a.props, "$name[$i] props")
        }
    }

    /**
     * Property lists are compared with "last value wins" semantics (spec/wire.md §2): the valid[]
     * vectors list duplicate keys as written, the tolerant[] vectors list them collapsed.
     */
    private fun assertProps(expected: List<Any?>, actual: List<DecodedProp>, what: String) {
        val want = LinkedHashMap<String, Any?>()
        for (e in expected.map { obj(it) }) want[e["k"] as String] = value(e, jsonAsStructure = false)
        val got = LinkedHashMap<String, DecodedProp>()
        for (a in actual) got[a.key] = a
        assertEquals(want.keys.toList(), got.keys.toList(), "$what keys")
        for ((k, v) in want) {
            val a = got[k]!!
            when (v) {
                is JsonText -> {
                    assertEquals(Wire.TAG_JSON, a.tag, "$what.$k tag")
                    assertEquals(v.text, a.value, "$what.$k")
                }
                is Double -> assertEquals(v.toBits(), (a.value as Double).toBits(), "$what.$k")
                else -> assertEquals(v, a.value, "$what.$k")
            }
        }
    }

    @Test
    fun validAndTolerantVectorsDecode() {
        val wire = VectorJson.wire()
        for (case in list(wire["valid"]).map { obj(it) } + list(wire["tolerant"]).map { obj(it) }) {
            assertDecodesTo(obj(case["batch"]), WireDecoder.decode(hex(case["hex"])), case["name"])
        }
    }

    @Test
    fun invalidVectorsAreRejectedAsSpecified() {
        val invalid = list(VectorJson.wire()["invalid"]).map { obj(it) }
        assertTrue(invalid.size >= 20)
        for (case in invalid) {
            val bytes = hex(case["hex"])
            val frame = WireDecoder.frameCheck(bytes)
            assertEquals(case["frame_ok"], frame.ok, "${case["name"]} frame_ok")
            assertEquals((case["http_status"] as Long).toInt(), frame.httpStatus, "${case["name"]} http_status")
            if (!frame.ok) continue
            if (case["header_ok"] == false) {
                assertFailsWith<WireDecoder.HeaderException>("${case["name"]}") { WireDecoder.decode(bytes) }
            } else {
                val expectedBad = list(case["bad_records"]).map { (it as Long).toInt() }
                assertEquals(expectedBad, WireDecoder.decode(bytes).badRecords, "${case["name"]} bad_records")
            }
        }
    }

    @Test
    fun murmur3Vectors() {
        val cases = list(VectorJson.murmur3()["cases"]).map { obj(it) }
        assertTrue(cases.size >= 17)
        for (c in cases) {
            val input = c["input"] as String
            assertEquals(c["hash"], Murmur3.hash32(input), "hash of '$input'")
            assertEquals((c["bucket"] as Long).toInt(), Murmur3.bucket(input), "bucket of '$input'")
        }
    }

    @Test
    fun propertyValueMapping() {
        val w = ByteWriter()
        val values: List<Pair<String, Any?>> = listOf(
            "byte" to 1.toByte(), "short" to 2.toShort(), "int" to 3, "long" to 4L, "uint" to 5u,
            "float" to 1.5f, "double" to 2.5, "char" to 'x', "list" to listOf(1, "a", null, true, 1.5),
            "map" to mapOf("k" to listOf(1, 2), "n" to null), "array" to intArrayOf(1, 2),
            "enum" to Sample.B, "obj" to Custom(),
        )
        Wire.writePropList(w, values)
        val batch = Wire.batch(
            Wire.headerBody(ByteArray(16), 0, "", "", "", ByteArray(16), emptyList()),
            Wire.frame(Wire.recordBody(0, ByteArray(16), 0, "e", values)),
        )
        val props = WireDecoder.decode(batch).records.single()
        for (k in listOf("byte", "short", "int", "long", "uint")) assertTrue(props.prop(k) is Long, k)
        assertEquals(1.5, props.prop("float"))
        assertEquals(2.5, props.prop("double"))
        assertEquals("x", props.prop("char"))
        assertEquals("""[1,"a",null,true,1.5]""", props.prop("list"))
        assertEquals("""{"k":[1,2],"n":null}""", props.prop("map"))
        assertEquals("[1,2]", props.prop("array"))
        assertEquals("B", props.prop("enum"))
        assertEquals("custom!", props.prop("obj"))
        assertEquals(Wire.TAG_JSON, props.props.first { it.key == "list" }.tag)
        assertEquals(Wire.TAG_JSON, props.props.first { it.key == "map" }.tag)
    }

    @Test
    fun propCodecEnforcesLimits() {
        val big = "x".repeat(Wire.MAX_VALUE_BYTES + 1)
        val encoded = PropCodec.encode(
            linkedMapOf("ok" to "v", "big" to big, "k".repeat(129) to 1, "fits" to "y".repeat(Wire.MAX_VALUE_BYTES), "throws" to Throwing()),
        )
        assertEquals(setOf("ok", "fits"), encoded.keys)
        assertEquals(2, encoded.count)

        val many = LinkedHashMap<String, Any?>()
        for (i in 0 until 300) many["k$i"] = i
        assertEquals(Wire.MAX_PROPS, PropCodec.encode(many).count)
        assertFalse(PropCodec.encode(many).keys.contains("k250"))
    }

    private enum class Sample { A, B }

    private class Custom {
        override fun toString() = "custom!"
    }

    private class Throwing {
        override fun toString(): String = throw IllegalStateException("boom")
    }
}
