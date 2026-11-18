@file:OptIn(kotlinx.cinterop.ExperimentalForeignApi::class)

package dev.izlek.analytics

import platform.Foundation.NSFileManager
import platform.Foundation.NSTemporaryDirectory
import platform.Foundation.NSUUID

internal actual fun createTempDir(): String {
    val dir = NSTemporaryDirectory().trimEnd('/') + "/izlek-test-" + NSUUID().UUIDString
    NSFileManager.defaultManager.createDirectoryAtPath(dir, withIntermediateDirectories = true, attributes = null, error = null)
    return dir
}
