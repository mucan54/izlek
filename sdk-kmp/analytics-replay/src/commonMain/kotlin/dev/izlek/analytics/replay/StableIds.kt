package dev.izlek.analytics.replay

/**
 * Hands out small, stable node ids (≥ 1) for arbitrary keys (a view address, a semantics id, ...).
 *
 * A key keeps its id for as long as it is seen in every capture pass; keys that were not seen in
 * a pass are forgotten when [endPass] is called. Not thread-safe (walkers run on the UI thread).
 */
public class StableIds {
    private val ids = HashMap<Any, Long>()
    private var seen = HashSet<Any>()
    private var next = 1L

    public fun idFor(key: Any): Long {
        seen.add(key)
        return ids.getOrPut(key) { next++ }
    }

    public fun endPass() {
        ids.keys.retainAll(seen)
        seen = HashSet()
    }
}
