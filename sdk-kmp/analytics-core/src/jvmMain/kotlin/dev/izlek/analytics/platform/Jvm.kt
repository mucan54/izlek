package dev.izlek.analytics.platform

import dev.izlek.analytics.PlatformContext
import java.io.File
import java.util.Locale
import java.util.TimeZone

internal actual fun storageDir(context: PlatformContext): String =
    context.storageDir ?: File(System.getProperty("user.home") ?: ".", ".izlek").absolutePath

// A plain JVM process has no background/foreground notion: it is always "foreground".
internal actual fun lifecycle(context: PlatformContext, onBackground: () -> Unit, onForeground: () -> Unit) {}

internal actual fun isAppInForeground(context: PlatformContext): Boolean = true

internal actual fun deviceContextProvider(context: PlatformContext): () -> Map<String, Any?> {
    val m = LinkedHashMap<String, Any?>()
    m["platform"] = "jvm"
    m["os"] = System.getProperty("os.name") ?: "unknown"
    m["os_version"] = System.getProperty("os.version") ?: ""
    m["device_type"] = "desktop"
    context.appVersion?.let { m["app_version"] = it }
    context.appBuild?.let { m["app_build"] = it }
    m["locale"] = Locale.getDefault().toLanguageTag()
    m["timezone"] = TimeZone.getDefault().id
    return { m }
}

internal actual fun beginBackgroundTask(name: String): () -> Unit = {}
