# izlek KMP SDK

Kotlin Multiplatform analitik SDK'sı: event toplama, kimlik, dosya tabanlı kuyruk, feature flag
değerlendirme ve wireframe tabanlı session replay. Normatif kaynaklar: `docs/design.md` §3, §4,
§12, §13 ve `spec/` (`wire.md`, `flags.md`, `replay.md`, `vectors/`).

## Modüller

| Modül | İçerik | Bağımlılık |
|---|---|---|
| `analytics-core` | `Analytics`, `Config`, LA1 kodlayıcı, dosya kuyruğu, gönderici, kimlik/oturum, platform katmanı | yalnızca `kotlinx-coroutines-core` |
| `analytics-flags` | `Flags`: kural seti indirme (ETag), disk kopyası, yerel değerlendirme, `flag_exposed` | `core` |
| `analytics-replay` | `Replay`: LAR1 kodlayıcı, kare zamanlayıcı, ağaç farkı, maskeleme, yükleme; Android View ve UIKit gezginleri | `core` |
| `analytics-replay-compose` | Compose semantics gezgini, `Modifier.analyticsMask()/analyticsUnmask()` (isteğe bağlı) | `replay` + Compose Multiplatform |
| `analytics-testkit` | Testler için: gömülü spec vektörleri, LA1 çözücü, sahte disk/HTTP/saat (yayınlanmaz) | `core` |
| `sample-jvm` | Komut satırı örneği (uçtan uca deneme için) | `core`, `flags` |
| `sample-android` | Android uygulama iskeleti (Android hedefi açıksa derlenir) | |
| `sample-ios` | Swift iskeleti, Gradle modülü değildir (derlenmedi) | |

Kaynak kümeleri: `commonMain` (tüm mantık), `jvmCommonMain` (JVM + Android ortak `java.*`
kodu), `jvmMain`, `androidMain`, `iosMain`, `nativeShared` (POSIX dosya katmanı ve zlib; hem
`iosMain` hem `linuxX64Main` içine derlenir), `linuxX64Main` (yalnızca doğrulama hedefi).

## Entegrasyon

Android (`Application.onCreate`):

```kotlin
Analytics.setup(Config(token = "pk_...", host = "https://in.example.com"), PlatformContext(this))
Flags.setup()                                                           // isteğe bağlı
Replay.setup(ReplayConfig(sampleRate = 0.1), AndroidViewTreeSource(this)) // isteğe bağlı
// Compose kullanan uygulamada: AndroidViewTreeSource(this).withCompose()

Analytics.capture("purchase", mapOf("amount" to 12.5, "items" to listOf(1, 2)))
Analytics.screen("Checkout")
Analytics.identify("user-42", mapOf("plan" to "pro"))
if (Flags.isEnabled("new_checkout")) { /* ... */ }
```

iOS (Swift, ana iş parçacığında; ayrıntı için `sample-ios/`):

```swift
Analytics.shared.setup(config: Config(token: "pk_...", host: "https://in.example.com", ...),
                       context: PlatformContext())
Replay.shared.setup(config: ReplayConfig(...), source: UIKitViewTreeSource())
```

`PrivacyInfo.xcprivacy` dosyası `analytics-core/src/iosMain/resources/` altındadır; framework'ü
paketlerken kaynak olarak eklenmelidir. Android tüketici ProGuard kuralı gerekmez
(`analytics-core/consumer-rules.pro` bilerek boştur; SDK yansıma kullanmaz). Android'de SDK
manifest'i `INTERNET` ve (yalnızca `network` bağlam alanı için) `ACCESS_NETWORK_STATE` izinlerini ekler.

JVM: `Analytics.setup(config, PlatformContext(storageDir = "...", appVersion = "1.0"))`.

`Flags.flag(key, default)` → `true` | varyant anahtarı | `default`. `Flags.isEnabled`, `Flags.variant`
yardımcılardır. Kimlik değişikliği (`identify`/`reset`) flag değerlendirmesine SDK iş parçacığı
komutu işledikten sonra yansır (asenkron).

## Derleme anahtarları

| Anahtar (`-P...` ya da `local.properties`) | Varsayılan | Etki |
|---|---|---|
| `izlek.android=true\|false` | Android SDK bulunursa açık (`ANDROID_HOME`, `ANDROID_SDK_ROOT`, `local.properties: sdk.dir`) | `androidTarget` + `sample-android` |
| `izlek.ios=true\|false` | macOS'ta açık | `iosArm64`, `iosSimulatorArm64` |
| `izlek.linux=true\|false` | kapalı | `linuxX64` doğrulama hedefi (ürün hedefi değildir; HTTP taşıması yoktur) |
| `izlek.compose=true\|false` | kapalı | `analytics-replay-compose` modülü |
| `izlek.publishDir=<dizin>` | `build/maven` | `publishAllPublicationsToIzlekRepository` görevinin yazdığı düz Maven deposu (açık olan tüm hedefler; `analytics-testkit` yayınlanmaz) |

