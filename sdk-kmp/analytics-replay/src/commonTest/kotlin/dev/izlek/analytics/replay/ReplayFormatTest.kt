package dev.izlek.analytics.replay

import dev.izlek.analytics.internal.ByteWriter
import dev.izlek.analytics.internal.hexToBytesOrNull
import dev.izlek.analytics.internal.toHex
import dev.izlek.analytics.testkit.VectorJson
import dev.izlek.analytics.testkit.VectorJson.list
import dev.izlek.analytics.testkit.VectorJson.obj
import kotlin.random.Random
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertIs
import kotlin.test.assertTrue

/** spec/vectors/replay.json, masking rules and the tree differ. */
class ReplayFormatTest {

    private fun int(v: Any?): Int = (v as Long).toInt()

    private fun node(v: Any?): ReplayNode {
        val n = obj(v)
        return ReplayNode(
            id = n["id"] as Long, parent = n["parent"] as Long, type = int(n["type"]),
            x = int(n["x"]), y = int(n["y"]), w = int(n["w"]), h = int(n["h"]), flags = int(n["flags"]),
            color = (n["color"] as Long?)?.toInt(), text = n["text"] as String?,
        )
    }

    private fun encode(chunk: Map<String, Any?>): ByteArray {
        val w = ByteWriter()
        w.bytes(LarEncoder.chunkHeader((chunk["session_id"] as String).hexToBytesOrNull()!!, chunk["seq"] as Long, chunk["start_ts"] as Long))
        for (f in list(chunk["frames"]).map { obj(it) }) {
            val dt = f["dt"] as Long
            val bytes = when (int(f["kind"])) {
                0 -> LarEncoder.fullSnapshot(dt, int(f["w"]), int(f["h"]), list(f["nodes"]).map { node(it) })
                1 -> LarEncoder.delta(dt, list(f["removed"]).map { it as Long }, list(f["upserted"]).map { node(it) })
                2 -> LarEncoder.touch(dt, int(f["phase"]), int(f["x"]), int(f["y"]))
                3 -> LarEncoder.scroll(dt, f["node_id"] as Long, int(f["dx"]), int(f["dy"]))
                4 -> LarEncoder.screenChange(dt, f["name"] as String)
                5 -> LarEncoder.viewport(dt, int(f["w"]), int(f["h"]))
                else -> error("unknown kind")
            }
            w.bytes(bytes)
        }
        return w.toByteArray()
    }

    @Test
    fun validVectorsEncodeByteForByte() {
        val valid = list(VectorJson.replay()["valid"]).map { obj(it) }
        assertTrue(valid.size >= 2)
        for (case in valid) {
            assertEquals(case["hex"], encode(obj(case["chunk"])).toHex(), "encode ${case["name"]}")
        }
    }

    @Test
    fun validAndTolerantVectorsDecode() {
        val doc = VectorJson.replay()
        for (case in list(doc["valid"]).map { obj(it) } + list(doc["tolerant"]).map { obj(it) }) {
            val expected = obj(case["chunk"])
            val actual = LarDecoder.decode((case["hex"] as String).hexToBytesOrNull()!!)
            assertEquals(expected["session_id"], actual.session)
            assertEquals(expected["seq"], actual.seq)
            assertEquals(expected["start_ts"], actual.startTs)
            val frames = list(expected["frames"]).map { obj(it) }
            assertEquals(frames.map { int(it["kind"]) }, actual.kinds, "${case["name"]} frame kinds")
            for ((i, f) in frames.withIndex()) {
                val a = actual.frames[i]
                when (a.kind) {
                    0 -> assertEquals(list(f["nodes"]).map { node(it) }, a.nodes)
                    1 -> {
                        assertEquals(list(f["removed"]), a.removed)
                        assertEquals(list(f["upserted"]).map { node(it) }, a.nodes)
                    }
                    4 -> assertEquals(f["name"], a.name)
                    5 -> assertEquals(listOf(int(f["w"]), int(f["h"])), listOf(a.w, a.h))
                }
            }
        }
    }

    // --- masking (design §13.4) -----------------------------------------------------------------

    private fun raw(
        id: Long, parent: Long, type: Int, text: String? = null, mask: MaskMode = MaskMode.INHERIT,
        secure: Boolean = false, color: Int? = null, clickable: Boolean = false, focused: Boolean = false,
    ) = RawNode(id, parent, type, 0, 0, 10, 10, color, text, clickable, focused, mask, secure)

