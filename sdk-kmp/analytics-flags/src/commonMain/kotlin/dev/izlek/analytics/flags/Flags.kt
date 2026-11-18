package dev.izlek.analytics.flags

import dev.izlek.analytics.Analytics
import dev.izlek.analytics.InternalIzlekApi
import dev.izlek.analytics.core.AnalyticsCore
import dev.izlek.analytics.core.PropCodec
import dev.izlek.analytics.internal.Json
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlin.concurrent.Volatile

public data class FlagsConfig(
    /** Base URL the ruleset is served from; defaults to `Config.host`. */
    val flagsHost: String? = null,
    /** Polling interval while the app is in the foreground. */
    val refreshIntervalMs: Long = 5 * 60_000L,
)

/**
 * Feature flags: the ruleset is downloaded as a static file and evaluated locally
 * (design §12, `spec/flags.md`).
 *
 * Call [setup] once, after `Analytics.setup`. Until a ruleset is available (from disk or from
 * the network) every lookup returns the caller's default.
 */
public object Flags {
    @Volatile
    private var client: FlagsClient? = null

    public fun setup(config: FlagsConfig = FlagsConfig()) {
        try {
            if (client != null) return
            val core = Analytics.coreOrNull() ?: return
            client = FlagsClient(core, config).also { it.start() }
        } catch (t: Throwable) {
            // never throw into the host application
        }
    }

    /**
     * The value of a flag: `true` (on, no variants), a variant key, or [default] when the flag is
     * off, unknown, or no ruleset is available.
     *
     * The first lookup of a flag in a session records a `flag_exposed` event.
     */
    public fun flag(key: String, default: Any? = null): Any? = client?.flag(key, default) ?: default

    /** `true` when the flag evaluates to `true` or to any variant. */
    public fun isEnabled(key: String, default: Boolean = false): Boolean {
        val c = client ?: return default
        return c.flag(key, null)?.let { true } ?: default
    }

    /** The variant key of a multivariate flag, or [default]. */
    public fun variant(key: String, default: String? = null): String? {
        val c = client ?: return default
        return c.flag(key, null) as? String ?: default
    }

    /** Downloads the ruleset now. Returns true when the local copy is current afterwards. */
    public suspend fun refresh(): Boolean = client?.refresh() ?: false

    @InternalIzlekApi
    public fun shutdown() {
        client?.stop()
        client = null
    }
}

/** Ruleset storage, refresh and exposure tracking for one [AnalyticsCore]. */
@InternalIzlekApi
public class FlagsClient(private val core: AnalyticsCore, private val config: FlagsConfig) {
    @Volatile
    private var ruleset: Ruleset? = null

    /** Flags already exposed in [exposedSession]; replaced, never mutated (read from any thread). */
    @Volatile
    private var exposed: Set<String> = emptySet()

    @Volatile
    private var exposedSession: String = ""

    // Owned by the SDK dispatcher.
    private var etag: String? = null
    private var job: Job? = null

    private val url = (config.flagsHost ?: core.config.host).trimEnd('/') + "/flags/" + core.config.token + ".json"
    private val path = core.dir + "/" + FILE

    /** The version (`v`) of the ruleset in use, or null. */
    public val rulesetVersion: Long? get() = ruleset?.version

    public fun start() {
        job = core.scope.launch {
            loadFromDisk()
            var failures = 0
            var ok = refreshNow()
            while (true) {
                // Until the first ruleset arrives retry sooner than the regular interval.
                val wait = if (!ok && ruleset == null) {
                    (FIRST_RETRY_MS shl minOf(failures++, 6)).coerceAtMost(config.refreshIntervalMs)
                } else {
                    config.refreshIntervalMs
                }
                delay(wait.coerceAtLeast(1_000))
                // Poll only while the app is in the foreground.
                core.snapshot.first { it != null && it.foreground }
                ok = refreshNow()
            }
        }
    }

    public fun stop() {
        job?.cancel()
        job = null
    }

    public suspend fun refresh(): Boolean = withContext(core.deps.dispatcher) { refreshNow() }

    public fun flag(key: String, default: Any?): Any? {
        val rules = ruleset ?: return default
        val snap = core.snapshot.value ?: return default
        val flag = rules.flags[key]
        val result = try {
            if (flag == null) null else FlagEvaluator.evaluate(flag, snap.props, snap.userId, snap.anonId)
        } catch (t: Throwable) {
            null
        }
        if (exposedSession != snap.sessionId || key !in exposed) {
            expose(key, result, rules.version, snap.sessionId)
        }
        return result ?: default
    }

    /** Records `flag_exposed` once per session and flag. Deduplicated on the SDK dispatcher. */
    private fun expose(key: String, result: Any?, version: Long, session: String) {
        val ts = core.now()
        core.scope.launch {
            if (exposedSession != session) {
                exposedSession = session
                exposed = emptySet()
            }
            if (key in exposed) return@launch
            exposed = exposed + key
            val variant = when (result) {
                null -> "false"
                true -> "true"
                else -> result.toString()
            }
            val props = linkedMapOf<String, Any?>("flag" to key, "variant" to variant, "ruleset_v" to version)
            core.capture(ts, "flag_exposed", PropCodec.encode(props))
        }
    }

    private fun loadFromDisk() {
        try {
            val text = core.deps.fs.readBytes(path)?.decodeToString() ?: return
            val doc = Json.parseOrNull(text) as? Map<*, *> ?: return
            val body = doc["ruleset"] as? String ?: return
            val parsed = Ruleset.parse(body) ?: return
            ruleset = parsed
            etag = doc["etag"] as? String
        } catch (e: Exception) {
            // unreadable copy: behave as if there were none
        }
    }

    private suspend fun refreshNow(): Boolean {
        val headers = LinkedHashMap<String, String>()
        headers["Accept"] = "application/json"
        val tag = etag
        if (tag != null && ruleset != null) headers["If-None-Match"] = tag
        val response = try {
            core.deps.http.request("GET", url, headers, null)
        } catch (e: CancellationException) {
            throw e
        } catch (t: Throwable) {
            return false
        }
        when (response.status) {
            304 -> return true
            200 -> {
                if (response.body.size > MAX_RULESET_BYTES) return false
                val text = response.body.decodeToString()
                val parsed = Ruleset.parse(text) ?: return false
                ruleset = parsed
                etag = response.headers["etag"]
                try {
                    val doc = linkedMapOf<String, Any?>("etag" to etag, "saved_at" to core.now(), "ruleset" to text)
                    core.deps.fs.writeAtomic(path, Json.write(doc).encodeToByteArray())
                } catch (e: Exception) {
                    // keep the in-memory copy; the next successful download writes it again
                }
                return true
            }
            else -> return false
        }
    }

    private companion object {
        const val FILE = "flags"
        const val MAX_RULESET_BYTES = 1_048_576
        const val FIRST_RETRY_MS = 5_000L
    }
}