Android SDK'sı ve macOS olmayan bir makinede düz `./gradlew build` yalnızca `jvm` hedefini derler
ve test eder. Sabitlenen sürümler: Gradle 8.14.3 (wrapper), Kotlin 2.2.21, AGP 8.11.1,
kotlinx-coroutines 1.10.2, compileSdk 35 / minSdk 21, Compose Multiplatform 1.9.3 (yalnızca
compose modülü). Sürümler `buildSrc/build.gradle.kts` ve `buildSrc/src/main/kotlin/IzlekBuild.kt`
içindedir.

## Testler

```
./gradlew jvmTest                                   # tüm modüller, JVM (vektörler, kuyruk, gerçek HTTP entegrasyonu)
./gradlew build                                     # derleme + testler (açık olan hedefler)
./gradlew linuxX64Test -Pizlek.linux=true           # ortak testler Kotlin/Native üzerinde (POSIX dosya katmanı, zlib, Regex)
./gradlew testDebugUnitTest                         # Android SDK varsa: ortak testler Android birim testi olarak
./gradlew compileKotlinIosArm64 compileKotlinIosSimulatorArm64 -Pizlek.ios=true   # Linux'ta yalnızca klib derlemesi
./gradlew :analytics-replay-compose:jvmTest -Pizlek.compose=true                    # Compose semantics prototipi (masaüstü, ekransız)
./gradlew :sample-jvm:run --args="--host http://localhost:8080 --token pk_dev --events 5 --user u1"
```

Vektörler: `analytics-testkit`'teki `generateSpecVectors` görevi `../spec/vectors/*.json`
dosyalarını derleme anında bir Kotlin kaynağına gömer; testler JSON'u SDK'nın kendi ayrıştırıcısıyla
okur. Elle kopyalanmış hex yoktur; tek doğruluk kaynağı `spec/vectors/` dizinidir.

## Hedef başına doğrulama durumu

