// Optional module (enabled with -Pizlek.compose=true): Compose semantics-tree walker for session
// replay plus Modifier.analyticsMask() / analyticsUnmask(). Kept out of the default build so that
// analytics-replay itself has no Compose dependency.
plugins {
    id("izlek.kmp-library")
    id("org.jetbrains.kotlin.plugin.compose") version "2.2.21"
    id("org.jetbrains.compose") version "1.9.3"
}

kotlin {
    sourceSets {
        commonMain.dependencies {
            api(project(":analytics-replay"))
            implementation(compose.runtime)
            implementation(compose.ui)
        }
        jvmTest.dependencies {
            implementation(compose.foundation)
            implementation(compose.desktop.currentOs)
            @OptIn(org.jetbrains.compose.ExperimentalComposeLibrary::class)
            implementation(compose.uiTest)
        }
    }
}
