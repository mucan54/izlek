@file:OptIn(ExperimentalForeignApi::class, BetaInteropApi::class)

package dev.izlek.analytics.platform

import dev.izlek.analytics.PlatformContext
import kotlinx.cinterop.BetaInteropApi
import kotlinx.cinterop.ExperimentalForeignApi
import kotlinx.cinterop.addressOf
import kotlinx.cinterop.alloc
import kotlinx.cinterop.convert
import kotlinx.cinterop.memScoped
import kotlinx.cinterop.ptr
import kotlinx.cinterop.toKString
import kotlinx.cinterop.useContents
import kotlinx.cinterop.usePinned
import kotlinx.coroutines.suspendCancellableCoroutine
import platform.Foundation.NSApplicationSupportDirectory
import platform.Foundation.NSBundle
import platform.Foundation.NSData
import platform.Foundation.NSDate
import platform.Foundation.NSHTTPURLResponse
import platform.Foundation.NSLocale
import platform.Foundation.NSMutableURLRequest
import platform.Foundation.NSNotificationCenter
import platform.Foundation.NSOperationQueue
import platform.Foundation.NSSearchPathForDirectoriesInDomains
import platform.Foundation.NSThread
import platform.Foundation.NSTimeZone
import platform.Foundation.NSURL
import platform.Foundation.NSURLRequestReloadIgnoringLocalCacheData
import platform.Foundation.NSURLSession
import platform.Foundation.NSUserDomainMask
import platform.Foundation.create
import platform.Foundation.currentLocale
import platform.Foundation.dataTaskWithRequest
import platform.Foundation.localTimeZone
import platform.Foundation.localeIdentifier
import platform.Foundation.setHTTPBody
import platform.Foundation.setHTTPMethod
import platform.Foundation.setHTTPShouldHandleCookies
import platform.Foundation.setValue
import platform.Foundation.timeIntervalSince1970
import platform.Security.SecRandomCopyBytes
import platform.Security.errSecSuccess
import platform.Security.kSecRandomDefault
import platform.UIKit.UIApplication
import platform.UIKit.UIApplicationDidEnterBackgroundNotification
import platform.UIKit.UIApplicationState
import platform.UIKit.UIApplicationWillEnterForegroundNotification
import platform.UIKit.UIBackgroundTaskInvalid
import platform.UIKit.UIDevice
import platform.UIKit.UIScreen
import platform.UIKit.UIUserInterfaceIdiomPad
import platform.UIKit.UIUserInterfaceIdiomPhone
import platform.UIKit.UIUserInterfaceIdiomTV
import platform.posix.memcpy
import platform.posix.uname
import platform.posix.utsname
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

private const val REQUEST_TIMEOUT_SECONDS = 30.0

internal actual suspend fun httpRequest(
    method: String,
    url: String,
    headers: Map<String, String>,
    body: ByteArray?,
): HttpResponse = suspendCancellableCoroutine { cont ->
    val nsUrl = NSURL.URLWithString(url)
    if (nsUrl == null) {
        cont.resumeWithException(FileIoException("bad url"))
        return@suspendCancellableCoroutine
    }
    val request = NSMutableURLRequest.requestWithURL(nsUrl)
    request.setHTTPMethod(method)
    request.setTimeoutInterval(REQUEST_TIMEOUT_SECONDS)
    request.setCachePolicy(NSURLRequestReloadIgnoringLocalCacheData)
    request.setHTTPShouldHandleCookies(false)
    for ((k, v) in headers) request.setValue(v, forHTTPHeaderField = k)
    if (body != null) request.setHTTPBody(body.toNSData())
    val task = NSURLSession.sharedSession.dataTaskWithRequest(request) { data, response, error ->
        val http = response as? NSHTTPURLResponse
        if (error != null || http == null) {
            cont.resumeWithException(FileIoException("network error: ${error?.localizedDescription ?: "no response"}"))
        } else {
            val responseHeaders = HashMap<String, String>()
            for ((k, v) in http.allHeaderFields) {
                if (k != null && v != null) responseHeaders[k.toString().lowercase()] = v.toString()
            }
            cont.resume(HttpResponse(http.statusCode.toInt(), responseHeaders, data?.toByteArray() ?: ByteArray(0)))
        }
    }
    cont.invokeOnCancellation { task.cancel() }
    task.resume()
}

