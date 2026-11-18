plugins {
    `kotlin-dsl`
}

repositories {
    mavenCentral()
    google()
    gradlePluginPortal()
}

// Versions are pinned here (single place). See README.md.
val kotlinVersion = "2.2.21"
val agpVersion = "8.11.1"

dependencies {
    implementation("org.jetbrains.kotlin:kotlin-gradle-plugin:$kotlinVersion")
    implementation("com.android.tools.build:gradle:$agpVersion")
}
