package dev.izlek.analytics.core

import dev.izlek.analytics.internal.IdGenerator
import dev.izlek.analytics.internal.hexToBytesOrNull
import dev.izlek.analytics.internal.toHex
import dev.izlek.analytics.platform.FileSystem

/** A closed segment waiting to be sent. */
internal class PendingFile(
    val name: String,
    val size: Long,
    val records: Int,
    val createdMs: Long,
)

/**
 * The on-disk queue (design §4.3): one open segment (`active`) and closed segments
 * (`pending-<seq>-<uuid>[-<split path>]`). Not thread-safe; owned by the SDK dispatcher.
 *
 * No method throws: I/O failures are reported through return values and leave the files in a
 * state that the next call (or the next start-up scan) can repair.
 */
internal class EventQueue(
    private val fs: FileSystem,
    private val dir: String,
    /** Fresh 16-byte ids for pending file names (UUIDv7). */
    private val newId: () -> ByteArray,
) {
    val pending = ArrayList<PendingFile>()

    var activeCount = 0
        private set
    var activeBytes = 0L
        private set
    var activeMeta: SegmentMeta? = null
        private set

    /** The tail of `active` may hold a partial record after a failed append. */
    private var activeDirty = false
    private var nextSeq = 1L

    private val activePath get() = "$dir/$ACTIVE"

    fun path(p: PendingFile): String = "$dir/${p.name}"

    fun totalBytes(): Long {
        var t = activeBytes
        for (p in pending) t += p.size
        return t
    }

    /**
     * Start-up scan: removes leftovers, truncates torn tails, and turns a left-over `active`
     * into a pending segment. Returns false when the directory cannot be used at all.
     */
    fun open(): Boolean {
        val names = try {
            fs.mkdirs(dir)
            fs.list(dir)
        } catch (e: Exception) {
            return false
        }
        val pendingNames = names.filter { isPendingName(it) }.sorted()
        val nameSet = pendingNames.toHashSet()
        for (n in names) {
            if (n.endsWith(TMP_SUFFIX)) quietDelete("$dir/$n")
        }
        for (n in pendingNames) {
            // A split writes both children before deleting the parent. If the parent is still
            // there the split did not finish: the parent wins and the children are discarded.
            if (hasExistingAncestor(n, nameSet)) {
                quietDelete("$dir/$n")
                continue
            }
            val info = recover("$dir/$n", n) ?: continue
            pending.add(PendingFile(n, info.size, info.records, info.createdMs))
            val seq = n.substring(PREFIX.length, PREFIX.length + SEQ_DIGITS).toLongOrNull() ?: 0
            if (seq >= nextSeq) nextSeq = seq + 1
        }
        if (ACTIVE in names) {
            val info = recover(activePath, ACTIVE)
            if (info != null) {
                // Records of a previous process run: close the segment so it is sent right away.
                val name = newPendingName()
                try {
                    fs.rename(activePath, "$dir/$name")
                    pending.add(PendingFile(name, info.size, info.records, info.createdMs))
                } catch (e: Exception) {
                    // Could not rename: keep writing to it as the open segment.
                    activeMeta = info.meta
                    activeCount = info.records
                    activeBytes = info.size
                    activeDirty = true // forces a truncate to the valid length before the next append
                }
            }
        }
        return true
    }

    private class Recovered(val size: Long, val records: Int, val meta: SegmentMeta) {
        val createdMs get() = meta.createdMs
    }

    /** Validates one segment file, truncating a torn tail. Deletes unusable or empty files. */
    private fun recover(path: String, @Suppress("UNUSED_PARAMETER") name: String): Recovered? {
        val bytes = try {
            fs.readBytes(path)
        } catch (e: Exception) {
            null
        } ?: return null
        val scan = Segment.scan(bytes)
        if (scan == null || scan.recordCount == 0) {
            quietDelete(path)
            return null
        }
        if (scan.validLength < bytes.size) {
            try {
                fs.truncate(path, scan.validLength.toLong())
            } catch (e: Exception) {
                // Sending re-scans the file and only uses the valid part, so this is harmless.
            }
        }
        return Recovered(scan.validLength.toLong(), scan.recordCount, scan.meta)
    }

    /**
     * Appends one framed record. A segment only ever holds records of one identity: when [meta]
     * differs from the open segment's identity the segment is closed first.
     */
    fun append(frame: ByteArray, meta: SegmentMeta): Boolean {
        try {
            val current = activeMeta
            if (current != null && !current.sameIdentity(meta)) {
                if (!rotate()) return false
            }
            if (activeDirty) repairActive()
            if (activeMeta == null) {
                val pre = Segment.preamble(meta)
                val first = ByteArray(pre.size + frame.size)
                pre.copyInto(first)
                frame.copyInto(first, pre.size)
                activeDirty = true
                fs.append(activePath, first)
                activeDirty = false
                activeMeta = meta
                activeBytes = first.size.toLong()
                activeCount = 1
            } else {
                activeDirty = true
                fs.append(activePath, frame)
                activeDirty = false
                activeBytes += frame.size
                activeCount++
            }
            return true
        } catch (e: Exception) {
            return false
        }
    }

    /** Brings `active` back to its last known good length. */
    private fun repairActive() {
        if (activeMeta == null) fs.delete(activePath) else fs.truncate(activePath, activeBytes)
        activeDirty = false
    }

    /** `active` → `pending-<seq>-<uuid>` (atomic rename). Returns false when nothing was closed. */
    fun rotate(): Boolean {
        val meta = activeMeta ?: return false
        if (activeCount == 0) return false
        return try {
            if (activeDirty) repairActive()
            val name = newPendingName()
            fs.rename(activePath, "$dir/$name")
            pending.add(PendingFile(name, activeBytes, activeCount, meta.createdMs))
            clearActive()
            true
        } catch (e: Exception) {
            false
        }
    }

    private fun clearActive() {
        activeMeta = null
        activeCount = 0
        activeBytes = 0
        activeDirty = false
    }

    /** Drops the open segment; returns the number of records lost. */
    fun dropActive(): Int {
        val n = activeCount
        quietDelete(activePath)
        clearActive()
        return n
    }

    fun remove(p: PendingFile) {
        quietDelete(path(p))
        pending.remove(p)
    }

    /** Deletes every queued record (opt-out). */
    fun wipe() {
        for (p in pending) quietDelete(path(p))
        pending.clear()
        quietDelete(activePath)
        clearActive()
    }

    enum class SplitResult { SPLIT, TOO_SMALL, FAILED }

    /**
     * Splits a pending segment in two at the record boundary closest to the middle (HTTP 413).
     * The children are named `<parent>-0` and `<parent>-1` and take the parent's place.
     */
    fun split(p: PendingFile): SplitResult {
        val index = pending.indexOf(p)
        if (index < 0) return SplitResult.FAILED
        val bytes = try {
            fs.readBytes(path(p))
        } catch (e: Exception) {
            null
        } ?: return SplitResult.FAILED
        val scan = Segment.scan(bytes) ?: return SplitResult.TOO_SMALL
        val n = scan.recordCount
        if (n < 2) return SplitResult.TOO_SMALL
        val half = scan.recordsOffset + (scan.validLength - scan.recordsOffset) / 2
        var k = 1
        while (k < n - 1 && scan.boundaries[k] < half) k++
        val cut = scan.boundaries[k]
        val pre = scan.recordsOffset
        val a = bytes.copyOfRange(0, cut)
        val b = ByteArray(pre + (scan.validLength - cut))
        bytes.copyInto(b, 0, 0, pre)
        bytes.copyInto(b, pre, cut, scan.validLength)
        val nameA = p.name + "-0"
        val nameB = p.name + "-1"
        try {
            fs.writeAtomic("$dir/$nameA", a)
            fs.writeAtomic("$dir/$nameB", b)
            fs.delete(path(p))
        } catch (e: Exception) {
            quietDelete("$dir/$nameA")
            quietDelete("$dir/$nameB")
            return SplitResult.FAILED
        }
        pending[index] = PendingFile(nameA, a.size.toLong(), k, p.createdMs)
        pending.add(index + 1, PendingFile(nameB, b.size.toLong(), n - k, p.createdMs))
        return SplitResult.SPLIT
    }

    private fun newPendingName(): String {
        val seq = nextSeq++
        return PREFIX + seq.toString().padStart(SEQ_DIGITS, '0') + "-" + newId().toHex()
    }

    private fun quietDelete(path: String) {
        try {
            fs.delete(path)
        } catch (e: Exception) {
            // nothing to do
        }
    }

    companion object {
        const val ACTIVE = "active"
        const val PREFIX = "pending-"
        const val TMP_SUFFIX = ".tmp"
        private const val SEQ_DIGITS = 12
        private const val BASE_LENGTH = 8 + SEQ_DIGITS + 1 + 32

        fun isPendingName(name: String): Boolean {
            if (!name.startsWith(PREFIX) || name.length < BASE_LENGTH || name.endsWith(TMP_SUFFIX)) return false
            for (i in PREFIX.length until PREFIX.length + SEQ_DIGITS) if (name[i] !in '0'..'9') return false
            if (name[PREFIX.length + SEQ_DIGITS] != '-') return false
            if (name.substring(PREFIX.length + SEQ_DIGITS + 1, BASE_LENGTH).hexToBytesOrNull() == null) return false
            var i = BASE_LENGTH
            while (i < name.length) {
                if (i + 1 >= name.length || name[i] != '-' || (name[i + 1] != '0' && name[i + 1] != '1')) return false
                i += 2
            }
            return true
        }

        private fun hasExistingAncestor(name: String, all: Set<String>): Boolean {
            var end = name.length - 2
            while (end >= BASE_LENGTH) {
                if (name.substring(0, end) in all) return true
                end -= 2
            }
            return false
        }

        /**
         * `batch_id` of a pending file, derived from its name only, so that every retry of the
         * same file carries the same id:
         * - unsplit file: the UUIDv7 embedded in the name;
         * - split child: UUIDv8-shaped digest of the part of the name after `pending-<seq>-`
         *   (parent uuid + split path), so the two halves get distinct, stable ids.
         */
        fun batchId(name: String): ByteArray {
            val tail = name.substring(PREFIX.length + SEQ_DIGITS + 1)
            if (name.length == BASE_LENGTH) {
                tail.hexToBytesOrNull()?.let { return it }
            }
            return IdGenerator.uuid8(tail)
        }
    }
}
