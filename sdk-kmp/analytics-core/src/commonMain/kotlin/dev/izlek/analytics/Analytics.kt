package dev.izlek.analytics

import dev.izlek.analytics.core.AnalyticsCore
import dev.izlek.analytics.core.PropCodec
import dev.izlek.analytics.platform.PlatformDeps
import dev.izlek.analytics.platform.defaultPlatformDeps
import kotlin.concurrent.Volatile

/**
 * Platform handle passed to [Analytics.setup].
 *
 * - Android: `PlatformContext(applicationContext)`
 * - iOS: `PlatformContext()`
 * - JVM: `PlatformContext(storageDir = ..., appVersion = ...)`
 */
public expect class PlatformContext

public data class Config(
    /** Public write token (`pk_...`). */
    val token: String,
    /** Ingestion base URL, e.g. `https://in.example.com`. */
    val host: String,
    /** Number of buffered records that triggers a flush. */
    val flushAt: Int = 20,
    /** Flush timer while the app is in the foreground. */
    val flushIntervalMs: Long = 30_000,
    /** Upper bound for the on-disk queue; the oldest batches are dropped beyond it. */
    val maxQueueBytes: Long = 10L * 1024 * 1024,
    /** Batches older than this are dropped instead of sent. */
    val maxEventAgeMs: Long = 7L * 24 * 3600 * 1000,
    /** Capture `app_installed`, `app_updated`, `app_opened`, `app_backgrounded` automatically. */
    val captureLifecycle: Boolean = true,
    /** gzip request bodies. */
    val gzip: Boolean = true,
)

/**
 * Entry point of the SDK.
 *
 * No function here blocks on I/O or throws: work is handed to a single-threaded dispatcher
 * that owns the on-disk queue. Calls made before [setup] are ignored.
 */
public object Analytics {
    /** Reported in the batch header (`sdk` field). */
    public const val SDK_NAME: String = "kmp/0.1.0"

    @Volatile
    private var core: AnalyticsCore? = null

    public fun setup(config: Config, context: PlatformContext) {
        try {
            if (core != null) return
            if (config.token.isBlank() || config.host.isBlank()) return
            start(config, defaultPlatformDeps(context))
        } catch (t: Throwable) {
            // Never let SDK start-up take the host application down.
        }
    }

    public fun capture(event: String, props: Map<String, Any?> = emptyMap()) {
        val c = core ?: return
        try {
            c.capture(c.now(), event, PropCodec.encode(props))
        } catch (t: Throwable) {
            c.onCallerError()
        }
    }

    public fun screen(name: String, props: Map<String, Any?> = emptyMap()) {
        val c = core ?: return
        try {
            // spec/wire.md §2: name = "screen", props.screen = <name>.
            c.capture(c.now(), "screen", PropCodec.encode(props, extraKey = "screen", extraValue = name))
        } catch (t: Throwable) {
            c.onCallerError()
        }
    }

    public fun identify(userId: String, personProps: Map<String, Any?> = emptyMap()) {
        val c = core ?: return
        try {
            c.identify(c.now(), userId, PropCodec.normalize(personProps))
        } catch (t: Throwable) {
            c.onCallerError()
        }
    }

    public fun setPersonProps(props: Map<String, Any?>) {
        val c = core ?: return
        try {
            c.setPersonProps(c.now(), PropCodec.normalize(props))
        } catch (t: Throwable) {
            c.onCallerError()
        }
    }

    /** Super properties: added to every event captured from now on. */
    public fun register(superProps: Map<String, Any?>) {
        val c = core ?: return
        try {
            c.register(PropCodec.normalize(superProps))
        } catch (t: Throwable) {
            c.onCallerError()
        }
    }

    public fun unregister(key: String) {
        core?.unregister(key)
    }

    /** Logout: writes a `reset` record, then switches to a fresh `anon_id`. */
    public fun reset() {
        val c = core ?: return
        c.reset(c.now())
    }

    /** Deletes the queue, ignores further calls, and remembers the choice across restarts. */
    public fun optOut() {
        core?.setOptOut(true)
    }

    public fun optIn() {
        core?.setOptOut(false)
    }

    /**
     * Closes the open segment and tries to send everything that is queued. Returns when the
     * attempt is over (queue empty, or sending stopped because of an error or a pause).
     */
    public suspend fun flush() {
        core?.flush()
    }

    // --- internal wiring ---------------------------------------------------------------------

    /** The running core, for the flags and replay modules. */
    @InternalIzlekApi
    public fun coreOrNull(): AnalyticsCore? = core

    /** Starts the SDK with explicit platform dependencies (tests, custom embeddings). */
    @InternalIzlekApi
    public fun start(config: Config, deps: PlatformDeps): AnalyticsCore {
        val c = AnalyticsCore(config, deps)
        core = c
        c.start(deps.initialForeground)
        return c
    }

    /** Stops the running core and forgets it. Queued data stays on disk. */
    @InternalIzlekApi
    public fun shutdown() {
        val c = core
        core = null
        c?.close()
    }
}
