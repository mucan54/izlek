package dev.izlek.analytics

/**
 * linuxX64 is a verification-only target: it exists so that the common logic, the POSIX file
 * layer and the zlib gzip code (shared with iOS) can be executed by tests on a Linux host.
 */
public actual class PlatformContext(public val storageDir: String = "/tmp/izlek")