    @Test
    fun maskingDefaults() {
        val out = Masking.apply(
            listOf(
                raw(1, 0, NodeType.CONTAINER, color = 0xFFFFFFFF.toInt()),
                raw(2, 1, NodeType.TEXT, "Hoş geldin"),
                raw(3, 1, NodeType.INPUT, "secret", focused = true),
                raw(4, 1, NodeType.IMAGE, "alt text", color = 0xFF112233.toInt()),
                raw(5, 1, NodeType.BUTTON, "Devam", color = 0xFF0A84FF.toInt(), clickable = true),
                raw(6, 1, NodeType.INPUT, ""),
                raw(7, 1, NodeType.INPUT, null),
            ),
            maskAllText = false,
        )
        assertEquals(ReplayNode(1, 0, 0, 0, 0, 10, 10, 1, color = -1), out[0])
        assertEquals("Hoş geldin", out[1].text)
        assertEquals(NodeFlags.HAS_TEXT, out[1].flags)
        // the same node as in spec/vectors/replay.json: flags 22 = has_text | masked | focused
        assertEquals("••••••", out[2].text)
        assertEquals(22, out[2].flags)
        // images never carry content
        assertEquals(ReplayNode(4, 1, NodeType.IMAGE, 0, 0, 10, 10, 0), out[3])
        assertEquals(11, out[4].flags)
        assertEquals("Devam", out[4].text)
        assertEquals("", out[5].text)
        assertEquals(NodeFlags.HAS_TEXT or NodeFlags.MASKED, out[5].flags)
        assertEquals(0, out[6].flags)
    }

    @Test
    fun maskAllTextAndPerComponentOverrides() {
        val tree = listOf(
            raw(1, 0, NodeType.CONTAINER),
            raw(2, 1, NodeType.TEXT, "public"),
            raw(3, 1, NodeType.CONTAINER, mask = MaskMode.UNMASK),
            raw(4, 3, NodeType.TEXT, "explicitly visible"),
            raw(5, 3, NodeType.INPUT, "search term"),
            raw(6, 3, NodeType.INPUT, "hunter2", secure = true),
            raw(7, 3, NodeType.TEXT, "IBAN TR00", mask = MaskMode.MASK),
            raw(8, 1, NodeType.CONTAINER, mask = MaskMode.MASK),
            raw(9, 8, NodeType.CONTAINER),
            raw(10, 9, NodeType.TEXT, "çğ🚀"),
            raw(11, 9, NodeType.TEXT, "ok", mask = MaskMode.UNMASK),
        )
        val all = Masking.apply(tree, maskAllText = true).associateBy { it.id }
        assertEquals("••••••", all[2]!!.text, "maskAllText")
        assertEquals("explicitly visible", all[4]!!.text, "analyticsUnmask() on an ancestor")
        assertEquals("search term", all[5]!!.text, "an explicitly unmasked input")
        assertEquals("•••••••", all[6]!!.text, "secure fields can never be unmasked")
        assertEquals("•••••••••", all[7]!!.text, "the nearest explicit mode wins")
        assertEquals("•••", all[10]!!.text, "length counted in code points")
        assertEquals("ok", all[11]!!.text)

        val defaults = Masking.apply(tree, maskAllText = false).associateBy { it.id }
        assertEquals("public", defaults[2]!!.text)
        assertEquals("•••", defaults[10]!!.text, "analyticsMask() applies to the whole subtree")
        assertTrue(defaults.values.filter { it.flags and NodeFlags.MASKED != 0 }.all { n -> n.text!!.all { it == '•' } })
    }

    @Test
    fun longTextIsTruncated() {
        val out = Masking.apply(listOf(raw(1, 0, NodeType.TEXT, "x".repeat(500)), raw(2, 0, NodeType.INPUT, "y".repeat(500))), false)
        assertEquals(200, out[0].text!!.length)
        assertEquals("•".repeat(200), out[1].text)
    }

    // --- differ ---------------------------------------------------------------------------------

    private fun n(id: Long, parent: Long, text: String? = null) =
        ReplayNode(id, parent, if (text == null) 0 else 1, 0, 0, 10, 10, if (text == null) 0 else 2, null, text)

    private val base = listOf(n(1, 0), n(2, 1, "a"), n(3, 1), n(4, 3, "b"), n(5, 3, "c"), n(6, 1, "d"))

    private fun delta(old: List<ReplayNode>, new: List<ReplayNode>) = assertIs<TreeDiffer.Diff.Delta>(TreeDiffer.diff(old, new))

