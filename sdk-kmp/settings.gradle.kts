import java.util.Properties

pluginManagement {
    repositories {
        mavenCentral()
        google()
        gradlePluginPortal()
    }
}

dependencyResolutionManagement {
    repositories {
        google()
        mavenCentral()
    }
}

rootProject.name = "izlek-sdk-kmp"

// ---------------------------------------------------------------------------
// Build switches (see README.md). Every switch is a Gradle property that can be
// given with -P on the command line or in gradle.properties / local.properties.
//
//   izlek.android  true|false   default: true when an Android SDK is found
//   izlek.ios      true|false   default: true on macOS hosts
//   izlek.linux    true|false   default: false (linuxX64 verification target)
//   izlek.compose  true|false   default: false (analytics-replay-compose)
// ---------------------------------------------------------------------------

val localProps = Properties().apply {
    val f = file("local.properties")
    if (f.isFile) f.inputStream().use { load(it) }
}

fun switch(name: String): Boolean? =
    (providers.gradleProperty(name).orNull ?: localProps.getProperty(name))?.trim()?.lowercase()?.let { it == "true" }

fun androidSdkPresent(): Boolean {
    val candidates = listOfNotNull(
        localProps.getProperty("sdk.dir"),
        System.getenv("ANDROID_HOME"),
        System.getenv("ANDROID_SDK_ROOT"),
    )
    return candidates.any { File(it, "platforms").isDirectory }
}

val isMac = System.getProperty("os.name").lowercase().contains("mac")

val androidEnabled = switch("izlek.android") ?: androidSdkPresent()
val iosEnabled = switch("izlek.ios") ?: isMac
val linuxEnabled = switch("izlek.linux") ?: false
val composeEnabled = switch("izlek.compose") ?: false

gradle.extra["izlek.android"] = androidEnabled
gradle.extra["izlek.ios"] = iosEnabled
gradle.extra["izlek.linux"] = linuxEnabled
gradle.extra["izlek.compose"] = composeEnabled

logger.lifecycle(
    "izlek targets: jvm=true android=$androidEnabled ios=$iosEnabled linuxX64=$linuxEnabled compose=$composeEnabled"
)

include(":analytics-core")
include(":analytics-flags")
include(":analytics-replay")
include(":analytics-testkit")
include(":sample-jvm")
if (composeEnabled) include(":analytics-replay-compose")
if (androidEnabled) include(":sample-android")
