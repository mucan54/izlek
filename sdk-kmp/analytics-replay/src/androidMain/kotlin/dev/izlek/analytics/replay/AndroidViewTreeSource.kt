package dev.izlek.analytics.replay

import android.app.Activity
import android.app.Application
import android.graphics.drawable.ColorDrawable
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.text.InputType
import android.text.method.PasswordTransformationMethod
import android.view.KeyboardShortcutGroup
import android.view.Menu
import android.view.MotionEvent
import android.view.View
import android.view.ViewGroup
import android.view.Window
import android.widget.AbsListView
import android.widget.Button
import android.widget.EditText
import android.widget.HorizontalScrollView
import android.widget.ImageView
import android.widget.ScrollView
import android.widget.TextView
import kotlinx.coroutines.suspendCancellableCoroutine
import java.lang.ref.WeakReference
import java.util.WeakHashMap
import java.util.concurrent.CopyOnWriteArrayList
import kotlin.coroutines.resume

/**
 * Lets another UI toolkit hosted inside a View (Jetpack Compose) describe its own subtree.
 * Return true when the view was handled; the default walker then skips it and its children.
 */
public fun interface ViewSubtreeWalker {
    public fun walk(view: View, parentId: Long, context: WalkContext): Boolean
}

/** What a [ViewSubtreeWalker] needs to add nodes to the capture in progress. */
public class WalkContext internal constructor(
    /** Pixels per dp. */
    public val density: Float,
    /** Screen position of the capture root, in pixels. */
    public val originX: Int,
    public val originY: Int,
    private val out: MutableList<RawNode>,
    private val ids: StableIds,
) {
    /** Stable node id for [key] (use something that identifies the element, e.g. `view to semanticsId`). */
    public fun idFor(key: Any): Long = ids.idFor(key) + FOREIGN_ID_BASE

    public fun add(node: RawNode) {
        if (out.size < MAX_NODES) out.add(node)
    }

    internal companion object {
        const val MAX_NODES = 2_000
        // View ids count up from 1; ids for foreign subtrees live in their own range.
        const val FOREIGN_ID_BASE = 1L shl 40
    }
}

/** Masks this view and everything below it in session replay (design §13.4). */
public fun View.analyticsMask() {
    ViewMasks.set(this, MaskMode.MASK)
}

/** Shows the text of this view and of everything below it, even with `maskAllText`. */
public fun View.analyticsUnmask() {
    ViewMasks.set(this, MaskMode.UNMASK)
}

internal object ViewMasks {
    private val modes = WeakHashMap<View, MaskMode>()

    fun set(view: View, mode: MaskMode) = synchronized(modes) { modes[view] = mode }

    fun get(view: View): MaskMode = synchronized(modes) { modes[view] } ?: MaskMode.INHERIT
}

/**
 * Walks the View hierarchy of the resumed activity on the main thread and reports rectangles,
 * types and text. Touches are forwarded to [Replay.touch] through a `Window.Callback` wrapper.
 *
 * Usage: `Replay.setup(ReplayConfig(), AndroidViewTreeSource(application))`.
 */
public class AndroidViewTreeSource(application: Application) : ViewTreeSource {
    private val main = Handler(Looper.getMainLooper())
    private var current: WeakReference<Activity>? = null
    private val viewIds = WeakHashMap<View, Long>()
    private var nextViewId = 1L
    private val foreignIds = StableIds()
    private val location = IntArray(2)

    /** Walkers for embedded toolkits; see `analytics-replay-compose`. */
    public val subtreeWalkers: MutableList<ViewSubtreeWalker> = CopyOnWriteArrayList()

    init {
        application.registerActivityLifecycleCallbacks(object : Application.ActivityLifecycleCallbacks {
            override fun onActivityResumed(activity: Activity) {
                current = WeakReference(activity)
                installTouchHook(activity)
            }

            override fun onActivityDestroyed(activity: Activity) {
                if (current?.get() === activity) current = null
            }

            override fun onActivityCreated(activity: Activity, savedInstanceState: Bundle?) {}
            override fun onActivityStarted(activity: Activity) {}
            override fun onActivityPaused(activity: Activity) {}
            override fun onActivityStopped(activity: Activity) {}
            override fun onActivitySaveInstanceState(activity: Activity, outState: Bundle) {}
        })
    }

    override suspend fun capture(): TreeCapture? = suspendCancellableCoroutine { cont ->
        val posted = main.post {
            val result = try {
                walk()
            } catch (t: Throwable) {
                null // the hierarchy changed under us; try again on the next tick
            }
            cont.resume(result)
        }
        if (!posted) cont.resume(null)
    }

