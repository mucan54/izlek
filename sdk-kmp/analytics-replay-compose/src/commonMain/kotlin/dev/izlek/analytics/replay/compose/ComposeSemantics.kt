package dev.izlek.analytics.replay.compose

import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.SemanticsActions
import androidx.compose.ui.semantics.SemanticsNode
import androidx.compose.ui.semantics.SemanticsOwner
import androidx.compose.ui.semantics.SemanticsProperties
import androidx.compose.ui.semantics.SemanticsPropertyKey
import androidx.compose.ui.semantics.getOrNull
import androidx.compose.ui.semantics.semantics
import dev.izlek.analytics.replay.MaskMode
import dev.izlek.analytics.replay.NodeType
import dev.izlek.analytics.replay.RawNode
import dev.izlek.analytics.replay.StableIds
import dev.izlek.analytics.replay.TreeCapture
import dev.izlek.analytics.replay.ViewTreeSource

/** Carries a masking request through the semantics tree. */
public val AnalyticsMaskKey: SemanticsPropertyKey<MaskMode> = SemanticsPropertyKey("IzlekAnalyticsMask")

/** Masks the text of this component and of everything inside it in session replay (design §13.4). */
public fun Modifier.analyticsMask(): Modifier = semantics { this[AnalyticsMaskKey] = MaskMode.MASK }

/** Shows the text of this component and of everything inside it, even with `maskAllText`. */
public fun Modifier.analyticsUnmask(): Modifier = semantics { this[AnalyticsMaskKey] = MaskMode.UNMASK }

/**
 * Turns a Compose semantics tree into replay nodes. This is common code: it only uses the public
 * `androidx.compose.ui.semantics` API, which is the same on Android, iOS and desktop.
 *
 * What semantics provides: bounds, text, editable text, password flag, role, click action, focus,
 * scroll ranges. What it does not provide: colors, and any element without semantics (plain
 * `Box`/`Spacer` backgrounds, decorative images without a content description).
 */
public object ComposeSemanticsWalker {

    /**
     * Appends the subtree of [root] to [out].
     *
     * @param density pixels per dp of the composition
     * @param offsetX position of the composition root inside the capture root, in dp
     * @param idFor stable replay id for a semantics node id
     */
    public fun walk(
        root: SemanticsNode,
        parentId: Long,
        density: Float,
        offsetX: Int,
        offsetY: Int,
        idFor: (Int) -> Long,
        out: MutableList<RawNode>,
        maxNodes: Int = 2_000,
    ) {
        if (out.size >= maxNodes) return
        val config = root.config
        val bounds = root.boundsInRoot
        val w = (bounds.width / density).toInt()
        val h = (bounds.height / density).toInt()
        val id = idFor(root.id)
        val editable = config.getOrNull(SemanticsProperties.EditableText)
        val texts = config.getOrNull(SemanticsProperties.Text)
        val role = config.getOrNull(SemanticsProperties.Role)
        val clickable = config.contains(SemanticsActions.OnClick)
        val scrollable = config.contains(SemanticsProperties.VerticalScrollAxisRange) ||
            config.contains(SemanticsProperties.HorizontalScrollAxisRange)
        var text: String? = null
        val type = when {
            editable != null || config.contains(SemanticsActions.SetText) -> {
                text = editable?.text ?: ""
                NodeType.INPUT
            }
            role == Role.Image -> NodeType.IMAGE
            role == Role.Button -> NodeType.BUTTON
            texts != null && texts.isNotEmpty() -> {
                text = texts.joinToString("\n") { it.text }
                NodeType.TEXT
            }
            scrollable -> NodeType.SCROLL
            role != null -> NodeType.OTHER // checkbox, switch, tab, ...
            else -> NodeType.CONTAINER
        }
        // Zero-sized nodes (e.g. the semantics root before layout) are kept as parents only when
        // they have children; a zero-sized leaf carries nothing to draw.
        val children = root.children
        if ((w <= 0 || h <= 0) && children.isEmpty()) return
        out.add(
            RawNode(
                id = id,
                parent = parentId,
                type = type,
                x = offsetX + (bounds.left / density).toInt(),
                y = offsetY + (bounds.top / density).toInt(),
                w = w.coerceAtLeast(0),
                h = h.coerceAtLeast(0),
                color = null, // not available from semantics
                text = text,
                clickable = clickable,
                focused = config.getOrNull(SemanticsProperties.Focused) == true,
                mask = config.getOrNull(AnalyticsMaskKey) ?: MaskMode.INHERIT,
                secure = config.contains(SemanticsProperties.Password),
            ),
        )
        for (child in children) walk(child, id, density, offsetX, offsetY, idFor, out, maxNodes)
    }
}

/**
 * A [ViewTreeSource] over one Compose semantics owner, for hosts where the owner can be reached
 * directly (tests, desktop, custom embeddings). On Android use `ComposeSubtreeWalker` with
 * `AndroidViewTreeSource` instead, which finds the owners inside the View hierarchy.
 *
 * [owner] is called on the SDK dispatcher; make it return null when the UI is not available.
 */
public class SemanticsOwnerTreeSource(
    private val density: () -> Float,
    private val screen: () -> String? = { null },
    private val owner: () -> SemanticsOwner?,
) : ViewTreeSource {
    private val ids = StableIds()

    override suspend fun capture(): TreeCapture? {
        val root = owner()?.unmergedRootSemanticsNode ?: return null
        val d = density().takeIf { it > 0f } ?: 1f
        val out = ArrayList<RawNode>()
        ComposeSemanticsWalker.walk(root, 0, d, 0, 0, { ids.idFor(it) }, out)
        ids.endPass()
        val bounds = root.boundsInRoot
        return TreeCapture((bounds.width / d).toInt(), (bounds.height / d).toInt(), out, screen())
    }
}
