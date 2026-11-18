package dev.izlek.analytics.core

import dev.izlek.analytics.internal.Json

/**
 * Content of the `state` file (design §4.3): identities, super properties, opt-out flag, plus
 * the bookkeeping the SDK needs across restarts. Stored as JSON, replaced atomically.
 */
internal class State {
    /** Random per-install id; never changes, not sent to the server. */
    var installId: String = ""
    var anonId: String = ""
    var userId: String = ""
    var superProps: LinkedHashMap<String, Any?> = LinkedHashMap()
    /** Last known person properties (for local flag evaluation only). */
    var personProps: LinkedHashMap<String, Any?> = LinkedHashMap()
    var optOut: Boolean = false
    /** Records dropped and not yet reported through `dropped_count`. */
    var dropped: Long = 0
    var appVersion: String? = null
    var appBuild: String? = null
    /** Current session id (hex32) and the time of the last activity in it. */
    var sessionId: String = ""
    var sessionLastMs: Long = 0

    fun toJson(): String {
        val m = LinkedHashMap<String, Any?>()
        m["v"] = 1L
        m["install_id"] = installId
        m["anon_id"] = anonId
        m["user_id"] = userId
        m["super"] = superProps
        m["person"] = personProps
        m["opt_out"] = optOut
        m["dropped"] = dropped
        m["app_version"] = appVersion
        m["app_build"] = appBuild
        m["session_id"] = sessionId
        m["session_last"] = sessionLastMs
        return Json.write(m)
    }

    companion object {
        /** Returns null when [text] is not a usable state document. */
        fun fromJson(text: String): State? {
            val m = Json.parseOrNull(text) as? Map<*, *> ?: return null
            val s = State()
            s.installId = m["install_id"] as? String ?: return null
            s.anonId = m["anon_id"] as? String ?: return null
            if (s.anonId.isEmpty()) return null
            s.userId = m["user_id"] as? String ?: ""
            s.superProps = toProps(m["super"])
            s.personProps = toProps(m["person"])
            s.optOut = m["opt_out"] as? Boolean ?: false
            s.dropped = (m["dropped"] as? Long)?.coerceAtLeast(0) ?: 0
            s.appVersion = m["app_version"] as? String
            s.appBuild = m["app_build"] as? String
            s.sessionId = (m["session_id"] as? String)?.takeIf { it.length == 32 } ?: ""
            s.sessionLastMs = m["session_last"] as? Long ?: 0
            return s
        }

        private fun toProps(v: Any?): LinkedHashMap<String, Any?> {
            val out = LinkedHashMap<String, Any?>()
            if (v is Map<*, *>) for ((k, x) in v) out[k.toString()] = x
            return out
        }
    }
}
