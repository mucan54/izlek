package dev.izlek.analytics.platform

import android.app.Activity
import android.app.ActivityManager
import android.app.Application
import android.content.Context
import android.content.res.Configuration
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.os.Build
import android.os.Bundle
import dev.izlek.analytics.PlatformContext
import java.util.Locale
import java.util.TimeZone

internal actual fun storageDir(context: PlatformContext): String = context.context.filesDir.absolutePath

/**
 * Process-wide foreground tracking with `ActivityLifecycleCallbacks` (no androidx dependency):
 * the app is in the foreground while at least one activity is started. Configuration changes
 * (stop immediately followed by start) do not count as background.
 */
internal actual fun lifecycle(context: PlatformContext, onBackground: () -> Unit, onForeground: () -> Unit) {
    val app = context.context.applicationContext as? Application ?: return
    app.registerActivityLifecycleCallbacks(object : Application.ActivityLifecycleCallbacks {
        private var started = 0
        private var changingConfiguration = false

        override fun onActivityStarted(activity: Activity) {
            started++
            if (started == 1 && !changingConfiguration) onForeground()
            changingConfiguration = false
        }

        override fun onActivityStopped(activity: Activity) {
            // Activities started before registration are not counted, hence the clamp.
            started = if (started > 0) started - 1 else 0
            if (started == 0) {
                changingConfiguration = activity.isChangingConfigurations
                if (!changingConfiguration) onBackground()
            }
        }

        override fun onActivityCreated(activity: Activity, savedInstanceState: Bundle?) {}
        override fun onActivityResumed(activity: Activity) {}
        override fun onActivityPaused(activity: Activity) {}
        override fun onActivitySaveInstanceState(activity: Activity, outState: Bundle) {}
        override fun onActivityDestroyed(activity: Activity) {}
    })
}

internal actual fun isAppInForeground(context: PlatformContext): Boolean = try {
    val info = ActivityManager.RunningAppProcessInfo()
    ActivityManager.getMyMemoryState(info)
    info.importance <= ActivityManager.RunningAppProcessInfo.IMPORTANCE_FOREGROUND
} catch (t: Throwable) {
    true
}

internal actual fun deviceContextProvider(context: PlatformContext): () -> Map<String, Any?> {
    val ctx = context.context
    val static = LinkedHashMap<String, Any?>()
    static["platform"] = "android"
    static["os"] = "Android"
    static["os_version"] = Build.VERSION.RELEASE ?: ""
    static["device_model"] = listOfNotNull(Build.MANUFACTURER, Build.MODEL).joinToString(" ").trim()
    try {
        val config = ctx.resources.configuration
        val uiMode = config.uiMode and Configuration.UI_MODE_TYPE_MASK
        static["device_type"] = when {
            uiMode == Configuration.UI_MODE_TYPE_TELEVISION -> "tv"
            uiMode == Configuration.UI_MODE_TYPE_WATCH -> "watch"
            config.smallestScreenWidthDp >= 600 -> "tablet"
            else -> "phone"
        }
        val metrics = ctx.resources.displayMetrics
        static["screen_w"] = metrics.widthPixels
        static["screen_h"] = metrics.heightPixels
    } catch (t: Throwable) {
        // leave out what cannot be read
    }
    try {
        @Suppress("DEPRECATION")
        val info = ctx.packageManager.getPackageInfo(ctx.packageName, 0)
        info.versionName?.let { static["app_version"] = it }
        @Suppress("DEPRECATION")
        val code = if (Build.VERSION.SDK_INT >= 28) info.longVersionCode else info.versionCode.toLong()
        static["app_build"] = code.toString()
    } catch (t: Throwable) {
        // package info unavailable
    }
    static["locale"] = Locale.getDefault().toLanguageTag()
    static["timezone"] = TimeZone.getDefault().id
    return {
        val network = networkType(ctx)
        if (network == null) static else LinkedHashMap(static).also { it["network"] = network }
    }
}

/** "wifi" / "cellular" / "other" / "none"; null when the state cannot be read (no permission). */
private fun networkType(ctx: Context): String? = try {
    val cm = ctx.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager
    if (cm == null) {
        null
    } else if (Build.VERSION.SDK_INT >= 23) {
        val caps = cm.activeNetwork?.let { cm.getNetworkCapabilities(it) }
        when {
            caps == null -> "none"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "wifi"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "cellular"
            else -> "other"
        }
    } else {
        @Suppress("DEPRECATION")
        val info = cm.activeNetworkInfo
        @Suppress("DEPRECATION")
        when {
            info == null || !info.isConnected -> "none"
            info.type == ConnectivityManager.TYPE_WIFI -> "wifi"
            info.type == ConnectivityManager.TYPE_MOBILE -> "cellular"
            else -> "other"
        }
    }
} catch (t: Throwable) {
    null
}

// Android gives a process a short grace period after onStop; no explicit background task API is
// used (WorkManager would add a dependency). See design §19, assumption 8.
internal actual fun beginBackgroundTask(name: String): () -> Unit = {}
