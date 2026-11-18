package dev.izlek.analytics.replay

import dev.izlek.analytics.internal.ByteReader
import dev.izlek.analytics.internal.toHex

/** Test-side LAR1 decoder (what a player does). Unknown frame kinds are skipped. */
class DecodedFrame(
    val kind: Int,
    val dt: Long,
    val w: Int = 0,
    val h: Int = 0,
    val nodes: List<ReplayNode> = emptyList(),
    val removed: List<Long> = emptyList(),
    val phase: Int = 0,
    val x: Int = 0,
    val y: Int = 0,
    val nodeId: Long = 0,
    val dx: Int = 0,
    val dy: Int = 0,
    val name: String = "",
)

class DecodedChunk(val session: String, val seq: Long, val startTs: Long, val frames: List<DecodedFrame>) {
    val kinds: List<Int> get() = frames.map { it.kind }
    val durationMs: Long get() = frames.sumOf { it.dt }
}

object LarDecoder {
    fun decode(bytes: ByteArray): DecodedChunk {
        require(bytes.copyOf(4).decodeToString() == "LAR1") { "bad magic" }
        val r = ByteReader(bytes, 4)
        val session = r.bytes(16).toHex()
        val seq = r.varint()
        val startTs = r.varint()
        val frames = ArrayList<DecodedFrame>()
        while (r.remaining > 0) {
            val len = r.length()
            val f = ByteReader(bytes, r.pos, r.pos + len)
            r.skip(len)
            val kind = f.u8()
            val dt = f.varint()
            when (kind) {
                0 -> {
                    val w = f.length()
                    val h = f.length()
                    frames.add(DecodedFrame(kind, dt, w = w, h = h, nodes = List(f.length()) { node(f) }))
                }
                1 -> {
                    val removed = List(f.length()) { f.varint() }
                    frames.add(DecodedFrame(kind, dt, removed = removed, nodes = List(f.length()) { node(f) }))
                }
                2 -> frames.add(DecodedFrame(kind, dt, phase = f.u8(), x = f.length(), y = f.length()))
                3 -> frames.add(DecodedFrame(kind, dt, nodeId = f.varint(), dx = f.zigzag().toInt(), dy = f.zigzag().toInt()))
                4 -> frames.add(DecodedFrame(kind, dt, name = f.string(strictUtf8 = true)))
                5 -> frames.add(DecodedFrame(kind, dt, w = f.length(), h = f.length()))
                else -> {} // skipped; its dt is lost here, which is fine for these tests
            }
        }
        return DecodedChunk(session, seq, startTs, frames)
    }

    private fun node(f: ByteReader): ReplayNode {
        val id = f.varint()
        val parent = f.varint()
        val type = f.u8()
        val x = f.zigzag().toInt()
        val y = f.zigzag().toInt()
        val w = f.length()
        val h = f.length()
        val flags = f.u8()
        val color = if (flags and NodeFlags.HAS_COLOR != 0) f.u32le() else null
        val text = if (flags and NodeFlags.HAS_TEXT != 0) f.string(strictUtf8 = true) else null
        return ReplayNode(id, parent, type, x, y, w, h, flags, color, text)
    }
}

/** Reference player: applies frames the way `spec/replay.md` §2 describes. */
class Player {
    private val nodes = HashMap<Long, ReplayNode>()
    private val children = HashMap<Long, ArrayList<Long>>()

    fun full(list: List<ReplayNode>) {
        nodes.clear()
        children.clear()
        for (n in list) add(n)
    }

    fun delta(removed: List<Long>, upserted: List<ReplayNode>) {
        for (id in removed) removeSubtree(id)
        for (n in upserted) {
            val old = nodes[n.id]
            if (old == null) {
                add(n)
            } else {
                if (old.parent != n.parent) {
                    children[old.parent]?.remove(n.id)
                    children.getOrPut(n.parent) { ArrayList() }.add(n.id)
                }
                nodes[n.id] = n
            }
        }
    }

    private fun add(n: ReplayNode) {
        nodes[n.id] = n
        children.getOrPut(n.parent) { ArrayList() }.add(n.id)
    }

    private fun removeSubtree(id: Long) {
        val n = nodes.remove(id) ?: return
        children[n.parent]?.remove(id)
        dropChildren(id)
    }

    private fun dropChildren(id: Long) {
        for (c in children.remove(id).orEmpty()) {
            nodes.remove(c)
            dropChildren(c)
        }
    }

    /** Draw order: pre-order from the roots. */
    fun flatten(): List<ReplayNode> {
        val out = ArrayList<ReplayNode>()
        fun visit(parent: Long) {
            for (id in children[parent].orEmpty()) {
                out.add(nodes[id]!!)
                visit(id)
            }
        }
        visit(0)
        return out
    }
}
