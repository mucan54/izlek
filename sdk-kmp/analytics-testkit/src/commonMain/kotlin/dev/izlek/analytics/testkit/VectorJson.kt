package dev.izlek.analytics.testkit

import dev.izlek.analytics.internal.Json

/** Typed access helpers for parsed vector JSON. */
public object VectorJson {
    @Suppress("UNCHECKED_CAST")
    public fun obj(v: Any?): Map<String, Any?> = v as Map<String, Any?>

    @Suppress("UNCHECKED_CAST")
    public fun list(v: Any?): List<Any?> = v as List<Any?>

    public fun wire(): Map<String, Any?> = obj(Json.parse(SpecVectors.wireJson))
    public fun murmur3(): Map<String, Any?> = obj(Json.parse(SpecVectors.murmur3Json))
    public fun flags(): Map<String, Any?> = obj(Json.parse(SpecVectors.flagsJson))
    public fun replay(): Map<String, Any?> = obj(Json.parse(SpecVectors.replayJson))
}