    private fun walk(): TreeCapture? {
        val activity = current?.get() ?: return null
        val root = activity.window?.decorView ?: return null
        if (root.width <= 0 || root.height <= 0) return null
        val density = root.resources.displayMetrics.density.takeIf { it > 0f } ?: 1f
        root.getLocationOnScreen(location)
        val out = ArrayList<RawNode>(256)
        val context = WalkContext(density, location[0], location[1], out, foreignIds)
        visit(root, 0, context, out)
        foreignIds.endPass()
        return TreeCapture(
            width = (root.width / density).toInt(),
            height = (root.height / density).toInt(),
            nodes = out,
            screen = activity.javaClass.simpleName,
        )
    }

    private fun visit(view: View, parentId: Long, context: WalkContext, out: MutableList<RawNode>) {
        if (out.size >= WalkContext.MAX_NODES) return
        if (view.visibility != View.VISIBLE || view.alpha <= 0f || view.width <= 0 || view.height <= 0) return
        for (walker in subtreeWalkers) {
            if (walker.walk(view, parentId, context)) return
        }
        val id = viewIds.getOrPut(view) { nextViewId++ }
        view.getLocationOnScreen(location)
        val d = context.density
        var text: String? = null
        var secure = false
        val type = when (view) {
            is EditText -> {
                text = view.text?.toString() ?: ""
                secure = isPassword(view)
                NodeType.INPUT
            }
            is Button -> {
                text = view.text?.toString()
                NodeType.BUTTON
            }
            is TextView -> {
                text = view.text?.toString()
                NodeType.TEXT
            }
            is ImageView -> NodeType.IMAGE
            is ScrollView, is HorizontalScrollView, is AbsListView -> NodeType.SCROLL
            is ViewGroup -> if (view.isScrollContainer || view.javaClass.name.endsWith("RecyclerView")) NodeType.SCROLL else NodeType.CONTAINER
            else -> NodeType.OTHER
        }
        out.add(
            RawNode(
                id = id,
                parent = parentId,
                type = type,
                x = ((location[0] - context.originX) / d).toInt(),
                y = ((location[1] - context.originY) / d).toInt(),
                w = (view.width / d).toInt(),
                h = (view.height / d).toInt(),
                color = (view.background as? ColorDrawable)?.color?.takeIf { (it ushr 24) != 0 },
                text = text,
                clickable = view.isClickable,
                focused = view.isFocused,
                mask = ViewMasks.get(view),
                secure = secure,
            ),
        )
        if (view is ViewGroup) {
            for (i in 0 until view.childCount) visit(view.getChildAt(i) ?: continue, id, context, out)
        }
    }

    private fun isPassword(view: EditText): Boolean {
        if (view.transformationMethod is PasswordTransformationMethod) return true
        val type = view.inputType
        val variation = type and InputType.TYPE_MASK_VARIATION
        return when (type and InputType.TYPE_MASK_CLASS) {
            InputType.TYPE_CLASS_TEXT -> variation == InputType.TYPE_TEXT_VARIATION_PASSWORD ||
                variation == InputType.TYPE_TEXT_VARIATION_VISIBLE_PASSWORD ||
                variation == InputType.TYPE_TEXT_VARIATION_WEB_PASSWORD
            InputType.TYPE_CLASS_NUMBER -> variation == InputType.TYPE_NUMBER_VARIATION_PASSWORD
            else -> false
        }
    }

    private fun installTouchHook(activity: Activity) {
        val window = activity.window ?: return
        val callback = window.callback ?: return
        if (callback is TouchCallback) return
        val density = activity.resources.displayMetrics.density.takeIf { it > 0f } ?: 1f
        window.callback = TouchCallback(callback, density)
    }

    private class TouchCallback(private val original: Window.Callback, private val density: Float) : Window.Callback by original {
        override fun dispatchTouchEvent(event: MotionEvent?): Boolean {
            if (event != null) {
                try {
                    val phase = when (event.actionMasked) {
                        MotionEvent.ACTION_DOWN -> TouchPhase.DOWN
                        MotionEvent.ACTION_MOVE -> TouchPhase.MOVE
                        MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL -> TouchPhase.UP
                        else -> null
                    }
                    if (phase != null) Replay.touch(phase, (event.x / density).toInt(), (event.y / density).toInt())
                } catch (t: Throwable) {
                    // never interfere with input dispatch
                }
            }
            return original.dispatchTouchEvent(event)
        }

        // Interface default methods are not covered by Kotlin's `by` delegation: forward them.
        override fun onProvideKeyboardShortcuts(data: MutableList<KeyboardShortcutGroup>?, menu: Menu?, deviceId: Int) {
            if (Build.VERSION.SDK_INT >= 24) original.onProvideKeyboardShortcuts(data, menu, deviceId)
        }

        override fun onPointerCaptureChanged(hasCapture: Boolean) {
            if (Build.VERSION.SDK_INT >= 26) original.onPointerCaptureChanged(hasCapture)
        }
    }
}
