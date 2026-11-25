// Test support shared by the commonTest source sets of all modules: generated spec vectors,
// an LA1/LAR1 decoder, and fakes (file system, HTTP, clock). Not published.
plugins {
    id("izlek.kmp-library")
}

val generateSpecVectors = tasks.register<GenerateSpecVectorsTask>("generateSpecVectors") {
    val dir = rootProject.layout.projectDirectory.dir("../spec/vectors")
    vectorFiles.from(
        dir.file("wire.json"),
        dir.file("murmur3.json"),
        dir.file("flags.json"),
        dir.file("replay.json"),
    )
    packageName.set("dev.izlek.analytics.testkit")
    outputDir.set(layout.buildDirectory.dir("generated/specVectors/kotlin"))
}

kotlin {
    sourceSets {
        commonMain {
            kotlin.srcDir(generateSpecVectors)
            dependencies {
                api(project(":analytics-core"))
                api("org.jetbrains.kotlinx:kotlinx-coroutines-test:${IzlekVersions.coroutines}")
            }
        }
    }
}

// Test support only: never published.
tasks.withType<AbstractPublishToMaven>().configureEach { enabled = false }
