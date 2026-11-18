package dev.izlek.analytics.replay

import dev.izlek.analytics.InternalIzlekApi
import dev.izlek.analytics.internal.ByteWriter

/** LAR1 chunk encoder (`spec/replay.md`, vectors: `spec/vectors/replay.json`). */
@InternalIzlekApi
public object LarEncoder {
    public const val KIND_FULL_SNAPSHOT: Int = 0
    public const val KIND_DELTA: Int = 1
    public const val KIND_TOUCH: Int = 2
    public const val KIND_SCROLL: Int = 3
    public const val KIND_SCREEN_CHANGE: Int = 4
    public const val KIND_VIEWPORT: Int = 5

    public const val TOUCH_DOWN: Int = 0
    public const val TOUCH_MOVE: Int = 1
    public const val TOUCH_UP: Int = 2

    private val MAGIC = byteArrayOf(0x4C, 0x41, 0x52, 0x31) // "LAR1"

    /** `"LAR1" session_id seq start_ts`; frames follow. */
    public fun chunkHeader(sessionId: ByteArray, seq: Long, startTs: Long): ByteArray {
        require(sessionId.size == 16)
        val w = ByteWriter(32)
        w.bytes(MAGIC)
        w.bytes(sessionId)
        w.varint(seq)
        w.varint(startTs)
        return w.toByteArray()
    }

    public fun writeNode(w: ByteWriter, n: ReplayNode) {
        // The presence bits always follow the optional fields, whatever the caller put in flags.
        var flags = n.flags and (NodeFlags.HAS_COLOR or NodeFlags.HAS_TEXT).inv() and 0xFF
        if (n.color != null) flags = flags or NodeFlags.HAS_COLOR
        if (n.text != null) flags = flags or NodeFlags.HAS_TEXT
        w.varint(n.id)
        w.varint(n.parent)
        w.u8(n.type)
        w.zigzag(n.x.toLong())
        w.zigzag(n.y.toLong())
        w.varint(n.w.coerceAtLeast(0).toLong())
        w.varint(n.h.coerceAtLeast(0).toLong())
        w.u8(flags)
        if (n.color != null) w.u32le(n.color)
        if (n.text != null) w.string(n.text)
    }

    private inline fun frame(kind: Int, dtMs: Long, payload: (ByteWriter) -> Unit): ByteArray {
        val body = ByteWriter(64)
        body.u8(kind)
        body.varint(dtMs.coerceAtLeast(0))
        payload(body)
        val out = ByteWriter(body.size + 3)
        out.varint(body.size.toLong())
        out.bytes(body.toByteArray())
        return out.toByteArray()
    }

    public fun fullSnapshot(dtMs: Long, width: Int, height: Int, nodes: List<ReplayNode>): ByteArray =
        frame(KIND_FULL_SNAPSHOT, dtMs) { w ->
            w.varint(width.coerceAtLeast(0).toLong())
            w.varint(height.coerceAtLeast(0).toLong())
            w.varint(nodes.size.toLong())
            for (n in nodes) writeNode(w, n)
        }

    public fun delta(dtMs: Long, removed: List<Long>, upserted: List<ReplayNode>): ByteArray =
        frame(KIND_DELTA, dtMs) { w ->
            w.varint(removed.size.toLong())
            for (id in removed) w.varint(id)
            w.varint(upserted.size.toLong())
            for (n in upserted) writeNode(w, n)
        }

    public fun touch(dtMs: Long, phase: Int, x: Int, y: Int): ByteArray =
        frame(KIND_TOUCH, dtMs) { w ->
            w.u8(phase)
            w.varint(x.coerceAtLeast(0).toLong())
            w.varint(y.coerceAtLeast(0).toLong())
        }

    public fun scroll(dtMs: Long, nodeId: Long, dx: Int, dy: Int): ByteArray =
        frame(KIND_SCROLL, dtMs) { w ->
            w.varint(nodeId)
            w.zigzag(dx.toLong())
            w.zigzag(dy.toLong())
        }

    public fun screenChange(dtMs: Long, name: String): ByteArray =
        frame(KIND_SCREEN_CHANGE, dtMs) { w -> w.string(name) }

    public fun viewport(dtMs: Long, width: Int, height: Int): ByteArray =
        frame(KIND_VIEWPORT, dtMs) { w ->
            w.varint(width.coerceAtLeast(0).toLong())
            w.varint(height.coerceAtLeast(0).toLong())
        }
}