| Hedef | Derleme | Test | Not |
|---|---|---|---|
| `jvm` | evet | evet (tüm testler, gerçek `HttpURLConnection` + gzip + dosya sistemi dahil) | tam doğrulandı |
| `android` | evet (debug + release, `sample-android` R8'li APK dahil) | ortak testler JVM üzerinde birim testi olarak | `analytics-core` bir uygulamada (Magazam, Compose, AGP 9 / Kotlin 2.4 ile tüketilen yayın) Android 15 emülatöründe uçtan uca çalıştı (2026-11-25: `capture`, `screen`, yaşam döngüsü olayları, arka planda gönderim). View gezgini, dokunma kancası ve `flags`/`replay` modülleri cihazda çalıştırılmadı |
| `iosArm64`, `iosSimulatorArm64` | klib; macOS'ta bir uygulamanın framework'üne bağlandı | hayır (SDK testleri) | `analytics-core` bir uygulamanın statik framework'üne bağlanıp iOS simülatöründe uçtan uca çalıştı (2026-11-25, Magazam: NSURLSession gönderimi, yaşam döngüsü bildirimleri, arka planda gönderim). Gerçek cihaz, UIKit gezgini ve `flags`/`replay` çalıştırılmadı |
| `linuxX64` | evet | evet (ortak testler yerel ikili olarak) | yalnızca doğrulama amaçlı |
| Compose modülü | jvm (masaüstü), android, ios klib | yalnızca jvm (masaüstü, ekransız) | Android/iOS üzerinde çalıştırılmadı |

## Davranış özeti

**Kuyruk (design §4.3).** Dizin `<uygulama verisi>/analytics/<token-hash>/`. Dosyalar: `state`
(JSON), `active`, `pending-<seq:12>-<uuidv7 hex>[-0|-1...]`. Segment dosyası biçimi:

```
Segment  := Preamble Record*
Preamble := varint(len) version:u8 created_ms:varint anon_id:string user_id:string session_id:bytes16
Record   := varint(len) RecordBody          ; LA1 kayıt çerçevesinin aynısı
```

İstek gövdesi = `Magic + Header + (preamble'dan sonraki dosya baytları)`. Header gönderim anında
üretilir (`sent_at`, güncel bağlam, `dropped_count`); `anon_id`/`user_id`/`session_id` ise
segmentin preamble'ından gelir, böylece bir segmentteki kayıtlar çevrimdışı kalınsa bile
yakalandıkları kimlikle gider. Kimlik ve oturum değişimi her zaman segment sınırıdır.

**`batch_id`.** Yalnızca `pending` dosya adından türetilir, bu yüzden aynı dosyanın her
denemesi (süreç yeniden başlasa da) aynı kimliği taşır: bölünmemiş dosyada addaki UUIDv7'nin
kendisi; 413 sonrası bölünen parçalarda `uuid8(murmur3 tabanlı 128 bit özet("<uuid hex>-<bölme yolu>"))`.

**Tetikleyiciler.** `flushAt`, ön planda `flushIntervalMs`, arka plana geçiş, açılışta bekleyen
dosya, `flush()`. `active` → `pending` çevirimi, bekleyen dosya yokken yapılır (çevrimdışıyken
çok sayıda küçük batch oluşmasın diye); 500 kayıt / ~900 KB / `maxQueueBytes/8` sınırı ve kimlik
değişimi her durumda çevirir. Aynı anda tek istek, en eski dosya önce.

**Yanıt kodları (§3.5).** 200 sil · 400 at · 401/403 at + 1 saat dur (bellekte, monoton saat) ·
413 kayıt sınırında ikiye böl (tek kayıtsa at) · 429 `Retry-After` · 5xx/ağ/bilinmeyen kod:
1 sn → 5 dk üstel backoff, gecikme `[taban/2, taban]` aralığında rastgele.

**Sınırlar.** `maxQueueBytes` aşılırsa en eski `pending` silinir; `maxEventAgeMs`'den eski
`pending` silinir (cihaz duvar saatiyle; saat geri alınmışsa dosya korunur). Atılan her kayıt
sayılır ve sonraki başarılı batch'in bağlamına `dropped_count` olarak yazılır. SDK tarafında:
128 bayttan uzun event adı ve 64 KB'ı aşan kayıt atılır; 128 bayttan uzun anahtar ya da 8192
bayttan uzun string/json değer taşıyan özellik çıkarılır; en fazla 200 özellik tutulur.

**Oturum.** 30 dk hareketsizlikte yeni `session_id` (UUIDv7); arka plandan dönüşte denetlenir;
süreç 30 dk içinde yeniden başlarsa oturum sürer. `reset()` yeni oturum başlatır.

**Replay.** En çok 2 kare/sn, değişmeyen ağaçta kare yok, her parça `full_snapshot` ile başlar,
60 sn'de bir tam kare, parça 5 sn / 64 KB'ta kapanır, gzip'lenip diskte `replay/` altında
bekletilir ve `/v1/r`'ye yüklenir. Kardeş sırası değişimi ya da araya ekleme delta ile ifade
edilemediği için tam kareye düşülür.

## Kişisel veri sayılan alanlar

- `anon_id`: SDK'nın ürettiği, kuruluma özgü rastgele kimlik (çevrimiçi tanımlayıcı).
- `user_id` ve `identify`/`setPersonProps` ile verilen **tüm kişi özellikleri** (uygulama ne verirse).
- `capture`/`screen`/`register` ile verilen özellikler: içerik uygulamanın sorumluluğundadır;
  e-posta, telefon, ad gibi alanlar kişisel veridir.
- Otomatik bağlam: `device_model`, `os_version`, `locale`, `timezone`, `screen_w/h`, `network`,
  `app_version` — tek başına kimlik değildir ama birleşince parmak izi niteliği taşıyabilir.
- `session_id` ve kayıt `uuid`/`ts` değerleri (davranış verisi).
- Replay: ekrandaki metinler. Giriş alanları ve parola alanları varsayılan olarak, diğer metinler
  `maskAllText` ya da `analyticsMask()` ile cihazdan çıkmadan `•` ile değiştirilir; görseller
  hiçbir içerik taşımaz. Maskelenmeyen metinler kişisel veri içerebilir.
- SDK IDFA/GAID okumaz; IP adresi SDK tarafından gönderilen bir alan değildir.
- `optOut()` kuyruğu (ve replay bekleme dizinini) siler, bayrak kalıcıdır.

## Bilinen eksikler

- Android/iOS platform katmanları cihaz üzerinde enstrümantasyon testinden geçmedi (design §17.8, §19 #8).
- iOS'ta `network` bağlam alanı yok; iOS'ta dokunma olayları otomatik yakalanmaz (`Replay.touch`).
- 401/403 sonrası 1 saatlik duraklama süreç yeniden başlayınca sıfırlanır.
- Android'de arka plan gönderimi için ek süre istenmez (WorkManager bağımlılığı alınmadı).
- Compose semantics ağacı renk ve semantics'i olmayan öğeleri (düz `Box`, dekoratif görsel) vermez (design §19 #7).
- Compose Multiplatform'da iOS/masaüstü için `SemanticsOwner`'a ulaşmanın herkese açık bir API'si yok; `SemanticsOwnerTreeSource` sahibi dışarıdan ister. Android'de `RootForTest` üzerinden alınır.
- Yayınlama (`maven-publish`) yapılandırması ve iOS framework/XCFramework paketlemesi yoktur.
