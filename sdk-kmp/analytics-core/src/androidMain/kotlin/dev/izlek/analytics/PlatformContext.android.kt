package dev.izlek.analytics

import android.content.Context

/** Android platform handle: `PlatformContext(context)`. Only the application context is kept. */
public actual class PlatformContext(context: Context) {
    public val context: Context = context.applicationContext ?: context
}
