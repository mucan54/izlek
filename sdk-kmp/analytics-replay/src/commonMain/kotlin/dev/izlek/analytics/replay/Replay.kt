package dev.izlek.analytics.replay

import dev.izlek.analytics.Analytics
import dev.izlek.analytics.InternalIzlekApi
import kotlin.concurrent.Volatile

public data class ReplayConfig(
    /** Share of sessions that are recorded, 0.0 – 1.0. Decided once per session from its id. */
    val sampleRate: Double = 1.0,
    /** Mask every text, not only input fields. */
    val maskAllText: Boolean = false,
    /** Time between captures; never below 500 ms (2 frames per second). */
    val frameIntervalMs: Long = 500,
    /** Upper bound for compressed chunks waiting on disk; the oldest are dropped beyond it. */
    val maxSpoolBytes: Long = 5L * 1024 * 1024,
)

/**
 * Session replay as wireframes (design §13): rectangles, types and masked text from the view
 * tree — never screenshots.
 *
 * Call [setup] after `Analytics.setup`, with a [ViewTreeSource] for the UI toolkit in use (see
 * the platform helpers in this module, or implement the interface).
 */
public object Replay {
    @Volatile
    private var recorder: ReplayRecorder? = null

    public fun setup(config: ReplayConfig = ReplayConfig(), source: ViewTreeSource) {
        try {
            if (recorder != null) return
            val core = Analytics.coreOrNull() ?: return
            recorder = ReplayRecorder(core, config, source).also { it.start() }
        } catch (t: Throwable) {
            // never throw into the host application
        }
    }

    /** Names the current screen: recorded as a `screen_change` followed by a full snapshot. */
    public fun screen(name: String) {
        recorder?.screen(name)
    }

    /** Reports a touch (dp coordinates). Platform helpers call this; apps can too. */
    public fun touch(phase: TouchPhase, x: Int, y: Int) {
        recorder?.touch(phase.ordinal, x, y)
    }

    /** Reports a scroll of the node [nodeId] by ([dx], [dy]) dp. */
    public fun scroll(nodeId: Long, dx: Int, dy: Int) {
        recorder?.scroll(nodeId, dx, dy)
    }

    /** Stops recording. Chunks already spooled are uploaded on the next start. */
    public fun stop() {
        recorder?.stop()
        recorder = null
    }

    @InternalIzlekApi
    public fun recorderOrNull(): ReplayRecorder? = recorder
}

/** Order matches the wire values: 0 down, 1 move, 2 up. */
public enum class TouchPhase { DOWN, MOVE, UP }