    @Test
    fun differBasics() {
        assertIs<TreeDiffer.Diff.Same>(TreeDiffer.diff(base, base.toList()))

        // one changed node
        val changed = base.map { if (it.id == 4L) it.copy(text = "B") else it }
        delta(base, changed).let {
            assertEquals(emptyList(), it.removed)
            assertEquals(listOf(n(4, 3, "B")), it.upserted)
        }

        // a removed subtree is named by its root only
        delta(base, base.filter { it.id !in listOf(3L, 4L, 5L) }).let {
            assertEquals(listOf(3L), it.removed)
            assertEquals(emptyList(), it.upserted)
        }

        // appended child
        delta(base, base + n(7, 1, "new")).let {
            assertEquals(emptyList(), it.removed)
            assertEquals(listOf(n(7, 1, "new")), it.upserted)
        }

        // a node that moves out of a removed subtree is re-sent together with its descendants
        val old = listOf(n(1, 0), n(2, 1), n(3, 2), n(4, 3, "leaf"), n(5, 1, "tail"))
        val moved = listOf(n(1, 0), n(5, 1, "tail"), n(3, 1), n(4, 3, "leaf"))
        delta(old, moved).let {
            assertEquals(listOf(2L), it.removed)
            assertEquals(listOf(n(3, 1), n(4, 3, "leaf")), it.upserted)
        }
    }

    @Test
    fun differFallsBackToFullSnapshotWhenOrderMatters() {
        // sibling reorder
        assertIs<TreeDiffer.Diff.Full>(TreeDiffer.diff(base, listOf(base[0], base[5], base[1], base[2], base[3], base[4])))
        // insertion between existing siblings
        assertIs<TreeDiffer.Diff.Full>(TreeDiffer.diff(base, listOf(base[0], base[1], n(9, 1, "x")) + base.drop(2)))
        // everything replaced
        assertIs<TreeDiffer.Diff.Full>(TreeDiffer.diff(base, listOf(n(10, 0), n(11, 10, "z"))))
        // duplicate ids from a buggy walker
        assertIs<TreeDiffer.Diff.Full>(TreeDiffer.diff(base, base + base[5]))
    }

    private class T(val id: Long, var text: String?, val kids: ArrayList<T> = ArrayList())

    private fun flatten(roots: List<T>): List<ReplayNode> {
        val out = ArrayList<ReplayNode>()
        fun visit(t: T, parent: Long) {
            out.add(n(t.id, parent, t.text))
            for (k in t.kids) visit(k, t.id)
        }
        for (r in roots) visit(r, 0)
        return out
    }

    /** Property: whatever the differ answers, a player that applies it ends up with the new tree. */
    @Test
    fun randomMutationsReplayToTheSameTree() {
        val random = Random(20261118)
        var nextId = 1L
        val root = T(nextId++, null)
        fun all(): List<T> {
            val out = ArrayList<T>()
            fun visit(t: T) {
                out.add(t)
                t.kids.forEach { visit(it) }
            }
            visit(root)
            return out
        }
        fun parentOf(x: T): T? = all().firstOrNull { x in it.kids }
        fun contains(a: T, b: T): Boolean = a === b || a.kids.any { contains(it, b) }

        val player = Player()
        var current = flatten(listOf(root))
        player.full(current)
        var deltas = 0
        var fulls = 0
        repeat(600) { step ->
            repeat(1 + random.nextInt(3)) {
                val nodes = all()
                val pick = nodes[random.nextInt(nodes.size)]
                when (random.nextInt(7)) {
                    0, 1 -> pick.kids.add(T(nextId++, if (random.nextBoolean()) "t$step" else null)) // append
                    2 -> pick.text = "changed$step"
                    3 -> if (pick !== root) parentOf(pick)!!.kids.remove(pick) // remove subtree
                    4 -> if (pick.kids.size >= 2) pick.kids.add(0, pick.kids.removeAt(pick.kids.size - 1)) // reorder
                    5 -> pick.kids.add(random.nextInt(pick.kids.size + 1), T(nextId++, "mid$step")) // insert anywhere
                    6 -> { // move a subtree under another node
                        val target = nodes[random.nextInt(nodes.size)]
                        if (pick !== root && !contains(pick, target)) {
                            parentOf(pick)!!.kids.remove(pick)
                            target.kids.add(pick)
                        }
                    }
                }
            }
            val next = flatten(listOf(root))
            when (val d = TreeDiffer.diff(current, next)) {
                TreeDiffer.Diff.Same -> assertEquals(current, next)
                TreeDiffer.Diff.Full -> {
                    player.full(next)
                    fulls++
                }
                is TreeDiffer.Diff.Delta -> {
                    player.delta(d.removed, d.upserted)
                    deltas++
                }
            }
            assertEquals(next, player.flatten(), "step $step")
            current = next
        }
        assertTrue(deltas > 100 && fulls > 20, "both paths exercised: deltas=$deltas fulls=$fulls")
    }
}
