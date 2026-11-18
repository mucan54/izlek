package dev.izlek.analytics

/**
 * JVM (desktop/server/CLI) platform handle.
 *
 * @param storageDir directory for the queue; defaults to `~/.izlek`
 * @param appVersion reported as `app_version`
 * @param appBuild reported as `app_build`
 */
public actual class PlatformContext(
    public val storageDir: String? = null,
    public val appVersion: String? = null,
    public val appBuild: String? = null,
)
