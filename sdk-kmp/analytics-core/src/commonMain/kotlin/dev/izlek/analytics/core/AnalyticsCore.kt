package dev.izlek.analytics.core

import dev.izlek.analytics.Analytics
import dev.izlek.analytics.Config
import dev.izlek.analytics.InternalIzlekApi
import dev.izlek.analytics.internal.ByteWriter
import dev.izlek.analytics.internal.IdGenerator
import dev.izlek.analytics.internal.Murmur3
import dev.izlek.analytics.internal.Wire
import dev.izlek.analytics.internal.hexToBytesOrNull
import dev.izlek.analytics.internal.toHex
import dev.izlek.analytics.platform.PlatformDeps
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.job
import kotlinx.coroutines.launch

/**
 * The SDK engine: identity, session, on-disk queue and sender.
 *
 * Threading model: public methods only enqueue a command (`Channel.trySend`, never blocks);
 * one coroutine on [PlatformDeps.dispatcher] (a serial dispatcher) executes the commands and
 * owns every mutable field. The sender is a second coroutine on the same dispatcher, so it
 * interleaves with command handling only at its suspension point (the HTTP call).
 */
@InternalIzlekApi
public class AnalyticsCore(
    public val config: Config,
    public val deps: PlatformDeps,
) {
    /** A consistent view of identity and evaluation properties, readable from any thread. */
    public class Snapshot(
        public val anonId: String,
        public val userId: String,
        /** hex32 */
        public val sessionId: String,
        /** auto context → super properties → person properties (later ones win). */
        public val props: Map<String, Any?>,
        public val optedOut: Boolean,
        public val foreground: Boolean,
    )

    /** Counters for tests and diagnostics. Written on the SDK dispatcher only. */
    public class Stats {
        public var sentBatches: Long = 0
        public var sentRecords: Long = 0
        public var droppedRecords: Long = 0
        public var internalErrors: Long = 0
        public var callerErrors: Long = 0
        public var lastStatus: Int = 0
        public var sendAttempts: Long = 0
    }

    /** `<storageDir>/analytics/<token-hash>` */
    public val dir: String = deps.storageDir.trimEnd('/') + "/analytics/" + tokenHash(config.token)

    public val scope: CoroutineScope = CoroutineScope(SupervisorJob() + deps.dispatcher)

    public val stats: Stats = Stats()

    private val _snapshot = MutableStateFlow<Snapshot?>(null)

    /** Null until start-up has finished reading the state file. */
    public val snapshot: StateFlow<Snapshot?> get() = _snapshot

    private val commands = Channel<Cmd>(Channel.UNLIMITED)
    private val ids = IdGenerator(deps.randomBytes)
    private val queue = EventQueue(deps.fs, dir) { ids.uuid7(deps.nowMs()) }
    private var state = State()
    private var sessionBytes = ByteArray(16)
    private var sessionPersistedMs = 0L
    private var stateDirty = false

    private var foreground = true
    private var openedOnce = false
    private var timerJob: Job? = null

    // sender
    private var sending = false
    private var inFlight: PendingFile? = null
    private var flushRequested = false
    private var pausedUntil = Long.MIN_VALUE
    private var wakeJob: Job? = null
    private var failures = 0
    private val flushWaiters = ArrayList<CompletableDeferred<Unit>>()

    private val endpoint = config.host.trimEnd('/') + "/v1/e"
    private val rotateAt = config.flushAt.coerceIn(1, Wire.MAX_BATCH_RECORDS)

    private sealed class Cmd {
        class Capture(val ts: Long, val name: String, val props: EncodedProps) : Cmd()
        class Identify(val ts: Long, val userId: String, val props: Map<String, Any?>) : Cmd()
        class SetPerson(val ts: Long, val props: Map<String, Any?>) : Cmd()
        class Register(val props: Map<String, Any?>) : Cmd()
        class Unregister(val key: String) : Cmd()
        class Reset(val ts: Long) : Cmd()
        class OptOut(val value: Boolean) : Cmd()
        class Flush(val done: CompletableDeferred<Unit>) : Cmd()
        class Lifecycle(val foreground: Boolean, val ts: Long) : Cmd()
    }

    // --- API (any thread) -----------------------------------------------------------------------

    public fun now(): Long = deps.nowMs()

    public fun capture(ts: Long, name: String, props: EncodedProps) {
        submit(Cmd.Capture(ts, name, props))
    }

    public fun identify(ts: Long, userId: String, props: Map<String, Any?>) {
        submit(Cmd.Identify(ts, userId, props))
    }

    public fun setPersonProps(ts: Long, props: Map<String, Any?>) {
        submit(Cmd.SetPerson(ts, props))
    }

    public fun register(props: Map<String, Any?>) {
        submit(Cmd.Register(props))
    }

    public fun unregister(key: String) {
        submit(Cmd.Unregister(key))
    }

    public fun reset(ts: Long) {
        submit(Cmd.Reset(ts))
    }

    public fun setOptOut(value: Boolean) {
        submit(Cmd.OptOut(value))
    }

    public fun onBackground() {
        submit(Cmd.Lifecycle(false, safeNow()))
    }

    public fun onForeground() {
        submit(Cmd.Lifecycle(true, safeNow()))
    }

    public suspend fun flush() {
        val done = CompletableDeferred<Unit>()
        if (!commands.trySend(Cmd.Flush(done)).isSuccess) return
        val handle = scope.coroutineContext.job.invokeOnCompletion { done.complete(Unit) }
        try {
            done.await()
        } finally {
            handle.dispose()
        }
    }

    public fun onCallerError() {
        stats.callerErrors++
    }

    /**
     * Starts the engine. [initialForeground] tells whether the application is visible right now
     * (decides whether `app_opened` is recorded immediately and whether the flush timer runs).
     */
    public fun start(initialForeground: Boolean = true) {
        foreground = initialForeground
        scope.launch {
            try {
                init()
            } catch (e: CancellationException) {
                throw e
            } catch (t: Throwable) {
                stats.internalErrors++
            }
            for (cmd in commands) {
                try {
                    if (stateDirty) persistState()
                    handle(cmd)
                } catch (e: CancellationException) {
                    throw e
                } catch (t: Throwable) {
                    stats.internalErrors++
                }
            }
        }
        try {
            deps.registerLifecycle({ onBackground() }, { onForeground() })
        } catch (t: Throwable) {
            stats.callerErrors++
        }
    }

    /** Stops all SDK coroutines. Queued data stays on disk. */
    public fun close() {
        commands.close()
        scope.cancel()
    }

    private fun submit(cmd: Cmd) {
        commands.trySend(cmd)
    }

    private fun safeNow(): Long = try {
        deps.nowMs()
    } catch (t: Throwable) {
        0
    }

    // --- start-up ---------------------------------------------------------------------------------

    private fun init() {
        queue.open()
        val now = deps.nowMs()
        var readFailed = false
        val text = try {
            deps.fs.readBytes("$dir/$STATE_FILE")?.decodeToString()
        } catch (e: Exception) {
            readFailed = true
            null
        }
        val loaded = text?.let { State.fromJson(it) }
        val firstRun = text == null && !readFailed
        state = loaded ?: State().also {
            it.installId = IdGenerator.format(ids.uuid7(now))
            it.anonId = IdGenerator.format(ids.uuid7(now))
        }
        if (state.optOut) queue.wipe()

        val context = safeContext()
        val version = context["app_version"]?.toString()
        val build = context["app_build"]?.toString()
        val previousVersion = state.appVersion
        val previousBuild = state.appBuild
        val updated = !firstRun && loaded != null &&
            (previousVersion != null || previousBuild != null) &&
            (previousVersion != version || previousBuild != build)
        state.appVersion = version
        state.appBuild = build

        sessionBytes = state.sessionId.hexToBytesOrNull()?.takeIf { it.size == 16 } ?: ByteArray(16)
        if (state.sessionId.isEmpty() || sessionExpired(now)) newSession(now) else state.sessionLastMs = now
        persistState()

        if (config.captureLifecycle && !state.optOut) {
            if (firstRun) {
                append(Wire.KIND_EVENT, now, "app_installed", PropCodec.encode(versionProps("version", version, "build", build)), true)
            } else if (updated) {
                append(
                    Wire.KIND_EVENT, now, "app_updated",
                    PropCodec.encode(versionProps("previous_version", previousVersion, "previous_build", previousBuild)), true,
                )
            }
            if (foreground) {
                append(Wire.KIND_EVENT, now, "app_opened", PropCodec.encode(mapOf("from_background" to false)), true)
                openedOnce = true
            }
        }
        publish()
        enforceLimits(now)
        if (foreground) startTimer()
        pump()
    }

    private fun versionProps(k1: String, v1: String?, k2: String, v2: String?): Map<String, Any?> {
        val m = LinkedHashMap<String, Any?>()
        if (v1 != null) m[k1] = v1
        if (v2 != null) m[k2] = v2
        return m
    }

    private fun safeContext(): Map<String, Any?> = try {
        deps.deviceContext()
    } catch (t: Throwable) {
        emptyMap()
    }

    // --- command handling (SDK dispatcher) -----------------------------------------------------

    private fun handle(cmd: Cmd) {
        when (cmd) {
            is Cmd.Capture -> {
                append(Wire.KIND_EVENT, cmd.ts, cmd.name, cmd.props, true)
                pump()
            }
            is Cmd.Identify -> identifyNow(cmd)
            is Cmd.SetPerson -> {
                if (state.optOut || cmd.props.isEmpty()) return
                state.personProps.putAll(cmd.props)
                persistState()
                append(Wire.KIND_SET_PERSON, cmd.ts, "", PropCodec.encode(cmd.props), false)
                publish()
                pump()
            }
            is Cmd.Register -> {
                if (state.optOut || cmd.props.isEmpty()) return
                state.superProps.putAll(cmd.props)
                persistState()
                publish()
            }
            is Cmd.Unregister -> {
                if (state.superProps.containsKey(cmd.key)) {
                    state.superProps.remove(cmd.key)
                    persistState()
                    publish()
                }
            }
            is Cmd.Reset -> {
                if (state.optOut) return
                resetNow(cmd.ts)
            }
            is Cmd.OptOut -> {
                if (state.optOut == cmd.value) return
                state.optOut = cmd.value
                if (cmd.value) {
                    queue.wipe()
                    state.dropped = 0
                    flushRequested = false
                    wakeJob?.cancel()
                    wakeJob = null
                }
                persistState()
                publish()
            }
            is Cmd.Flush -> {
                flushWaiters.add(cmd.done)
                if (queue.activeCount > 0) flushRequested = true
                pump()
            }
            is Cmd.Lifecycle -> lifecycleNow(cmd)
        }
    }

    private fun identifyNow(cmd: Cmd.Identify) {
        if (state.optOut) return
        val id = cmd.userId
        if (id.isEmpty() || id.encodeToByteArray().size > Wire.MAX_ID_BYTES) {
            addDropped(1)
            return
        }
        // Identifying as a different user without reset(): the anonymous id must not be linked
        // to two users, so the previous user is logged out first.
        if (state.userId.isNotEmpty() && state.userId != id) resetNow(cmd.ts)
        state.userId = id
        val person = LinkedHashMap<String, Any?>()
        person["user_id"] = id
        for ((k, v) in cmd.props) {
            if (k == "user_id") continue
            person[k] = v
            state.personProps[k] = v
        }
        persistState()
        // The record carries the new identity, so the queue closes the anonymous segment first
        // (identity change is always a segment boundary).
        append(Wire.KIND_IDENTIFY, cmd.ts, "", PropCodec.encode(person), false)
        publish()
        pump()
    }

    private fun resetNow(ts: Long) {
        append(Wire.KIND_RESET, ts, "", EncodedProps.EMPTY, false)
        // Close the segment right away so that it is sent with the old identity.
        queue.rotate()
        state.anonId = IdGenerator.format(ids.uuid7(ts))
        state.userId = ""
        state.personProps.clear()
        newSession(ts)
        persistState()
        publish()
        pump()
    }

    private fun lifecycleNow(cmd: Cmd.Lifecycle) {
        if (cmd.foreground == foreground) return
        foreground = cmd.foreground
        if (cmd.foreground) {
            // Session timeout is checked when coming back from the background (design §4.6).
            if (!state.optOut) touchSession(cmd.ts)
            if (config.captureLifecycle) {
                append(Wire.KIND_EVENT, cmd.ts, "app_opened", PropCodec.encode(mapOf("from_background" to openedOnce)), true)
            }
            openedOnce = true
            startTimer()
        } else {
            timerJob?.cancel()
            timerJob = null
            if (config.captureLifecycle) append(Wire.KIND_EVENT, cmd.ts, "app_backgrounded", EncodedProps.EMPTY, true)
            if (!state.optOut) persistState()
            if (queue.activeCount > 0) flushRequested = true
        }
        publish()
        pump()
    }

    /** Encodes and stores one record. Never throws; failures are counted as dropped records. */
    private fun append(kind: Int, ts: Long, name: String, props: EncodedProps, withSuper: Boolean) {
        if (state.optOut) return
        if (name.length > Wire.MAX_NAME_BYTES / 4 && name.encodeToByteArray().size > Wire.MAX_NAME_BYTES) {
            addDropped(1)
            return
        }
        touchSession(ts)
        var count = props.count
        var bytes = props.bytes
        if (withSuper && state.superProps.isNotEmpty()) {
            val w = ByteWriter(bytes.size + 64)
            var n = 0
            for ((k, v) in state.superProps) {
                if (k in props.keys) continue // the record's own value wins
                if (n + props.count >= Wire.MAX_PROPS) break
                if (Wire.writeProp(w, k, v, enforceLimits = true)) n++
            }
            w.bytes(bytes)
            count += n
            bytes = w.toByteArray()
        }
        val body = Wire.recordBody(kind, ids.uuid7(ts), ts, name, count, bytes)
        if (body.size > Wire.MAX_FRAME_BYTES) {
            addDropped(1)
            return
        }
        val frame = Wire.frame(body)
        // Keep every segment within the batch limits of spec/wire.md §3.
        if (queue.activeCount >= Wire.MAX_BATCH_RECORDS || queue.activeBytes + frame.size > MAX_SEGMENT_BYTES) {
            queue.rotate()
        }
        val meta = SegmentMeta(ts, state.anonId, state.userId, sessionBytes)
        if (!queue.append(frame, meta)) {
            addDropped(1)
            return
        }
        if (queue.activeCount >= rotateAt) flushRequested = true
        enforceLimits(deps.nowMs())
    }

    // --- session ----------------------------------------------------------------------------------

    private fun sessionExpired(now: Long): Boolean {
        val idle = now - state.sessionLastMs
        // A negative idle time means the wall clock went backwards: keep the session.
        return idle > SESSION_TIMEOUT_MS
    }

    private fun touchSession(now: Long) {
        if (state.sessionId.isEmpty() || sessionExpired(now)) {
            newSession(now)
            persistState()
            publish()
            return
        }
        state.sessionLastMs = now
        val sincePersist = now - sessionPersistedMs
        if (sincePersist > SESSION_PERSIST_INTERVAL_MS || sincePersist < 0) persistState()
    }

    private fun newSession(now: Long) {
        sessionBytes = ids.uuid7(now)
        state.sessionId = sessionBytes.toHex()
        state.sessionLastMs = now
    }

    // --- state ------------------------------------------------------------------------------------

    private fun persistState() {
        stateDirty = try {
            deps.fs.writeAtomic("$dir/$STATE_FILE", state.toJson().encodeToByteArray())
            sessionPersistedMs = state.sessionLastMs
            false
        } catch (e: Exception) {
            true
        }
    }

    private fun publish() {
        val props = LinkedHashMap<String, Any?>()
        props.putAll(safeContext())
        props.putAll(state.superProps)
        props.putAll(state.personProps)
        _snapshot.value = Snapshot(state.anonId, state.userId, state.sessionId, props, state.optOut, foreground)
    }

    private fun addDropped(n: Int) {
        if (n <= 0) return
        stats.droppedRecords += n
        state.dropped += n
        persistState()
    }

    /** Applies `maxEventAgeMs` and `maxQueueBytes` (design §4.3). */
    private fun enforceLimits(now: Long) {
        var i = 0
        while (i < queue.pending.size) {
            val p = queue.pending[i]
            // Files "from the future" (clock moved backwards) are kept.
            if (p !== inFlight && now - p.createdMs > config.maxEventAgeMs) {
                queue.remove(p)
                addDropped(p.records)
            } else {
                i++
            }
        }
        while (queue.totalBytes() > config.maxQueueBytes) {
            val victim = queue.pending.firstOrNull { it !== inFlight }
            if (victim != null) {
                queue.remove(victim)
                addDropped(victim.records)
            } else if (inFlight == null && queue.activeCount > 0) {
                addDropped(queue.dropActive())
            } else {
                break
            }
        }
    }

    // --- sender -----------------------------------------------------------------------------------

    private fun startTimer() {
        timerJob?.cancel()
        timerJob = null
        if (config.flushIntervalMs <= 0) return
        timerJob = scope.launch {
            while (true) {
                delay(config.flushIntervalMs)
                if (queue.activeCount > 0) flushRequested = true
                pump()
            }
        }
    }

    /** Starts the sender when there is something to send and sending is not paused. */
    private fun pump() {
        if (sending) return
        if (state.optOut) {
            completeFlushWaiters()
            return
        }
        val wait = pausedUntil - deps.monotonicMs()
        if (pausedUntil != Long.MIN_VALUE && wait > 0) {
            scheduleWake(wait)
            completeFlushWaiters()
            return
        }
        val wantRotate = flushRequested && queue.activeCount > 0
        if (queue.pending.isEmpty() && !wantRotate) {
            flushRequested = false
            completeFlushWaiters()
            return
        }
        sending = true
        scope.launch {
            try {
                sendLoop()
            } catch (e: CancellationException) {
                throw e
            } catch (t: Throwable) {
                stats.internalErrors++
                pause(backoffMs())
            } finally {
                sending = false
                inFlight = null
                completeFlushWaiters()
            }
        }
    }

    private fun completeFlushWaiters() {
        if (flushWaiters.isEmpty()) return
        for (w in flushWaiters) w.complete(Unit)
        flushWaiters.clear()
    }

    private fun scheduleWake(delayMs: Long) {
        if (wakeJob != null) return
        wakeJob = scope.launch {
            delay(delayMs)
            wakeJob = null
            pump()
        }
    }

    private fun pause(ms: Long) {
        pausedUntil = deps.monotonicMs() + ms
        wakeJob?.cancel()
        wakeJob = null
    }

    /** Exponential backoff 1 s → 5 min; the actual delay is uniform in [base/2, base]. */
    private fun backoffMs(): Long {
        val shift = if (failures > 9) 9 else failures
        failures++
        val base = (BACKOFF_MIN_MS shl shift).coerceAtMost(BACKOFF_MAX_MS)
        val r = try {
            val b = deps.randomBytes(2)
            (((b[0].toInt() and 0xFF) shl 8) or (b[1].toInt() and 0xFF)) / 65536.0
        } catch (t: Throwable) {
            0.5
        }
        return base / 2 + ((base / 2) * r).toLong()
    }

    /** One pending file at a time, oldest first (design §4.4). */
    private suspend fun sendLoop() {
        val endBackgroundTask = try {
            deps.beginBackgroundTask("izlek-flush")
        } catch (t: Throwable) {
            {}
        }
        try {
            while (true) {
                if (state.optOut) break
                val wait = pausedUntil - deps.monotonicMs()
                if (pausedUntil != Long.MIN_VALUE && wait > 0) {
                    scheduleWake(wait)
                    break
                }
                enforceLimits(deps.nowMs())
                if (queue.pending.isEmpty()) {
                    // Lazy rotation: the open segment is closed only when the backlog is empty,
                    // so offline periods produce few large batches instead of many small ones.
                    val rotate = flushRequested && queue.activeCount > 0
                    flushRequested = false
                    if (rotate && queue.rotate()) continue
                    break
                }
                sendOne(queue.pending[0])
            }
        } finally {
            try {
                endBackgroundTask()
            } catch (t: Throwable) {
                // ignore
            }
        }
    }

    private suspend fun sendOne(p: PendingFile) {
        val bytes = try {
            deps.fs.readBytes(queue.path(p))
        } catch (e: Exception) {
            // Cannot read right now (I/O error): try again later.
            pause(backoffMs())
            return
        }
        val scan = bytes?.let { Segment.scan(it) }
        if (bytes == null || scan == null || scan.recordCount == 0) {
            queue.remove(p)
            addDropped(p.records)
            return
        }
        val reportedDropped = state.dropped
        val context = ArrayList<Pair<String, Any?>>()
        for ((k, v) in safeContext()) if (k != "sdk") context.add(k to v)
        if (reportedDropped > 0) context.add("dropped_count" to reportedDropped)
        val header = Wire.headerBody(
            batchId = EventQueue.batchId(p.name),
            sentAt = deps.nowMs().coerceAtLeast(0),
            sdk = Analytics.SDK_NAME,
            anonId = scan.meta.anonId,
            userId = scan.meta.userId,
            sessionId = scan.meta.sessionId,
            context = context,
        )
        val raw = Wire.batch(header, bytes, scan.recordsOffset, scan.validLength - scan.recordsOffset)
        val headers = LinkedHashMap<String, String>()
        headers["Authorization"] = "Bearer " + config.token
        headers["Content-Type"] = "application/octet-stream"
        var payload = raw
        if (config.gzip) {
            try {
                payload = deps.gzip(raw)
                headers["Content-Encoding"] = "gzip"
            } catch (t: Throwable) {
                payload = raw
            }
        }

        inFlight = p
        stats.sendAttempts++
        var status = -1
        var retryAfterMs: Long? = null
        try {
            val response = deps.http.request("POST", endpoint, headers, payload)
            status = response.status
            retryAfterMs = response.headers["retry-after"]?.trim()?.toLongOrNull()
                ?.let { (it * 1000).coerceIn(RETRY_AFTER_MIN_MS, RETRY_AFTER_MAX_MS) }
        } catch (e: CancellationException) {
            throw e
        } catch (t: Throwable) {
            status = -1 // network error
        } finally {
            inFlight = null
        }
        stats.lastStatus = status

        // The queue may have changed while the request was in flight (opt-out, eviction).
        val present = queue.pending.any { it === p }
        when {
            status == 200 -> {
                if (present) queue.remove(p)
                failures = 0
                stats.sentBatches++
                stats.sentRecords += scan.recordCount
                if (reportedDropped > 0) {
                    state.dropped = (state.dropped - reportedDropped).coerceAtLeast(0)
                    persistState()
                }
            }
            status == 400 -> {
                if (present) {
                    queue.remove(p)
                    addDropped(p.records)
                }
                failures = 0
            }
            status == 401 || status == 403 -> {
                if (present) {
                    queue.remove(p)
                    addDropped(p.records)
                }
                pause(AUTH_PAUSE_MS)
            }
            status == 413 -> {
                if (present) {
                    when (queue.split(p)) {
                        EventQueue.SplitResult.SPLIT -> {}
                        EventQueue.SplitResult.TOO_SMALL -> {
                            queue.remove(p)
                            addDropped(p.records)
                        }
                        EventQueue.SplitResult.FAILED -> pause(backoffMs())
                    }
                }
            }
            status == 429 -> pause(retryAfterMs ?: backoffMs())
            else -> {
                // 5xx, network errors and anything unexpected are treated as transient.
                val backoff = backoffMs()
                pause(if (retryAfterMs != null && status == 503) maxOf(backoff, retryAfterMs) else backoff)
            }
        }
    }

    public companion object {
        public const val SESSION_TIMEOUT_MS: Long = 30L * 60 * 1000
        public const val AUTH_PAUSE_MS: Long = 3_600_000
        public const val BACKOFF_MIN_MS: Long = 1_000
        public const val BACKOFF_MAX_MS: Long = 300_000
        private const val RETRY_AFTER_MIN_MS = 1_000L
        private const val RETRY_AFTER_MAX_MS = 3_600_000L
        private const val SESSION_PERSIST_INTERVAL_MS = 60_000L
        private const val STATE_FILE = "state"

        /** Largest segment file: leaves room for the magic and a maximum-size header. */
        internal const val MAX_SEGMENT_BYTES: Long = 900_000

        /** Directory name derived from the token (the token itself is not written to disk paths). */
        public fun tokenHash(token: String): String {
            val data = token.encodeToByteArray()
            val a = Murmur3.hash32(data, 0).toLong() and 0xFFFFFFFFL
            val b = Murmur3.hash32(data, 1).toLong() and 0xFFFFFFFFL
            return a.toString(16).padStart(8, '0') + b.toString(16).padStart(8, '0')
        }
    }
}
