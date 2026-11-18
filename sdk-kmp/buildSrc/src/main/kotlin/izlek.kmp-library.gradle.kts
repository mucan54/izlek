import com.android.build.gradle.LibraryExtension
import org.jetbrains.kotlin.gradle.ExperimentalKotlinGradlePluginApi
import org.jetbrains.kotlin.gradle.dsl.JvmTarget
import org.jetbrains.kotlin.gradle.tasks.KotlinJvmCompile

// Convention for the multiplatform library modules.
//
// Targets:
//   jvm                      always
//   androidTarget            when izlek.android is on (Android SDK present, or forced)
//   iosArm64 + simulator     when izlek.ios is on (macOS host, or forced)
//   linuxX64                 when izlek.linux is on (verification-only target, not a product target)

plugins {
    id("org.jetbrains.kotlin.multiplatform")
}

val androidEnabled = izlekSwitch("android")
val iosEnabled = izlekSwitch("ios")
// Compose Multiplatform has no linuxX64 artifacts, so the Compose module never gets that target.
val linuxEnabled = izlekSwitch("linux") && project.name != "analytics-replay-compose"

if (androidEnabled) {
    apply(plugin = "com.android.library")
}

kotlin {
    jvm()
    if (androidEnabled) {
        androidTarget {
            publishLibraryVariants("release")
        }
    }
    if (iosEnabled) {
        iosArm64()
        iosSimulatorArm64()
    }
    if (linuxEnabled) {
        linuxX64()
    }

    // Default hierarchy (native > apple > ios, ...) plus `jvmCommon` shared by jvm and android.
    @OptIn(ExperimentalKotlinGradlePluginApi::class)
    applyDefaultHierarchyTemplate {
        common {
            group("jvmCommon") {
                withJvm()
                withAndroidTarget()
            }
        }
    }

    compilerOptions {
        freeCompilerArgs.add("-Xexpect-actual-classes")
    }

    sourceSets.configureEach {
        languageSettings.optIn("dev.izlek.analytics.InternalIzlekApi")
        if (name.endsWith("Test") || project.name == "analytics-testkit") {
            languageSettings.optIn("kotlinx.coroutines.ExperimentalCoroutinesApi")
        }
    }

    sourceSets.named("commonTest") {
        dependencies {
            implementation(kotlin("test"))
            implementation("org.jetbrains.kotlinx:kotlinx-coroutines-test:${IzlekVersions.coroutines}")
        }
    }

    // `src/nativeShared/kotlin` holds POSIX/zlib code that is compiled separately into the iOS and
    // the linuxX64 source sets (instead of a commonized `nativeMain`), so that each side resolves
    // against its own platform libraries.
    val nativeShared = file("src/nativeShared/kotlin")
    sourceSets.matching { it.name == "iosMain" || it.name == "linuxX64Main" }.configureEach {
        kotlin.srcDir(nativeShared)
    }
}

tasks.withType<KotlinJvmCompile>().configureEach {
    compilerOptions.jvmTarget.set(JvmTarget.JVM_11)
}

if (androidEnabled) {
    extensions.configure<LibraryExtension>("android") {
        namespace = "dev.izlek." + project.name.replace('-', '.')
        compileSdk = IzlekVersions.androidCompileSdk
        defaultConfig {
            minSdk = IzlekVersions.androidMinSdk
            val rules = file("consumer-rules.pro")
            if (rules.isFile) consumerProguardFiles(rules)
        }
        compileOptions {
            sourceCompatibility = JavaVersion.VERSION_11
            targetCompatibility = JavaVersion.VERSION_11
        }
    }
}

tasks.withType<Test>().configureEach {
    testLogging {
        events("failed", "skipped")
        showStandardStreams = false
        exceptionFormat = org.gradle.api.tasks.testing.logging.TestExceptionFormat.FULL
    }
}
