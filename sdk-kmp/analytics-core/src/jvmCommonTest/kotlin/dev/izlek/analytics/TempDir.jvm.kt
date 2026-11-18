package dev.izlek.analytics

import java.io.File

internal actual fun createTempDir(): String {
    val dir = File(System.getProperty("java.io.tmpdir"), "izlek-test-" + System.nanoTime())
    check(dir.mkdirs()) { "cannot create $dir" }
    dir.deleteOnExit()
    return dir.absolutePath
}
