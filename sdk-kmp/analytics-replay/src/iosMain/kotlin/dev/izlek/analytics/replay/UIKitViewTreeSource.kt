@file:OptIn(ExperimentalForeignApi::class, BetaInteropApi::class)

package dev.izlek.analytics.replay

import kotlinx.cinterop.BetaInteropApi
import kotlinx.cinterop.ExperimentalForeignApi
import kotlinx.cinterop.alloc
import kotlinx.cinterop.memScoped
import kotlinx.cinterop.ptr
import kotlinx.cinterop.useContents
import kotlinx.cinterop.value
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import platform.CoreGraphics.CGFloatVar
import platform.Foundation.NSMapTable
import platform.Foundation.NSNumber
import platform.Foundation.NSStringFromClass
import platform.UIKit.UIApplication
import platform.UIKit.UIButton
import platform.UIKit.UIColor
import platform.UIKit.UIControl
import platform.UIKit.UIImageView
import platform.UIKit.UILabel
import platform.UIKit.UINavigationController
import platform.UIKit.UIScrollView
import platform.UIKit.UITabBarController
import platform.UIKit.UITextField
import platform.UIKit.UITextView
import platform.UIKit.UIView
import platform.UIKit.UIViewController
import platform.UIKit.UIWindow
import platform.objc.object_getClass

/** Masks this view and everything below it in session replay (design §13.4). */
public fun UIView.analyticsMask() {
    UIKitMasks.set(this, MaskMode.MASK)
}

/** Shows the text of this view and of everything below it, even with `maskAllText`. */
public fun UIView.analyticsUnmask() {
    UIKitMasks.set(this, MaskMode.UNMASK)
}

internal object UIKitMasks {
    // Weak keys: a mask request never keeps a view alive. Main thread only.
    private val table = NSMapTable.weakToStrongObjectsMapTable()

    fun set(view: UIView, mode: MaskMode) {
        table.setObject(NSNumber(int = mode.ordinal), forKey = view)
    }

    fun get(view: UIView): MaskMode {
        val n = table.objectForKey(view) as? NSNumber ?: return MaskMode.INHERIT
        return MaskMode.entries.getOrElse(n.intValue) { MaskMode.INHERIT }
    }
}

/**
 * Walks the UIKit view hierarchy of the key window on the main thread.
 *
 * Usage: `Replay.setup(ReplayConfig(), UIKitViewTreeSource())`. Touches are not hooked
 * automatically on iOS; forward them with `Replay.touch(...)` (e.g. from a `UIWindow` subclass
 * overriding `sendEvent`).
 */
public class UIKitViewTreeSource : ViewTreeSource {
    private val ids = StableIds()

    override suspend fun capture(): TreeCapture? = withContext(Dispatchers.Main) {
        try {
            walk()
        } catch (t: Throwable) {
            null
        }
    }

    private fun walk(): TreeCapture? {
        val window = keyWindow() ?: return null
        val out = ArrayList<RawNode>(256)
        visit(window, 0, out)
        ids.endPass()
        var width = 0
        var height = 0
        window.bounds.useContents {
            width = size.width.toInt()
            height = size.height.toInt()
        }
        if (width <= 0 || height <= 0) return null
        return TreeCapture(width, height, out, screenName(window.rootViewController))
    }

    private fun keyWindow(): UIWindow? {
        val app = UIApplication.sharedApplication
        for (w in app.windows) {
            val window = w as? UIWindow ?: continue
            if (window.isKeyWindow() && !window.hidden) return window
        }
        return app.windows.firstOrNull() as? UIWindow
    }

    private fun screenName(root: UIViewController?): String? {
        var vc = root ?: return null
        while (true) {
            val next = vc.presentedViewController
                ?: (vc as? UINavigationController)?.topViewController
                ?: (vc as? UITabBarController)?.selectedViewController
                ?: break
            vc = next
        }
        val cls = object_getClass(vc) ?: return null
        // Swift classes are reported as "Module.ClassName".
        return NSStringFromClass(cls).substringAfterLast('.')
    }

    private fun visit(view: UIView, parentId: Long, out: MutableList<RawNode>) {
        if (out.size >= MAX_NODES) return
        if (view.hidden || view.alpha < 0.01) return
        var x = 0
        var y = 0
        var w = 0
        var h = 0
        // toView = null converts to the coordinate system of the view's window.
        view.convertRect(view.bounds, toView = null).useContents {
            x = origin.x.toInt()
            y = origin.y.toInt()
            w = size.width.toInt()
            h = size.height.toInt()
        }
        if (w <= 0 || h <= 0) return
        // The object address identifies a live view; StableIds maps it to a small id and forgets
        // addresses that were not seen in a pass (so a reused address gets a fresh id).
        val id = ids.idFor(view.hash)
        var text: String? = null
        var secure = false
        val type = when (view) {
            is UITextField -> {
                text = view.text ?: ""
                secure = view.secureTextEntry
                NodeType.INPUT
            }
            is UITextView -> {
                text = view.text
                secure = view.secureTextEntry
                if (view.editable) NodeType.INPUT else NodeType.TEXT
            }
            is UIButton -> {
                text = view.currentTitle
                NodeType.BUTTON
            }
            is UILabel -> {
                text = view.text
                NodeType.TEXT
            }
            is UIImageView -> NodeType.IMAGE
            is UIScrollView -> NodeType.SCROLL
            is UIControl -> NodeType.OTHER
            else -> NodeType.CONTAINER
        }
        out.add(
            RawNode(
                id = id,
                parent = parentId,
                type = type,
                x = x,
                y = y,
                w = w,
                h = h,
                color = view.backgroundColor?.let { argb(it) },
                text = text,
                clickable = (view is UIControl && view.enabled) || !view.gestureRecognizers.isNullOrEmpty(),
                focused = view.isFirstResponder(),
                mask = UIKitMasks.get(view),
                secure = secure,
            ),
        )
        for (child in view.subviews) visit(child as? UIView ?: continue, id, out)
    }

    /** `0xAARRGGBB`, or null for fully transparent / non-RGB colors. */
    private fun argb(color: UIColor): Int? = memScoped {
        val r = alloc<CGFloatVar>()
        val g = alloc<CGFloatVar>()
        val b = alloc<CGFloatVar>()
        val a = alloc<CGFloatVar>()
        if (!color.getRed(r.ptr, green = g.ptr, blue = b.ptr, alpha = a.ptr)) return@memScoped null
        fun channel(v: Double): Int = (v * 255.0 + 0.5).toInt().coerceIn(0, 255)
        val alpha = channel(a.value)
        if (alpha == 0) null else (alpha shl 24) or (channel(r.value) shl 16) or (channel(g.value) shl 8) or channel(b.value)
    }

    private companion object {
        const val MAX_NODES = 2_000
    }
}
