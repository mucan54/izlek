package dev.izlek.analytics.replay

import dev.izlek.analytics.InternalIzlekApi

/**
 * Masking rules of design §13.4, applied at capture time, before anything is stored or sent:
 *
 * - text input fields: content replaced by `•`, always (unless explicitly unmasked);
 * - secure (password) fields: always masked, cannot be unmasked;
 * - images: placeholder rectangle only, never any content;
 * - other text: visible by default, masked when `maskAllText` is set;
 * - `analyticsMask()` / `analyticsUnmask()` on a component apply to it and to its subtree
 *   (the nearest ancestor that set a mode wins).
 *
 * Masked text keeps its length (number of code points) so layouts still look right.
 */
@InternalIzlekApi
public object Masking {
    private const val BULLET = '•'

    public fun apply(raw: List<RawNode>, maskAllText: Boolean, maxTextLength: Int = 200): List<ReplayNode> {
        val effective = HashMap<Long, MaskMode>(raw.size * 2)
        val out = ArrayList<ReplayNode>(raw.size)
        for (n in raw) {
            val mode = if (n.mask != MaskMode.INHERIT) n.mask else effective[n.parent] ?: MaskMode.INHERIT
            effective[n.id] = mode
            var text = n.text
            var color = n.color
            var flags = 0
            if (n.type == NodeType.IMAGE) {
                text = null
                color = null
            }
            if (text != null) {
                val masked = when {
                    n.secure -> true
                    mode == MaskMode.MASK -> true
                    mode == MaskMode.UNMASK -> false
                    n.type == NodeType.INPUT -> true
                    else -> maskAllText
                }
                val length = minOf(codePoints(text), maxTextLength)
                if (masked) {
                    text = CharArray(length) { BULLET }.concatToString()
                    flags = flags or NodeFlags.MASKED
                } else if (codePoints(text) > maxTextLength) {
                    text = takeCodePoints(text, maxTextLength)
                }
                flags = flags or NodeFlags.HAS_TEXT
            }
            if (color != null) flags = flags or NodeFlags.HAS_COLOR
            if (n.clickable) flags = flags or NodeFlags.CLICKABLE
            if (n.focused) flags = flags or NodeFlags.FOCUSED
            out.add(ReplayNode(n.id, n.parent, n.type, n.x, n.y, n.w, n.h, flags, color, text))
        }
        return out
    }

    private fun codePoints(s: String): Int {
        var count = 0
        var i = 0
        while (i < s.length) {
            if (s[i].isHighSurrogate() && i + 1 < s.length && s[i + 1].isLowSurrogate()) i++
            i++
            count++
        }
        return count
    }

    private fun takeCodePoints(s: String, n: Int): String {
        var count = 0
        var i = 0
        while (i < s.length && count < n) {
            if (s[i].isHighSurrogate() && i + 1 < s.length && s[i + 1].isLowSurrogate()) i++
            i++
            count++
        }
        return s.substring(0, i)
    }
}
