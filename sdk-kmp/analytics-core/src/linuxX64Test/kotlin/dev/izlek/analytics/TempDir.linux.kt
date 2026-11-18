@file:OptIn(kotlinx.cinterop.ExperimentalForeignApi::class)

package dev.izlek.analytics

import dev.izlek.analytics.platform.currentTimeMillis
import kotlinx.cinterop.convert
import platform.posix.getpid
import platform.posix.mkdir

private var counter = 0

internal actual fun createTempDir(): String {
    val dir = "/tmp/izlek-test-${getpid()}-${currentTimeMillis()}-${counter++}"
    check(mkdir(dir, 0x1C0.convert()) == 0) { "cannot create $dir" }
    return dir
}
