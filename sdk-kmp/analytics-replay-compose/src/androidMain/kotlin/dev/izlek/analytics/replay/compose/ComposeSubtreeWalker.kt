package dev.izlek.analytics.replay.compose

import android.view.View
import androidx.compose.ui.node.RootForTest
import dev.izlek.analytics.replay.AndroidViewTreeSource
import dev.izlek.analytics.replay.NodeType
import dev.izlek.analytics.replay.RawNode
import dev.izlek.analytics.replay.ViewSubtreeWalker
import dev.izlek.analytics.replay.WalkContext

/**
 * Describes Compose content hosted in the View hierarchy. Every Compose host view
 * (`AndroidComposeView`) implements the public `RootForTest` interface, which exposes its
 * `SemanticsOwner`; no reflection and no internal API is involved.
 */
public class ComposeSubtreeWalker : ViewSubtreeWalker {
    private val location = IntArray(2)

    override fun walk(view: View, parentId: Long, context: WalkContext): Boolean {
        val root = view as? RootForTest ?: return false
        view.getLocationOnScreen(location)
        val d = context.density
        val x = ((location[0] - context.originX) / d).toInt()
        val y = ((location[1] - context.originY) / d).toInt()
        val hostId = context.idFor(view)
        context.add(RawNode(hostId, parentId, NodeType.CONTAINER, x, y, (view.width / d).toInt(), (view.height / d).toInt()))
        val nodes = ArrayList<RawNode>()
        ComposeSemanticsWalker.walk(
            root = root.semanticsOwner.unmergedRootSemanticsNode,
            parentId = hostId,
            density = d,
            offsetX = x,
            offsetY = y,
            idFor = { semanticsId -> context.idFor(ComposeNodeKey(view, semanticsId)) },
            out = nodes,
        )
        for (n in nodes) context.add(n)
        return true
    }

    private data class ComposeNodeKey(val host: View, val semanticsId: Int)
}

/** Adds Compose support to the View walker: `AndroidViewTreeSource(app).withCompose()`. */
public fun AndroidViewTreeSource.withCompose(): AndroidViewTreeSource {
    subtreeWalkers.add(ComposeSubtreeWalker())
    return this
}
