// Minimal Android application skeleton. Only part of the build when the Android target is
// enabled (see settings.gradle.kts). Shows the three setup calls; it has no UI resources.
plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "dev.izlek.sample.android"
    compileSdk = IzlekVersions.androidCompileSdk
    defaultConfig {
        applicationId = "dev.izlek.sample.android"
        minSdk = IzlekVersions.androidMinSdk
        targetSdk = IzlekVersions.androidCompileSdk
        versionCode = 1
        versionName = "0.1.0"
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_11
        targetCompatibility = JavaVersion.VERSION_11
    }
    buildTypes {
        release {
            // R8 on, to show that the SDK needs no keep rules.
            isMinifyEnabled = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"))
            signingConfig = signingConfigs.getByName("debug")
        }
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_11)
    }
}

dependencies {
    implementation(project(":analytics-core"))
    implementation(project(":analytics-flags"))
    implementation(project(":analytics-replay"))
}
