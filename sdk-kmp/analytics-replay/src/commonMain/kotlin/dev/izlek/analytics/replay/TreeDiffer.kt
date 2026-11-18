package dev.izlek.analytics.replay

import dev.izlek.analytics.InternalIzlekApi

/**
 * Computes `delta` frames (`spec/replay.md` §2).
 *
 * Player semantics assumed (the only ones the spec defines): `removed` drops a node with its
 * whole subtree, then every `upserted` node replaces the node with the same id or is added as
 * the last child of its parent. A delta can therefore not express a reordering of siblings or
 * an insertion between existing siblings; in those cases a full snapshot is requested.
 */
@InternalIzlekApi
public object TreeDiffer {
    public sealed class Diff {
        /** Nothing changed: no frame is written. */
        public object Same : Diff()

        /** The change cannot be (or is not worth being) expressed as a delta. */
        public object Full : Diff()

        public class Delta(public val removed: List<Long>, public val upserted: List<ReplayNode>) : Diff()
    }

    public fun diff(old: List<ReplayNode>, new: List<ReplayNode>): Diff {
        if (old == new) return Diff.Same
        val oldById = HashMap<Long, ReplayNode>(old.size * 2)
        for (n in old) oldById[n.id] = n
        val newById = HashMap<Long, ReplayNode>(new.size * 2)
        for (n in new) if (newById.put(n.id, n) != null) return Diff.Full // duplicate ids
        if (oldById.size != old.size) return Diff.Full

        if (!orderPreserved(old, new, oldById, newById)) return Diff.Full

        // Roots of removed subtrees: removed nodes whose old parent is still alive (or was a root).
        val removedRoots = ArrayList<Long>()
        for (n in old) {
            if (newById.containsKey(n.id)) continue
            if (n.parent == 0L || newById.containsKey(n.parent)) removedRoots.add(n.id)
        }

        // A surviving node that used to live below a removed node disappears from the player
        // together with that subtree, so it has to be sent again even when it did not change.
        val goneWithSubtree = HashMap<Long, Boolean>()
        fun underRemoved(id: Long): Boolean {
            val node = oldById[id] ?: return false
            goneWithSubtree[id]?.let { return it }
            val result = when {
                node.parent == 0L -> false
                !newById.containsKey(node.parent) -> true
                else -> underRemoved(node.parent)
            }
            goneWithSubtree[id] = result
            return result
        }

        val upserted = ArrayList<ReplayNode>()
        for (n in new) {
            val before = oldById[n.id]
            if (before == null || before != n || underRemoved(n.id)) upserted.add(n)
        }
        if (upserted.size + removedRoots.size >= new.size + 1) return Diff.Full
        return Diff.Delta(removedRoots, upserted)
    }

    /**
     * True when, for every parent, the children that stay under it keep their relative order and
     * every child that is new under that parent comes after them.
     */
    private fun orderPreserved(
        old: List<ReplayNode>,
        new: List<ReplayNode>,
        oldById: Map<Long, ReplayNode>,
        newById: Map<Long, ReplayNode>,
    ): Boolean {
        fun stays(n: ReplayNode): Boolean = oldById[n.id]?.parent == n.parent && newById[n.id]?.parent == n.parent
        val oldKept = HashMap<Long, ArrayList<Long>>()
        for (n in old) if (stays(n)) oldKept.getOrPut(n.parent) { ArrayList() }.add(n.id)
        val position = HashMap<Long, Int>() // per parent: how many kept children were seen so far
        val sawAdded = HashSet<Long>() // parents that already have a new child in the new order
        for (n in new) {
            if (stays(n)) {
                if (n.parent in sawAdded) return false
                val index = position[n.parent] ?: 0
                val expected = oldKept[n.parent]
                if (expected == null || index >= expected.size || expected[index] != n.id) return false
                position[n.parent] = index + 1
            } else {
                sawAdded.add(n.parent)
            }
        }
        return true
    }
}
