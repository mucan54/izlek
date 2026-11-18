plugins {
    id("izlek.kmp-library")
}

kotlin {
    sourceSets {
        commonMain.dependencies {
            api(project(":analytics-core"))
        }
        commonTest.dependencies {
            implementation(project(":analytics-testkit"))
        }
    }
}