private fun ByteArray.toNSData(): NSData {
    if (isEmpty()) return NSData()
    return usePinned { NSData.create(bytes = it.addressOf(0), length = size.convert()) }
}

private fun NSData.toByteArray(): ByteArray {
    val n = length.toInt()
    val out = ByteArray(n)
    if (n > 0) out.usePinned { memcpy(it.addressOf(0), bytes, length) }
    return out
}

internal actual fun storageDir(context: PlatformContext): String {
    val dirs = NSSearchPathForDirectoriesInDomains(NSApplicationSupportDirectory, NSUserDomainMask, true)
    return dirs.firstOrNull() as? String ?: "."
}

internal actual fun lifecycle(context: PlatformContext, onBackground: () -> Unit, onForeground: () -> Unit) {
    val center = NSNotificationCenter.defaultCenter
    val queue = NSOperationQueue.mainQueue
    // The observers live as long as the process; the SDK is never torn down in production.
    center.addObserverForName(UIApplicationDidEnterBackgroundNotification, null, queue) { _ -> onBackground() }
    center.addObserverForName(UIApplicationWillEnterForegroundNotification, null, queue) { _ -> onForeground() }
}

internal actual fun isAppInForeground(context: PlatformContext): Boolean {
    // UIApplication state may only be read on the main thread.
    if (!NSThread.isMainThread) return true
    return UIApplication.sharedApplication.applicationState != UIApplicationState.UIApplicationStateBackground
}

internal actual fun deviceContextProvider(context: PlatformContext): () -> Map<String, Any?> {
    val m = LinkedHashMap<String, Any?>()
    m["platform"] = "ios"
    val device = UIDevice.currentDevice
    m["os"] = device.systemName
    m["os_version"] = device.systemVersion
    m["device_model"] = machineName() ?: device.model
    m["device_type"] = when (device.userInterfaceIdiom) {
        UIUserInterfaceIdiomPad -> "tablet"
        UIUserInterfaceIdiomPhone -> "phone"
        UIUserInterfaceIdiomTV -> "tv"
        else -> "other"
    }
    val bundle = NSBundle.mainBundle
    (bundle.objectForInfoDictionaryKey("CFBundleShortVersionString") as? String)?.let { m["app_version"] = it }
    (bundle.objectForInfoDictionaryKey("CFBundleVersion") as? String)?.let { m["app_build"] = it }
    m["locale"] = NSLocale.currentLocale.localeIdentifier.replace('_', '-')
    m["timezone"] = NSTimeZone.localTimeZone.name
    if (NSThread.isMainThread) {
        UIScreen.mainScreen.nativeBounds.useContents {
            m["screen_w"] = size.width.toLong()
            m["screen_h"] = size.height.toLong()
        }
    }
    // `network` (wifi/cellular) is not reported on iOS in this version (would need NWPathMonitor).
    return { m }
}

/** Hardware identifier such as "iPhone15,2". */
private fun machineName(): String? = memScoped {
    val info = alloc<utsname>()
    if (uname(info.ptr) != 0) null else info.machine.toKString().ifEmpty { null }
}

internal actual fun randomBytes(n: Int): ByteArray {
    val out = ByteArray(n)
    if (n == 0) return out
    val status = out.usePinned { SecRandomCopyBytes(kSecRandomDefault, n.convert(), it.addressOf(0)) }
    if (status != errSecSuccess) throw IllegalStateException("SecRandomCopyBytes failed: $status")
    return out
}

internal actual fun currentTimeMillis(): Long = (NSDate().timeIntervalSince1970 * 1000.0).toLong()

/**
 * Asks UIKit for extra time when the app goes to the background while a flush is running.
 * Not available in app extensions.
 */
internal actual fun beginBackgroundTask(name: String): () -> Unit {
    val app = UIApplication.sharedApplication
    var id = UIBackgroundTaskInvalid
    val end = {
        if (id != UIBackgroundTaskInvalid) {
            app.endBackgroundTask(id)
            id = UIBackgroundTaskInvalid
        }
    }
    id = app.beginBackgroundTaskWithName(name) { end() }
    return end
}
