# sample-ios (iskelet)

Bu dizin derlenen bir Gradle modülü **değildir**; yalnızca iOS entegrasyonunun nasıl görüneceğini
gösteren Swift iskeletidir. Linux'ta Xcode olmadığı için **hiç derlenmedi ve çalıştırılmadı**.

1. Uygulamanızın `shared` KMP modülünde SDK modüllerini framework'e dışa aktarın:

   ```kotlin
   kotlin {
       listOf(iosArm64(), iosSimulatorArm64()).forEach { target ->
           target.binaries.framework {
               baseName = "Shared"
               export("dev.izlek:analytics-core:0.1.0")
               export("dev.izlek:analytics-flags:0.1.0")
               export("dev.izlek:analytics-replay:0.1.0")
           }
       }
       sourceSets.commonMain.dependencies {
           api("dev.izlek:analytics-core:0.1.0")
           api("dev.izlek:analytics-flags:0.1.0")
           api("dev.izlek:analytics-replay:0.1.0")
       }
   }
   ```

2. `analytics-core/src/iosMain/resources/PrivacyInfo.xcprivacy` dosyasını framework/uygulama
   paketine kaynak olarak ekleyin (ya da içeriğini uygulamanızın privacy manifest'i ile birleştirin).

3. `SampleApp.swift` dosyasındaki gibi `Analytics.shared.setup(...)` çağrısını ana iş parçacığında,
   uygulama açılışında yapın.
