package dev.izlek.analytics.replay

/** Node types of `spec/replay.md` §2. */
public object NodeType {
    public const val CONTAINER: Int = 0
    public const val TEXT: Int = 1
    public const val IMAGE: Int = 2
    public const val BUTTON: Int = 3
    public const val INPUT: Int = 4
    public const val SCROLL: Int = 5
    public const val OTHER: Int = 6
}

/** Flag bits of a wire node. */
public object NodeFlags {
    public const val HAS_COLOR: Int = 1
    public const val HAS_TEXT: Int = 2
    public const val MASKED: Int = 4
    public const val CLICKABLE: Int = 8
    public const val FOCUSED: Int = 16
}

/**
 * A node exactly as it goes on the wire (`Node` in `spec/replay.md` §2). Coordinates are screen
 * pixels in dp / points, absolute. [color] is `0xAARRGGBB`.
 */
public data class ReplayNode(
    val id: Long,
    val parent: Long,
    val type: Int,
    val x: Int,
    val y: Int,
    val w: Int,
    val h: Int,
    val flags: Int,
    val color: Int? = null,
    val text: String? = null,
)

/** Per-component masking request (`analyticsMask()` / `analyticsUnmask()`). */
public enum class MaskMode {
    /** Follow the nearest ancestor that set a mode, then the defaults of design §13.4. */
    INHERIT,
    MASK,
    UNMASK,
}

/**
 * A node as a platform walker sees it, before masking. Walkers report what is on screen;
 * [Masking] decides what may leave the device.
 */
public class RawNode(
    /** Stable for the lifetime of the UI element, ≥ 1. */
    public val id: Long,
    /** 0 for a root. Parents come before their children in the list. */
    public val parent: Long,
    public val type: Int,
    public val x: Int,
    public val y: Int,
    public val w: Int,
    public val h: Int,
    public val color: Int? = null,
    public val text: String? = null,
    public val clickable: Boolean = false,
    public val focused: Boolean = false,
    public val mask: MaskMode = MaskMode.INHERIT,
    /** Password / secure fields are masked no matter what. */
    public val secure: Boolean = false,
)

/** One capture of the visible UI tree, in draw order (parents before children). */
public class TreeCapture(
    public val width: Int,
    public val height: Int,
    public val nodes: List<RawNode>,
    /** Name of the current screen when the source knows it (e.g. the activity or view controller). */
    public val screen: String? = null,
)

/**
 * Where wireframes come from: the Android View hierarchy, UIKit, a Compose semantics tree, or
 * anything an application provides.
 */
public interface ViewTreeSource {
    /**
     * Returns the current tree, or null when nothing can be captured right now. Called at most
     * twice a second on the SDK dispatcher; implementations hop to the UI thread themselves.
     */
    public suspend fun capture(): TreeCapture?
}
