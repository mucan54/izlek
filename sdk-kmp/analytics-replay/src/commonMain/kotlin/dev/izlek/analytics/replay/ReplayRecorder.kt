package dev.izlek.analytics.replay

import dev.izlek.analytics.InternalIzlekApi
import dev.izlek.analytics.core.AnalyticsCore
import dev.izlek.analytics.internal.ByteFormatException
import dev.izlek.analytics.internal.ByteReader
import dev.izlek.analytics.internal.ByteWriter
import dev.izlek.analytics.internal.Json
import dev.izlek.analytics.internal.Murmur3
import dev.izlek.analytics.internal.hexToBytesOrNull
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/**
 * Frame scheduler, chunker and uploader (`spec/replay.md` §3–§4). Everything runs on the SDK
 * dispatcher of [core]; the public methods may be called from any thread.
 *
 * Closed chunks are gzipped and spooled under `<analytics dir>/replay/` until the server
 * confirms them, so they survive restarts and offline periods (bounded by [ReplayConfig.maxSpoolBytes]).
 */
@InternalIzlekApi
public class ReplayRecorder(
    private val core: AnalyticsCore,
    private val config: ReplayConfig,
    private val source: ViewTreeSource,
) {
    /** Counters for tests and diagnostics. */
    public class Stats {
        public var fullSnapshots: Long = 0
        public var deltas: Long = 0
        public var chunksClosed: Long = 0
        public var chunksUploaded: Long = 0
        public var chunksDropped: Long = 0
        public var captures: Long = 0
    }

    public val stats: Stats = Stats()

    private class Chunk(
        val session: String,
        val seq: Long,
        val startTs: Long,
        val startMono: Long,
        val anonId: String,
        val userId: String,
    ) {
        val frames = ByteWriter(4096)
        var lastMono = startMono
        var count = 0
    }

    private val dir = core.dir + "/replay"
    private val endpoint = core.config.host.trimEnd('/') + "/v1/r"
    private val frameIntervalMs = config.frameIntervalMs.coerceAtLeast(MIN_FRAME_INTERVAL_MS)

    private var job: Job? = null

    // recording state
    private var session = ""
    private var sampled = false
    private var nextSeq = 0L
    private var chunk: Chunk? = null
    private var tree: List<ReplayNode>? = null
    private var width = 0
    private var height = 0
    private var lastFullMono = 0L
    private var screen: String? = null
    private var requestedScreen: String? = null
    private var screenFromApp = false
    private var lastMoveMono = Long.MIN_VALUE

    // upload state
    private var uploading = false
    private var disabled = false
    private var failures = 0
    private var pausedUntil = Long.MIN_VALUE
    private var wakeJob: Job? = null

    public fun start() {
        if (job != null) return
        job = core.scope.launch {
            try {
                core.deps.fs.mkdirs(dir)
            } catch (e: Exception) {
                // spooling will fail and chunks will be dropped; recording itself still works
            }
            pumpUpload()
            while (true) {
                delay(frameIntervalMs)
                try {
                    tick()
                } catch (e: CancellationException) {
                    throw e
                } catch (t: Throwable) {
                    // a failing walker must never take the SDK down
                }
            }
        }
    }

    public fun stop() {
        job?.cancel()
        job = null
        wakeJob?.cancel()
        wakeJob = null
    }

    /** Reports the current screen name; the next frame is a `screen_change` + full snapshot. */
    public fun screen(name: String) {
        core.scope.launch {
            requestedScreen = name
            screenFromApp = true
        }
    }

    /** A touch at ([x], [y]) in dp; `phase` is one of `LarEncoder.TOUCH_*`. */
    public fun touch(phase: Int, x: Int, y: Int) {
        val mono = monoOrNull() ?: return
        core.scope.launch {
            if (phase == LarEncoder.TOUCH_MOVE) {
                if (lastMoveMono != Long.MIN_VALUE && mono - lastMoveMono < MOVE_THROTTLE_MS) return@launch
                lastMoveMono = mono
            }
            writeInput(mono) { dt -> LarEncoder.touch(dt, phase, x, y) }
        }
    }

    public fun scroll(nodeId: Long, dx: Int, dy: Int) {
        val mono = monoOrNull() ?: return
        core.scope.launch { writeInput(mono) { dt -> LarEncoder.scroll(dt, nodeId, dx, dy) } }
    }

    private fun monoOrNull(): Long? = try {
        core.deps.monotonicMs()
    } catch (t: Throwable) {
        null
    }

    // --- scheduler --------------------------------------------------------------------------------

    private suspend fun tick() {
        val snap = core.snapshot.value ?: return
        if (snap.optedOut) {
            // Opt-out: nothing leaves the device, including what was already spooled.
            chunk = null
            tree = null
            wipeSpool()
            return
        }
        if (disabled || !snap.foreground) {
            closeChunk()
            return
        }
        if (snap.sessionId != session) {
            closeChunk()
            beginSession(snap.sessionId)
        }
        if (!sampled) return
        chunk?.let { if (core.deps.monotonicMs() - it.startMono >= CHUNK_MAX_MS) closeChunk() }

        val capture = source.capture() ?: return
        stats.captures++
        var raw = capture.nodes
        if (raw.size > MAX_NODES) raw = raw.subList(0, MAX_NODES)
        val nodes = Masking.apply(raw, config.maskAllText)
        val now = core.deps.monotonicMs()

        val wantedScreen = if (screenFromApp) requestedScreen else capture.screen
        val screenChanged = wantedScreen != null && wantedScreen != screen
        val before = tree
        val sizeChanged = before != null && (capture.width != width || capture.height != height)
        val fullDue = before == null || now - lastFullMono >= FULL_SNAPSHOT_MAX_INTERVAL_MS

        if (chunk != null && before != null && !screenChanged && !sizeChanged && !fullDue) {
            when (val d = TreeDiffer.diff(before, nodes)) {
                TreeDiffer.Diff.Same -> return // unchanged tree: no frame
                is TreeDiffer.Diff.Delta -> {
                    write(now, snap) { dt -> LarEncoder.delta(dt, d.removed, d.upserted) }
                    stats.deltas++
                    tree = nodes
                    finishFrame()
                    return
                }
                TreeDiffer.Diff.Full -> {}
            }
        } else if (chunk == null && before != null && !screenChanged && !sizeChanged && !fullDue && before == nodes) {
            return // nothing on screen changed since the last chunk: do not open a new one
        }

        if (screenChanged) {
            write(now, snap) { dt -> LarEncoder.screenChange(dt, wantedScreen!!) }
            screen = wantedScreen
        }
        if (sizeChanged) write(now, snap) { dt -> LarEncoder.viewport(dt, capture.width, capture.height) }
        width = capture.width
        height = capture.height
        tree = nodes
        writeFull(now, snap)
        finishFrame()
    }

    private fun beginSession(id: String) {
        session = id
        sampled = id.length == 32 && Murmur3.bucket(id) < config.sampleRate * 10_000
        tree = null
        screen = null
        nextSeq = 0
        // Continue the sequence when the same session is resumed after a restart; chunk numbers
        // must never be reused within a session (the server overwrites on equal (s, n)).
        try {
            val doc = core.deps.fs.readBytes("$dir/$STATE_FILE")?.decodeToString()?.let { Json.parseOrNull(it) } as? Map<*, *>
            if (doc != null && doc["session"] == id) nextSeq = (doc["next_seq"] as? Long) ?: 0
        } catch (e: Exception) {
            // start from 0
        }
    }

    private fun writeFull(now: Long, snap: AnalyticsCore.Snapshot) {
        val nodes = tree ?: return
        write(now, snap) { dt -> LarEncoder.fullSnapshot(dt, width, height, nodes) }
        lastFullMono = now
        stats.fullSnapshots++
    }

    /** Touch and scroll frames; a chunk that starts with one gets the last known tree first. */
    private fun writeInput(mono: Long, frame: (Long) -> ByteArray) {
        val snap = core.snapshot.value ?: return
        if (disabled || !sampled || snap.optedOut || !snap.foreground || snap.sessionId != session) return
        if (tree == null) return
        chunk?.let { if (mono - it.startMono >= CHUNK_MAX_MS) closeChunk() }
        if (chunk == null) writeFull(mono, snap)
        write(mono, snap, frame)
        finishFrame()
    }

    private fun write(now: Long, snap: AnalyticsCore.Snapshot, frame: (Long) -> ByteArray) {
        val c = chunk ?: Chunk(session, nextSeq, core.deps.nowMs(), now, snap.anonId, snap.userId).also { chunk = it }
        val bytes = frame((now - c.lastMono).coerceAtLeast(0))
        c.frames.bytes(bytes)
        c.lastMono = maxOf(c.lastMono, now)
        c.count++
    }

    private fun finishFrame() {
        val c = chunk ?: return
        if (c.frames.size >= CHUNK_MAX_BYTES) closeChunk()
    }

    // --- chunks → spool -----------------------------------------------------------------------------

    private fun closeChunk() {
        val c = chunk ?: return
        chunk = null
        if (c.count == 0) return
        nextSeq = c.seq + 1
        stats.chunksClosed++
        try {
            val sessionBytes = c.session.hexToBytesOrNull() ?: return
            val raw = ByteWriter(c.frames.size + 32)
            raw.bytes(LarEncoder.chunkHeader(sessionBytes, c.seq, c.startTs))
            raw.bytes(c.frames.toByteArray())
            val gz = core.deps.gzip(raw.toByteArray()) // the endpoint requires a compressed body
            val file = ByteWriter(gz.size + 96)
            file.u8(SPOOL_VERSION)
            file.varint(c.startTs.coerceAtLeast(0))
            file.varint((c.lastMono - c.startMono).coerceAtLeast(0))
            file.string(c.anonId)
            file.string(c.userId)
            file.bytes(gz)
            val state = linkedMapOf<String, Any?>("session" to c.session, "next_seq" to nextSeq)
            core.deps.fs.writeAtomic("$dir/$STATE_FILE", Json.write(state).encodeToByteArray())
            core.deps.fs.writeAtomic("$dir/${spoolName(c.session, c.seq)}", file.toByteArray())
        } catch (t: Throwable) {
            stats.chunksDropped++
            return
        }
        enforceSpoolLimit()
        pumpUpload()
    }

    private fun wipeSpool() {
        for (f in spoolFiles()) {
            try {
                core.deps.fs.delete("$dir/$f")
                stats.chunksDropped++
            } catch (e: Exception) {
                // retried on the next tick
            }
        }
    }

    private fun spoolFiles(): List<String> = try {
        core.deps.fs.list(dir).filter { it.startsWith(SPOOL_PREFIX) && !it.endsWith(".tmp") }.sorted()
    } catch (e: Exception) {
        emptyList()
    }

    private fun enforceSpoolLimit() {
        try {
            val files = spoolFiles().map { it to (core.deps.fs.readBytes("$dir/$it")?.size ?: 0) }
            var total = files.sumOf { it.second.toLong() }
            for ((name, size) in files) {
                if (total <= config.maxSpoolBytes) break
                core.deps.fs.delete("$dir/$name")
                stats.chunksDropped++
                total -= size
            }
        } catch (e: Exception) {
            // try again when the next chunk is closed
        }
    }

    // --- upload -------------------------------------------------------------------------------------

    private fun pumpUpload() {
        if (uploading || disabled) return
        val wait = pausedUntil - core.deps.monotonicMs()
        if (pausedUntil != Long.MIN_VALUE && wait > 0) {
            if (wakeJob == null) {
                wakeJob = core.scope.launch {
                    delay(wait)
                    wakeJob = null
                    pumpUpload()
                }
            }
            return
        }
        uploading = true
        core.scope.launch {
            try {
                uploadLoop()
            } catch (e: CancellationException) {
                throw e
            } catch (t: Throwable) {
                pause()
            } finally {
                uploading = false
            }
        }
    }

    private fun pause() {
        val shift = minOf(failures++, 9)
        val base = (AnalyticsCore.BACKOFF_MIN_MS shl shift).coerceAtMost(AnalyticsCore.BACKOFF_MAX_MS)
        val r = try {
            (core.deps.randomBytes(1)[0].toInt() and 0xFF) / 256.0
        } catch (t: Throwable) {
            0.5
        }
        pausedUntil = core.deps.monotonicMs() + base / 2 + ((base / 2) * r).toLong()
    }

    private suspend fun uploadLoop() {
        while (!disabled) {
            if (core.snapshot.value?.optedOut == true) {
                wipeSpool()
                return
            }
            val name = spoolFiles().firstOrNull() ?: return
            val path = "$dir/$name"
            val bytes = core.deps.fs.readBytes(path)
            val parsed = bytes?.let { parseSpool(name, it) }
            if (parsed == null) {
                core.deps.fs.delete(path)
                stats.chunksDropped++
                continue
            }
            val status = try {
                core.deps.http.request("POST", parsed.url, parsed.headers, parsed.body).status
            } catch (e: CancellationException) {
                throw e
            } catch (t: Throwable) {
                -1
            }
            when {
                status == 200 -> {
                    core.deps.fs.delete(path)
                    failures = 0
                    stats.chunksUploaded++
                }
                status == 400 || status == 413 -> {
                    core.deps.fs.delete(path)
                    stats.chunksDropped++
                }
                status == 401 || status == 403 -> {
                    // The token is invalid or replay is not enabled for it (spec/replay.md §4):
                    // stop recording for this process and drop what was spooled.
                    disabled = true
                    for (f in spoolFiles()) {
                        core.deps.fs.delete("$dir/$f")
                        stats.chunksDropped++
                    }
                }
                else -> {
                    pause()
                    pumpLater()
                    return
                }
            }
        }
    }

    private fun pumpLater() {
        val wait = (pausedUntil - core.deps.monotonicMs()).coerceAtLeast(1)
        wakeJob?.cancel()
        wakeJob = core.scope.launch {
            delay(wait)
            wakeJob = null
            pumpUpload()
        }
    }

    private class Upload(val url: String, val headers: Map<String, String>, val body: ByteArray)

    private fun parseSpool(name: String, bytes: ByteArray): Upload? {
        // name: r-<session hex32>-<seq, 6+ digits>
        val parts = name.removePrefix(SPOOL_PREFIX).split('-')
        if (parts.size != 2 || parts[0].length != 32) return null
        val seq = parts[1].toLongOrNull() ?: return null
        return try {
            val r = ByteReader(bytes)
            if (r.u8() != SPOOL_VERSION) return null
            val startTs = r.varint()
            val duration = r.varint()
            val anon = r.string()
            val user = r.string()
            val body = r.bytes(r.remaining)
            val url = StringBuilder(endpoint)
            url.append("?s=").append(parts[0]).append("&n=").append(seq).append("&ts=").append(startTs)
            url.append("&d=").append(duration).append("&a=").append(urlEncode(anon))
            if (user.isNotEmpty()) url.append("&u=").append(urlEncode(user))
            val headers = linkedMapOf(
                "Authorization" to "Bearer " + core.config.token,
                "Content-Type" to "application/octet-stream",
                "Content-Encoding" to "gzip",
            )
            Upload(url.toString(), headers, body)
        } catch (e: ByteFormatException) {
            null
        }
    }

    internal companion object {
        const val MIN_FRAME_INTERVAL_MS = 500L // at most 2 frames per second
        const val CHUNK_MAX_MS = 5_000L
        const val CHUNK_MAX_BYTES = 64 * 1024
        const val FULL_SNAPSHOT_MAX_INTERVAL_MS = 60_000L
        const val MAX_NODES = 2_000
        const val MOVE_THROTTLE_MS = 50L
        private const val SPOOL_PREFIX = "r-"
        private const val SPOOL_VERSION = 1
        private const val STATE_FILE = "state"

        fun spoolName(session: String, seq: Long): String = SPOOL_PREFIX + session + "-" + seq.toString().padStart(6, '0')

        /** Percent-encoding of everything outside RFC 3986 "unreserved". */
        fun urlEncode(s: String): String {
            val sb = StringBuilder()
            for (b in s.encodeToByteArray()) {
                val c = b.toInt() and 0xFF
                val ch = c.toChar()
                if (ch in 'a'..'z' || ch in 'A'..'Z' || ch in '0'..'9' || ch == '-' || ch == '.' || ch == '_' || ch == '~') {
                    sb.append(ch)
                } else {
                    sb.append('%').append("0123456789ABCDEF"[c ushr 4]).append("0123456789ABCDEF"[c and 15])
                }
            }
            return sb.toString()
        }
    }
}
