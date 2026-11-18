// Command line sample: sends a few events to a given host/token. Useful for end-to-end tests.
//
//   ./gradlew :sample-jvm:run --args="--host http://localhost:8080 --token pk_dev --events 5 --user user-1"
plugins {
    id("org.jetbrains.kotlin.jvm")
    application
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_11)
        optIn.add("dev.izlek.analytics.InternalIzlekApi")
    }
}

java {
    sourceCompatibility = JavaVersion.VERSION_11
    targetCompatibility = JavaVersion.VERSION_11
}

dependencies {
    implementation(project(":analytics-core"))
    implementation(project(":analytics-flags"))
}

application {
    mainClass.set("dev.izlek.sample.MainKt")
}
