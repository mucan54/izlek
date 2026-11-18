plugins {
    id("izlek.kmp-library")
}

kotlin {
    sourceSets {
        commonMain.dependencies {
            // The only runtime dependency of analytics-core (design §4.1).
            api("org.jetbrains.kotlinx:kotlinx-coroutines-core:${IzlekVersions.coroutines}")
        }
        commonTest.dependencies {
            implementation(project(":analytics-testkit"))
        }
    }
}
