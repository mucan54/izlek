import org.gradle.api.Project
import org.gradle.api.plugins.ExtensionAware

/** Pinned dependency versions shared by all modules. */
object IzlekVersions {
    const val coroutines = "1.10.2"
    const val androidCompileSdk = 35
    const val androidMinSdk = 21
}

/** Reads a build switch computed in settings.gradle.kts (izlek.android / izlek.ios / izlek.linux / izlek.compose). */
fun Project.izlekSwitch(name: String): Boolean =
    (gradle as ExtensionAware).extensions.extraProperties.get("izlek.$name") as Boolean
