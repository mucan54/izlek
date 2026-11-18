# Hafif Analitik Platformu — Teknik Tasarım

Sürüm: 0.1 (taslak) · Tarih: 2026-11-18 · Kapsam: sıfırdan production'a kadar

Bu belge; istemci SDK'ları, ingestion servisi, depolama, sorgu motoru, feature flag, session replay ve operasyon dahil tüm sistemi tanımlar. Sayısal değerlerin tamamı **hedef** ya da **başlangıç ayarıdır**; ölçülmüş sonuç değildir. Ölçülmesi gereken varsayımlar Bölüm 19'da listelenmiştir.

---

## 1. Amaç ve ilkeler

### 1.1 Amaç
Ürün analitiği için uçtan uca, kendi kendine yeten, hafif ve yatayda ölçeklenen bir platform:

- Event toplama (mobil, web, sunucu)
- Kimlik birleştirme (anonim → tanımlı kullanıcı)
- Trend, funnel, retention sorguları
- Feature flag (yerel değerlendirme)
- Session replay (wireframe tabanlı)
- PostHog SDK'larıyla wire düzeyinde uyum (kenar adaptörü olarak)

### 1.2 Kapsam dışı (v1)
- Reklam kaynağı takibi (attribution), çökme raporlama, push kampanyaları
- Group analytics (şirket/hesap düzeyi)
- Sunucu taraflı flag değerlendirme
- Saniye altı gerçek zamanlı dashboard (tazelik hedefi ≤ 90 sn)

### 1.3 Tasarım ilkeleri
1. **Sıcak yolda en az iş.** Ingestion yalnızca doğrular ve diske ekler; çözümleme arka plandadır.
2. **Dosya formatı veritabanıdır.** Kalıcı veri object storage'da açık formatta (Parquet) durur; sorgu motoru değiştirilebilir bir okuyucudur.
3. **Veritabanı sunucusu yok.** Durum; yerel WAL, object storage ve küçük bir kontrol veritabanından ibarettir.
4. **Her bileşen bağımsız ölçeklenir ve bağımsız çöker.** Bileşenler arası sözleşme dosya formatlarıdır.
5. **İstemci kuyruğu kalıcıdır, sunucu idempotenttir.** Tekrar gönderim her zaman güvenlidir.
6. **Sessiz yanlışlığa karşı diferansiyel test.** Özel operatörler, aynı veri üzerinde düz SQL sonucuyla karşılaştırılır.

---

## 2. Mimari genel bakış

```
 ┌────────────┐   ┌────────────┐   ┌──────────────┐
 │ KMP SDK    │   │ Web SDK    │   │ PostHog SDK  │
 │ (mobil)    │   │ (TS)       │   │ (uyum modu)  │
 └─────┬──────┘   └─────┬──────┘   └──────┬───────┘
       │ /v1/e (ikili)  │ /v1/e           │ /batch, /i/v0/e (JSON)
       └────────────────┴────────┬────────┘
                                 ▼
                    ┌─────────────────────────┐
                    │ INGESTION (Go)          │  durumsuz*, N adet
                    │ doğrula → WAL → fsync   │
                    └───────────┬─────────────┘
                                │ kapanan segment (zstd)
                                ▼
        ┌──────────────────────────────────────────────┐
        │ OBJECT STORAGE (S3 uyumlu)                   │
        │ wal/   data/   persons/   identity/          │
        │ replay/   flags/   control/                  │
        └───────┬──────────────────────────┬───────────┘
                │                          │
                ▼                          ▼
     ┌──────────────────────┐   ┌──────────────────────────┐
     │ SHIPPER (Rust)       │   │ QUERY (Rust, DataFusion) │
     │ WAL → sıralı Parquet │   │ manifest → prune → scan  │
     │ compaction, GC       │   │ funnel/retention operatör│
     └──────────────────────┘   └────────────┬─────────────┘
                                             │
                                ┌────────────▼────────────┐
                                │ CONTROL PLANE + UI      │
                                │ proje, token, flag,     │
                                │ kayıtlı sorgular        │
                                └─────────────────────────┘
```
\* Ingestion node'u yalnızca son ~10 sn'lik veriyi yerel diskte tutar.

### 2.1 Bileşenler

| Bileşen | Dil | Durum | Ölçekleme |
|---|---|---|---|
| Ingestion | Go (stdlib `net/http`) | Yerel WAL (kısa ömürlü) | Yük dengeleyici arkasında yatay |
| Shipper | Rust (`arrow`, `parquet`, `object_store`) | Yok (object storage) | Proje hash'ine göre shard |
| Compactor / GC | Rust (shipper ile aynı binary, ayrı mod) | Yok | Shard başına bir işçi |
| Query | Rust (`datafusion`, `axum`) | NVMe dosya cache'i (atılabilir) | Yatay, durumsuz |
| Control plane | Go | SQLite (ya da Postgres) | Tek node + yedek |
| UI | TypeScript SPA | Yok | Statik dosya |
| KMP SDK | Kotlin Multiplatform | Cihazda dosya kuyruğu | — |
| Web SDK | TypeScript | `localStorage`/bellek | — |

Bağımlılık sürümleri uygulama başlangıcında sabitlenir ve `Cargo.lock` / `go.sum` ile kilitlenir.

### 2.2 Veri akışı (event)
1. SDK event'i cihazdaki kuyruk dosyasına ekler.
2. Tetikleyici geldiğinde batch'i `/v1/e` ucuna POST eder.
3. Ingestion token'ı doğrular, çerçeveyi kontrol eder, WAL'a ekler, grup fsync sonrası `200` döner.
4. Segment kapanınca (64 MB ya da 10 sn) zstd ile sıkıştırılıp `wal/` altına yüklenir.
5. Shipper segmentleri okur, `(proje, gün)` gruplarına ayırır, `(person_key, ts, uuid)` sıralar, Parquet yazar, manifest'i günceller.
6. Query servisi manifest'i okur, ilgili dosyaları tarar.

Uçtan uca tazelik hedefi: p95 ≤ 90 sn.

---

## 3. Wire protokolü (native, v1)

### 3.1 Uçlar

| Yöntem | Yol | Amaç |
|---|---|---|
| POST | `/v1/e` | Event batch'i (ikili) |
| POST | `/v1/r` | Session replay parçası |
| GET | `/healthz`, `/readyz` | Canlılık / hazır olma |
| GET | `/metrics` | Prometheus (iç ağ) |
| POST | `/batch`, `/i/v0/e`, `/e`, `/capture` | PostHog uyum adaptörü (JSON) |
| POST/GET | `/flags`, `/decide` | PostHog uyum: boş flag yanıtı |

Feature flag kural setleri ingestion'dan değil CDN/object storage'dan sunulur (Bölüm 11).

### 3.2 Kimlik doğrulama
- Yazma token'ı herkese açıktır (`pk_` önekli), uygulama içine gömülür.
- Gönderim: `Authorization: Bearer pk_…`. `sendBeacon` başlık koyamadığı için `?t=pk_…` sorgu parametresi de kabul edilir.
- Okuma/sorgu için ayrı gizli anahtar (`sk_`), yalnızca sunucu tarafında.

### 3.3 Batch çerçevesi
HTTP gövdesi `Content-Encoding: gzip` ile sıkıştırılabilir (zorunlu değil). Açılmış gövde:

```
Batch   := Magic Header Record*
Magic   := "LA1" 0x00                       ; 4 bayt
Header  := varint(len) HeaderBody
Record  := varint(len) RecordBody
```

**HeaderBody** (alan sırası sabit):

| Alan | Tip | Açıklama |
|---|---|---|
| `batch_id` | 16 bayt | UUIDv7; idempotency anahtarı |
| `sent_at` | varint (unix ms) | Cihaz saatine göre gönderim anı |
| `sdk` | string | Örn. `kmp/0.1.0` |
| `anon_id` | string | Cihazın anonim kimliği |
| `user_id` | string (boş olabilir) | Identify sonrası kullanıcı kimliği |
| `session_id` | 16 bayt (sıfır = yok) | Oturum kimliği |
| `context` | PropList | Batch'teki tüm kayıtlar için ortak özellikler |

**RecordBody**:

| Alan | Tip | Açıklama |
|---|---|---|
| `kind` | u8 | 0 event · 1 identify · 2 set_person · 3 reset |
| `uuid` | 16 bayt | UUIDv7, istemci üretir |
| `ts` | varint (unix ms) | Cihaz saatine göre olay anı |
| `name` | string | Event adı (`kind=0` için) |
| `props` | PropList | Kayda özel özellikler |

**PropList** := `varint(count)` ardından `count` adet `(key: string, tag: u8, value)`:

| Tag | Tip | Kodlama |
|---|---|---|
| 0 | null | — |
| 1 | false | — |
| 2 | true | — |
| 3 | int64 | zigzag varint |
| 4 | float64 | 8 bayt little-endian |
| 5 | string | varint(len) + UTF-8 |
| 6 | json | varint(len) + UTF-8 JSON (dizi/nesne için) |

`string` := `varint(len)` + UTF-8 baytlar.

**Kayıt türleri**
- `identify` (1): başlıktaki `anon_id` → `props["user_id"]` bağını kurar.
- `set_person` (2): `props` kişi özellikleridir; son yazan kazanır.
- `reset` (3): çıkış; SDK yeni `anon_id` üretir. Sunucuda bilgi amaçlıdır.

### 3.4 Sınırlar (başlangıç)

| Sınır | Değer |
|---|---|
| Açılmış batch boyutu | 1 MB |
| Batch başına kayıt | 500 |
| Tek kayıt | 64 KB |
| Kayıt başına özellik | 200 |
| Anahtar uzunluğu | 128 bayt |
| String değer | 8 KB |
| Event adı | 128 bayt |

### 3.5 Yanıtlar ve retry sözleşmesi
SDK yanıt gövdesini okumaz; yalnızca durum koduna bakar.

| Kod | Anlam | SDK davranışı |
|---|---|---|
| 200 | Kalıcı olarak yazıldı | Batch dosyasını sil |
| 400 | Bozuk çerçeve | Batch'i **at**, sayaç artır |
| 401 / 403 | Geçersiz ya da kapalı token | Batch'i at; gönderimi 1 saat durdur |
| 413 | Çok büyük | Batch'i ikiye böl, tekrar dene |
| 429 | Hız sınırı | `Retry-After` kadar bekle, tekrar dene |
| 5xx / ağ hatası | Geçici | Üstel backoff (1 sn → 5 dk, jitter), tekrar dene |

Sunucu `200` dönmeden önce veriyi fsync etmiş olmalıdır. `200` sonrası kayıp, hata sayılır.

### 3.6 Saat düzeltmesi
Sunucu alış zamanı `recv_ts`, cihaz `sent_at` gönderir. `skew = recv_ts − sent_at`.
- `|skew| > 5 sn` ise her kaydın `ts` değeri `ts + skew` olarak düzeltilir, `clock_corrected = true` işaretlenir.
- Düzeltilmiş `ts` gelecekte ise `recv_ts`'ye sabitlenir.
- `ts`, `recv_ts`'den 30 günden eski ise olduğu gibi bırakılır ama `late = true` işaretlenir.

### 3.7 PostHog uyum adaptörü
- JSON gövde hiç dönüştürülmeden WAL'a `format = 2` zarfıyla yazılır; dönüşüm shipper'da yapılır (sıcak yol yine parse etmez, yalnızca JSON geçerliliğini ve `api_key` alanını kontrol eder).
- Eşleme (shipper):

| PostHog alanı | Karşılığı |
|---|---|
| `api_key` / `token` | proje |
| `event` | `name` |
| `distinct_id` ya da `properties.distinct_id` | `user_id` ya da `anon_id` |
| `uuid` | `uuid` (yoksa içerikten deterministik türetilir) |
| `timestamp` + `sent_at` | `ts` + saat düzeltmesi |
| `$identify` + `$anon_distinct_id` | `identify` kaydı |
| `$create_alias` | `identify` kaydı |
| `properties.$set` | `set_person` kaydı |
| `properties.*` | `props` (anahtar adları korunur) |

- `/flags` ve `/decide`: SDK'nın beklediği şemada boş flag kümesi. Yanıt gövdeleri ve durum kodları, resmi SDK'larla çalışan uyum testleriyle doğrulanır (Bölüm 17.2).
- Kapsam dışı: PostHog session replay ucu ve yeni v1 capture uçları. İhtiyaç olursa adaptöre eklenir.

---

## 4. KMP SDK

### 4.1 Modüller

| Artefakt | İçerik | Bağımlılık |
|---|---|---|
| `analytics-core` | Event, kimlik, kuyruk, gönderim | `kotlinx-coroutines-core` |
| `analytics-flags` | Kural seti indirme, yerel değerlendirme | `core` |
| `analytics-replay` | Wireframe yakalama | `core` (+ Compose, opsiyonel) |

Hedefler: `androidTarget`, `iosArm64`, `iosSimulatorArm64`, `jvm`. Web için ayrı TypeScript SDK (Bölüm 5).

### 4.2 Public API

```kotlin
object Analytics {
    fun setup(config: Config, context: PlatformContext)

    fun capture(event: String, props: Map<String, Any?> = emptyMap())
    fun screen(name: String, props: Map<String, Any?> = emptyMap())
    fun identify(userId: String, personProps: Map<String, Any?> = emptyMap())
    fun setPersonProps(props: Map<String, Any?>)
    fun register(superProps: Map<String, Any?>)   // her event'e eklenir
    fun unregister(key: String)
    fun reset()                                   // çıkış: yeni anon_id

    fun optOut()
    fun optIn()
    suspend fun flush()
}

data class Config(
    val token: String,
    val host: String,
    val flushAt: Int = 20,
    val flushIntervalMs: Long = 30_000,
    val maxQueueBytes: Long = 10L * 1024 * 1024,
    val maxEventAgeMs: Long = 7L * 24 * 3600 * 1000,
    val captureLifecycle: Boolean = true,
    val gzip: Boolean = true,
)
```

`capture` hiçbir zaman bloklamaz ve hata fırlatmaz; kayıt tek iş parçacıklı bir dispatcher'a devredilir.

### 4.3 Kuyruk: dosya = istek gövdesi

Dizin: `<uygulama verisi>/analytics/<token-hash>/`

| Dosya | İçerik |
|---|---|
| `state` | `anon_id`, `user_id`, super props, opt-out bayrağı |
| `active` | Açık segment: uzunluk önekli `Record` dizisi |
| `pending-<seq>` | Kapanmış, gönderilmeyi bekleyen segment |

Akış:
1. `capture` → `Record` kodlanır → `active` dosyasına eklenir.
2. Tetikleyici gelince `active` → `pending-<seq>` olarak yeniden adlandırılır (atomik).
3. Gönderimde gövde = `Magic + Header + pending dosyasının baytları`. Header gönderim anında üretilir (`sent_at`, güncel `user_id`, context).
4. `200` → dosya silinir. Diğer kodlar Bölüm 3.5'e göre.

Bozulmaya karşı:
- Açılışta `active` ve `pending-*` dosyaları taranır; son kayıt yarım yazılmışsa dosya son tam kaydın sonuna kırpılır.
- `batch_id`, `pending` dosyasının adından deterministik türetilir; aynı dosyanın tekrar gönderimi aynı `batch_id`'yi taşır.

Sınırlar:
- Toplam boyut `maxQueueBytes`'ı aşarsa en eski `pending` silinir.
- `maxEventAgeMs`'den eski `pending` dosyaları silinir.
- Atılan kayıt sayısı bir sonraki batch'in context'ine `dropped_count` olarak eklenir.

### 4.4 Tetikleyiciler
- `active` içindeki kayıt sayısı `flushAt`'e ulaştı
- Ön plandayken `flushIntervalMs` zamanlayıcısı
- Uygulama arka plana geçti (iOS'ta `beginBackgroundTask` ile kısa süre istenir)
- Açılışta bekleyen `pending` varsa
- Elle `flush()`

Aynı anda tek gönderim yapılır; `pending` dosyaları sırayla gider.

### 4.5 Platforma özel kısım (`expect/actual`)

| Arayüz | Android / JVM | iOS |
|---|---|---|
| `httpPost(url, headers, body): Int` | `HttpURLConnection` | `NSURLSession` |
| `storageDir(): Path` | `context.filesDir` / kullanıcı dizini | `NSApplicationSupportDirectory` |
| `gzip(bytes): ByteArray` | `java.util.zip` | `platform.zlib` |
| `lifecycle(onBackground, onForeground)` | `ProcessLifecycleOwner` ya da `ActivityLifecycleCallbacks` | `UIApplication` bildirimleri |
| `deviceContext(): Map` | `Build`, `PackageManager` | `UIDevice`, `NSBundle` |
| `randomBytes(n)` | `SecureRandom` | `SecRandomCopyBytes` |

Dosya G/Ç için küçük bir `expect/actual` katmanı yazılır; `okio` bağımlılığı alınmaz.

### 4.6 Kimlik ve oturum
- `anon_id`: ilk açılışta UUIDv7, `state` dosyasında kalıcı.
- `identify(userId)`: `user_id` kaydedilir, bir `identify` kaydı yazılır. Sonraki batch başlıkları her iki kimliği taşır.
- `reset()`: `reset` kaydı yazılır, `active` hemen `pending`'e çevrilir (başlık eski kimlikle gitsin diye), ardından yeni `anon_id` üretilir ve `user_id` temizlenir.
- Oturum: 30 dk hareketsizlikten sonra yeni `session_id`. Arka plandan dönüşte süre kontrol edilir.
- Kimlik değişimi (`identify`, `reset`) her zaman segment sınırıdır: bir `pending` dosyasındaki tüm kayıtlar aynı kimliğe aittir.

### 4.7 Otomatik context (batch başlığında, bir kez)
`platform`, `os`, `os_version`, `device_model`, `device_type`, `app_version`, `app_build`, `locale`, `timezone`, `screen_w`, `screen_h`, `network` (wifi/cellular), `sdk`.

### 4.8 Otomatik event'ler (`captureLifecycle = true`)
`app_installed`, `app_updated`, `app_opened`, `app_backgrounded`. Ekran görüntülemeleri `screen()` ile elle; otomatik gezinme takibi v1'de yok.

### 4.9 Gizlilik
- `optOut()`: kuyruk silinir, `capture` çağrıları yok sayılır, bayrak kalıcıdır.
- SDK IDFA/GAID toplamaz.
- iOS privacy manifest dosyası SDK ile birlikte gelir.

### 4.10 Boyut ve performans hedefleri
- `core` Android'e eklediği boyut: < 60 KB (R8 sonrası). Hedeftir.
- `capture` çağrısının çağıran iş parçacığındaki maliyeti: < 50 µs.
- Bellek: kuyruk bellekte tutulmaz; yalnızca kodlama tamponu.

---

## 5. Web SDK (TypeScript)

- Elle yazılmış, bağımlılıksız; hedef < 4 KB gzip (replay hariç).
- Aynı ikili wire formatı (`Uint8Array` + `DataView`), aynı test vektörleri.
- Kuyruk: bellekte; sayfa kapanışında `fetch(..., { keepalive: true })`, olmazsa `sendBeacon` (`?t=` ile). Gönderilemeyen batch `localStorage`'a yazılır (üst sınır 200 KB) ve sonraki sayfa açılışında gönderilir.
- `anon_id`: birinci taraf çerez + `localStorage`. Çerez alan adı yapılandırılabilir (alt alan adları arası paylaşım).
- Otomatik: `pageview` (History API dinlenerek SPA dahil), `pageleave`. Tıklama autocapture v1'de yok; `data-track` özniteliğiyle açık işaretleme desteklenir.
- Context: `url`, `path`, `referrer`, `utm_*`, `locale`, `viewport_w/h`, `ua` (ham), `sdk`.
- Bot filtresi: bilinen bot UA'ları istemcide atılır; sunucuda ikinci kontrol (shipper).

---

## 6. Ingestion servisi (Go)

### 6.1 Sıcak yol

```
1. Token'ı başlıktan/sorgudan al → bellek içi map'te ara (proje id, durum, limitler)
2. Hız sınırı kontrolü (token ve IP)
3. Gövdeyi sınırlı okuyucuyla oku (sıkıştırılmış ≤ 1 MB, açılmış ≤ 1 MB)
4. gzip ise aç
5. Çerçeve doğrula: magic, başlık uzunluğu, kayıt uzunlukları toplamı = gövde uzunluğu,
   kayıt sayısı ≤ sınır. (Kayıt içerikleri çözülmez.)
6. batch_id'yi başlıktan oku → yakın geçmiş LRU'sunda varsa: 200 dön, yazma
7. Zarf + gövdeyi WAL yazarına gönder, fsync onayını bekle
8. 200 dön
```

Adım 5 yalnızca uzunluk alanlarını gezer; O(kayıt sayısı), bellek ayırmaz.

### 6.2 WAL segment formatı

Dosya: `<wal_dir>/<node_id>-<seq>.wal`

```
Segment := SegMagic("LAW1") Entry*
Entry   := u32le(len) u32le(crc32c) Envelope Body
Envelope:= u8  format        ; 1 native · 2 posthog-json · 3 replay-index
           u32 project_id
           u64 recv_ts_ms
           u8[2] country     ; IP'den, bilinmiyorsa "ZZ"
           u8  flags         ; bit0: web kaynağı
```

- CRC, `Envelope + Body` üzerindedir.
- IP adresi **yazılmaz**; yalnızca ülke kodu türetilir (gömülü ülke veritabanı, bellek içi arama).
- User-Agent web kaynağında context içinde zaten gelir; sunucu ayrıca yazmaz.

### 6.3 Grup fsync
- Tek yazar goroutine. İstekler kanal üzerinden girer.
- Yazar; 2 ms geçince ya da 1 MB birikince `write` + `fsync` yapar, bekleyen tüm isteklere onay verir.
- p99 onay gecikmesi hedefi: < 50 ms (disk dahil).

### 6.4 Segment döngüsü
- Rotasyon: 64 MB ya da 10 sn (hangisi önce).
- Kapanan segment: zstd (seviye 3) → `wal/dt=YYYY-MM-DD/hh=HH/<node_id>-<seq>.wal.zst` anahtarına yüklenir.
- Yükleme başarılı olunca yerel dosya silinir. Başarısızsa üstel backoff ile sonsuz tekrar.
- Açılışta yerel dizindeki tüm segmentler: son girdi CRC'si bozuksa kırpılır, sonra yüklenir.

### 6.5 Geri basınç
- Yüklenmemiş yerel WAL boyutu `wal_max_local` (varsayılan: diskin %70'i) sınırını aşarsa `/readyz` başarısız olur ve yeni istekler `503 + Retry-After` alır.
- Object storage kesintisinde node, disk dolana kadar kabul etmeye devam eder.

### 6.6 Dayanıklılık penceresi (bilinen ödünleşim)
Onaylanmış ama henüz yüklenmemiş veri (en fazla ~10 sn) yalnızca node'un yerel diskindedir. Disk kalıcı olarak kaybolursa bu pencere kaybolur. Azaltma seçenekleri (yapılandırma ile):
- `wal_dir`'i ağ bağlı kalıcı diske koymak (node ölse de disk kalır),
- rotasyon süresini kısaltmak (daha çok küçük nesne),
- eşzamanlı ikinci node'a çoğaltma (v2).

### 6.7 Token kaydı
- Control plane `control/tokens.json` dosyasını object storage'a yazar: `{ token → project_id, status, rate_limit, allowed_origins }`.
- Ingestion 30 sn'de bir `If-None-Match` ile yoklar; son iyi kopyayı diskte tutar (control plane çökse de ingestion çalışır).
- Bilinmeyen token: `401`. Kapalı proje: `403`.

### 6.8 Hız sınırı ve kötüye kullanım
- Token başına token-bucket (varsayılan 2.000 istek/sn, proje bazında ayarlanır).
- IP başına token-bucket (varsayılan 50 istek/sn).
- Sınırlar node yereldir (paylaşılan durum yok); etkin sınır ≈ node sayısı × değer. Bilinçli tercih.
- Web token'ları için `Origin` izin listesi (boşsa kontrol yok).
- CORS: `Access-Control-Allow-Origin` izin listesine göre; `OPTIONS` ön uçuşu 24 saat önbelleklenir.

### 6.9 Yapılandırma (ortam değişkenleri)

```
LA_LISTEN=:8080
LA_NODE_ID=ing-01
LA_WAL_DIR=/var/lib/la/wal
LA_WAL_SEGMENT_BYTES=67108864
LA_WAL_SEGMENT_SECONDS=10
LA_WAL_FSYNC_MS=2
LA_S3_ENDPOINT=...
LA_S3_BUCKET=...
LA_S3_ACCESS_KEY=...   LA_S3_SECRET_KEY=...
LA_TOKENS_KEY=control/tokens.json
LA_COMPAT_POSTHOG=true
LA_GEO_DB=/etc/la/country.mmdb
```

### 6.10 Kapanış
`SIGTERM` → `/readyz` başarısız → 5 sn bekle (yük dengeleyici çıkarır) → açık istekleri bitir → segmenti kapat → yükle → çık.

---

## 7. Object storage düzeni

```
wal/dt=2026-11-18/hh=21/ing-01-000123.wal.zst
wal/_done/ing-01-000123.ok

data/p=<project_id>/d=2026-11-18/L0-<segrange>-<hash>.parquet
data/p=<project_id>/d=2026-11-18/L2-<version>.parquet
data/p=<project_id>/d=2026-11-18/_m/00000042.json        ; gün manifest'i

identity/p=<project_id>/map-<version>.parquet             ; anon_id → user_id
identity/p=<project_id>/_m/00000007.json

persons/p=<project_id>/props-<version>.parquet            ; kişi özellikleri
persons/p=<project_id>/_m/00000003.json

tombstones/p=<project_id>/<ts>.json                       ; silme istekleri

replay/p=<project_id>/d=2026-11-18/<session_id>/<seq>.bin.zst

flags/<token>.json                                        ; CDN'den sunulur
control/tokens.json
control/schema/p=<project_id>.json                        ; terfi edilmiş sütunlar
```

Gereksinim: object storage **koşullu yazma** (`If-None-Match: *`) desteklemelidir; manifest sürümlerinin doğrusal olması buna dayanır. Seçilen sağlayıcıda Faz 0'da doğrulanır.

Yaşam döngüsü kuralları:
- `wal/`: 30 gün sonra silinir (yeniden işleme penceresi).
- `replay/`: proje ayarına göre (varsayılan 30 gün).
- `data/`: proje saklama süresine göre gün klasörü bazında.

---

## 8. Shipper (Rust)

### 8.1 Görev
`wal/` altındaki segmentleri okuyup proje ve gün bazında sıralı Parquet dosyalarına çevirmek; kimlik ve kişi özelliklerini güncellemek.

### 8.2 Döngü
1. `wal/` altını listele; `_done/` işareti olmayan segmentleri sırala.
2. 30 sn ya da 256 MB (açılmış) dolana kadar segment topla.
3. Her girdiyi çöz:
   - `format=1`: ikili batch → kayıtlar. Başlık context'i her kayda yayılır.
   - `format=2`: PostHog JSON → Bölüm 3.7 eşlemesi.
   - `format=3`: replay indeks kaydı → `session_replay_chunk` event'i.
4. Saat düzeltmesini uygula (Bölüm 3.6).
5. `identify` kayıtlarından kimlik eşlemelerini, `set_person` kayıtlarından kişi özelliklerini ayır.
6. Her event için `person_key` hesapla (Bölüm 9).
7. `(project_id, gün(ts))` grupla; grup içinde `(person_key, ts, uuid)` sırala; batch içi `uuid` tekrarlarını at.
8. Her grup için bir L0 Parquet yaz. Dosya adı girdi segment aralığından deterministiktir → yeniden işleme aynı adı üretir.
9. Her etkilenen gün için yeni manifest sürümünü koşullu yazma ile ekle.
10. Kimlik ve kişi dosyalarını güncelle (kendi manifest'leriyle).
11. Segmentler için `_done/` işaretini yaz.

Çökme güvenliği: 8–11 arası herhangi bir yerde çökme, aynı segmentlerin yeniden işlenmesine yol açar. Dosya adları deterministik ve manifest ekleme idempotent olduğundan (aynı dosya adı zaten listedeyse atlanır) sonuç aynıdır.

### 8.3 Parquet şeması (`events`)

| Sütun | Tip | Not |
|---|---|---|
| `uuid` | FIXED_LEN_BYTE_ARRAY(16) | |
| `ts` | TIMESTAMP(ms, UTC) | Düzeltilmiş |
| `recv_ts` | TIMESTAMP(ms, UTC) | |
| `event` | STRING | Sözlük kodlamalı |
| `person_key` | STRING | Sıralama anahtarı |
| `anon_id` | STRING | |
| `user_id` | STRING, nullable | |
| `session_id` | FIXED_LEN_BYTE_ARRAY(16), nullable | |
| `platform`, `os`, `os_version`, `device_model`, `device_type`, `app_version`, `locale`, `country`, `sdk` | STRING | Sözlük |
| `url_path`, `referrer_host`, `screen` | STRING, nullable | |
| `flags` | INT32 | bit0 `clock_corrected`, bit1 `late`, bit2 `compat` |
| `p_<anahtar>` | değişken, nullable | Terfi edilmiş özellikler |
| `props` | STRING | Kalan özellikler, JSON |

Yazım ayarları: zstd sıkıştırma, satır grubu ~100.000 satır, sütun istatistikleri ve sayfa indeksi açık, `event` ve `person_key` için sözlük.

### 8.4 Özellik terfisi
- Shipper, proje başına anahtar frekansı ve tip istatistiği tutar (`control/schema/p=<id>.json`).
- Bir anahtar son 7 günde event'lerin ≥ %5'inde ve tutarlı tipte görülürse `p_<anahtar>` sütununa terfi eder (proje başına en fazla 200 sütun).
- Terfi ileriye dönüktür; eski dosyalar compaction sırasında yeni şemaya geçer.
- Sorgu katmanı `props.x` erişimini şeffaf çözer: sütun varsa sütundan, yoksa `props` JSON'undan.
- Tip çakışmasında değer `props` içinde kalır, sütun `NULL` olur.

### 8.5 Shard'lama
- `hash(project_id) mod N` → shard. Her shard'da tek etkin shipper.
- Tek etkinlik: object storage'da kira (lease) nesnesi, koşullu yazma ile, 30 sn yenileme.
- Tüm shipper'lar tüm WAL segmentlerini okur ama yalnızca kendi shard'ının projelerini işler; `_done` işareti shard başınadır (`wal/_done/<shard>/…`).

### 8.6 Bot ve geçersiz kayıt filtresi
- Bilinen bot UA'ları (web) → atılır, sayaç artar.
- Event adı ya da kimliği boş kayıtlar → atılır.
- Atılanlar proje başına `ingest_rejected` metriğinde raporlanır.

---

## 9. Kimlik çözümleme

### 9.1 Model
- Her event `anon_id` taşır; identify sonrası `user_id` de taşır.
- `person_key` := `user_id` biliniyorsa `"u:" + user_id`, değilse `"a:" + anon_id`.
- Kimlik eşleme tablosu: `anon_id → user_id` (proje başına). Bir `anon_id` yalnızca bir `user_id`'ye bağlanır; ilk bağ kalıcıdır. Farklı bir `user_id` ile ikinci `identify` gelirse yok sayılır ve metrik artar (paylaşılan cihaz durumu; SDK `reset()` çağrısıyla yeni `anon_id` üretmelidir).

### 9.2 Üç aşamalı çözümleme
1. **Yazım anında (shipper):** event'te `user_id` varsa ya da `anon_id` eşleme tablosunda varsa `person_key = "u:…"`.
2. **Sorgu anında:** henüz düzeltilmemiş `"a:…"` anahtarları için eşleme tablosu belleğe alınır ve `person_key` yeniden yazılır. Yeniden yazılan satırlar sıralamayı bozduğu için bu satırlar ayrı akışta sıralanıp ana akışla birleştirilir (genelde küçük bir azınlık).
3. **Compaction'da:** gün dosyası yeniden yazılırken güncel eşleme uygulanır ve dosya yeniden sıralanır. Compaction'dan geçmiş günlerde 2. aşama yalnızca compaction sonrası gelen eşlemeler için çalışır.

Manifest, her dosya için `identity_version` taşır; sorgu yalnızca bu sürümden sonraki eşlemeleri uygular.

### 9.3 Kişi özellikleri
- `set_person` ve `identify` ile gelen özellikler `persons/` altında `(person_key, key) → (value, ts)` olarak tutulur; son yazan kazanır.
- Sorguda `person.x` erişimi bu tabloyla birleştirmedir (güncel değer). Event anındaki değer gerekiyorsa özellik event'e de konmalıdır.

---

## 10. Compaction, saklama, silme

### 10.1 Seviyeler

| Seviye | Kaynak | Tetikleyici |
|---|---|---|
| L0 | Shipper çıktısı | — |
| L1 | Aynı günün L0'ları | Günde > 8 L0 ya da toplam > 256 MB |
| L2 | Günün tümü, tek/az dosya | Gün kapandıktan 6 saat sonra; sonrasında geç gelen L0 birikirse |

Compaction adımları: dosyaları k-yollu birleştir (zaten sıralı) → güncel kimlik eşlemesini uygula → gerekiyorsa yeniden sırala → `uuid` tekrarlarını at → tombstone'ları uygula → güncel şemaya terfi et → yeni dosya → manifest'in yeni sürümü.

L2 hedef dosya boyutu 512 MB; büyük günler `person_key` aralığına göre birden çok dosyaya bölünür (aralıklar manifest'te).

### 10.2 Manifest

```json
{
  "version": 42,
  "project": 1017,
  "day": "2026-11-18",
  "files": [
    {
      "key": "data/p=1017/d=2026-11-18/L2-00000042.parquet",
      "level": 2, "rows": 18234112, "bytes": 412334551,
      "min_ts": 1795046400000, "max_ts": 1795132799999,
      "min_person": "a:0190…", "max_person": "u:zz…",
      "identity_version": 7, "schema_version": 12
    }
  ]
}
```

- Yeni sürüm, `…/_m/<version+1>.json` anahtarına koşullu yazma ile eklenir; çakışmada yeniden oku ve tekrar dene.
- Okuyucular en yüksek sürümü kullanır (listeleme + 10 sn bellek önbelleği).

### 10.3 Çöp toplama
- Hiçbir güncel manifest'te geçmeyen ve 24 saatten eski veri dosyaları silinir.
- Son 20 sürüm dışındaki manifest'ler silinir.
- 24 saat bekleme, süren sorguların ve yarım kalmış yazımların güvenliği içindir.

### 10.4 Saklama süresi
Proje başına gün sayısı. Süresi dolan gün klasörü manifest'iyle birlikte silinir.

### 10.5 Kişi silme (KVKK/GDPR)
1. Control plane `tombstones/p=<id>/<ts>.json` yazar: `{ person_keys: […], anon_ids: […] }`.
2. Query servisi tombstone'ları 60 sn içinde yükler ve bu kişileri **anında** sonuçlardan dışlar.
3. Compactor, etkilenen günleri 7 gün içinde yeniden yazar; replay nesneleri ve kişi/kimlik kayıtları silinir.
4. `wal/` kopyaları 30 günlük yaşam döngüsüyle kendiliğinden kaybolur. Kesin silme üst sınırı: 30 gün. Daha kısa gerekiyorsa `wal/` saklama süresi düşürülür.

---

## 11. Sorgu servisi (Rust)

### 11.1 Yapı
- `axum` HTTP + `datafusion` yürütme + `object_store` erişimi.
- Her istek tek bir projeye bağlıdır; proje kimliği `sk_` anahtarından ya da UI oturumundan gelir, sorgu metninden **asla** gelmez.
- Özel `TableProvider`:
  1. Tarih aralığındaki gün manifest'lerini okur.
  2. Dosyaları `min_ts/max_ts` ve (varsa) `person` aralığıyla eler.
  3. Parquet satır grubu istatistikleri ve sayfa indeksiyle ikinci eleme.
  4. Çıktı sıralamasını `(person_key, ts)` olarak bildirir; planlayıcı gereksiz sıralamayı atar.

### 11.2 Dosya önbelleği
- Yerel NVMe'de LRU; anahtar = nesne anahtarı (dosyalar değişmez olduğundan geçersizleşme yok).
- Parquet footer'ları bellekte ayrıca önbelleklenir.
- Soğuk okuma: aralık istekleriyle yalnızca gerekli sütun parçaları çekilir.

### 11.3 API

| Uç | Amaç |
|---|---|
| `POST /q/trends` | Zaman serisi: sayım, tekil kişi, toplam/ortalama; kırılım ve filtre |
| `POST /q/funnel` | Sıralı adımlar, dönüşüm penceresi, kırılım |
| `POST /q/retention` | Kohort × dönem matrisi |
| `POST /q/paths` | Adım geçişleri (v1.1) |
| `POST /q/events` | Ham event listesi (kişi/oturum bazında, sayfalı) |
| `POST /q/sql` | Kısıtlı SQL (yalnızca `SELECT`; `events`, `persons` tabloları) |

Sorgu gövdeleri JSON; filtreler küçük bir ifade ağacıdır (`and/or/not`, `eq/ne/in/gt/lt/contains/is_set`).

### 11.4 Funnel operatörü
Girdi `(person_key, ts)` sıralı akıştır; birden çok dosya sıra koruyan k-yollu birleştirme ile tek akışa iner (L2 sonrası k ≈ gün sayısı).

Kişi başına durum (adım sayısı `S`, pencere `W`):

```
t[0..S)  : her adım için, o adıma ulaşan en geç başlangıcın zamanı (yoksa ∅)
best     : ulaşılan en yüksek adım

her satır (ts, event) için:
    j = S-1 .. 1:                      # büyükten küçüğe: aynı satır iki adım sayılmasın
        eğer satır adım j koşulunu sağlıyor ve t[j-1] ≠ ∅ ve ts - t[j-1] ≤ W:
            t[j] = max(t[j], t[j-1])   # zinciri başlatan zamanı taşı
            best = max(best, j+1)
    eğer satır adım 0 koşulunu sağlıyor:
        t[0] = ts
        best = max(best, 1)

person_key değişince: histogram[best] += 1 ; durumu sıfırla
```

- Bellek: O(S) (kırılım varsa O(S × kırılım değeri)). Kişi sayısından bağımsız.
- Yalnızca gereken sütunlar okunur: `person_key`, `ts`, `event` ve filtrelerde geçen sütunlar.
- Çıktı: adım başına kişi sayısı, adımlar arası medyan süre (t-digest), kırılım değerine göre.
- Varyantlar: katı sıra (araya başka event girmesin), herhangi sıra — aynı durum makinesinin modları.

### 11.5 Retention operatörü
Aynı sıralı akış. Kişi başına: kohort dönemi (ilk "başlangıç" event'i) ve "dönüş" event'inin görüldüğü dönemlerin bit kümesi. Kişi değişince matris güncellenir. Bellek O(dönem sayısı).

### 11.6 Trends
- Sayım ve toplamlar sıradan gruplama; sıralamadan yararlanmaz, yalnızca sütun eleme ve sözlük kodlamadan yararlanır.
- Tekil kişi sayımı: girdi `person_key` sıralı olduğundan gün içinde tam (yaklaşık değil) sayım akış halinde yapılır; günler arası tekil sayım için birleştirilmiş akış kullanılır.
- Optimizasyon (v1.1): compaction sırasında gün başına `event × saat × {platform, country, app_version}` ön toplam dosyası. Yalnızca bu boyutlarla sınırlı sorgular ham veriye dokunmaz.

### 11.7 Sınırlar ve izolasyon
- Sorgu başına bellek havuzu (varsayılan 2 GB), süre sınırı (60 sn), taranan bayt sınırı (proje planına göre).
- Proje başına eşzamanlı sorgu sınırı (varsayılan 4); aşan istekler kuyruğa girer.
- Sonuç önbelleği: anahtar = sorgu hash'i + ilgili manifest sürümleri. Manifest değişmedikçe sonuç geçerlidir.
- `/q/sql`: ayrıştırılmış plan üzerinde izin listesi (yalnızca `SELECT`, izinli tablo ve fonksiyonlar).

### 11.8 Tazelik
Sorgular yalnızca manifest'e girmiş veriyi görür. Canlı kuyruk görünümü (son dakikalar için WAL okuma) v1 kapsamı dışındadır.

---

## 12. Feature flags

### 12.1 Model
Değerlendirme tamamen istemcidedir. Sunucu yalnızca statik kural seti yayınlar.

`flags/<token>.json` (CDN, `ETag`, `Cache-Control: max-age=60`):

```json
{
  "v": 31,
  "flags": [
    {
      "key": "new_checkout",
      "enabled": true,
      "salt": "k3f9",
      "rules": [
        {
          "when": { "and": [
            { "prop": "platform", "op": "eq", "value": "ios" },
            { "prop": "app_version", "op": "semver_gte", "value": "3.2.0" }
          ]},
          "rollout": 2500,
          "variants": [ { "key": "control", "w": 5000 }, { "key": "b", "w": 5000 } ]
        }
      ],
      "default": null
    }
  ]
}
```

### 12.2 Değerlendirme
1. Kurallar sırayla denenir; `when` koşulu SDK'nın bildiği özelliklerle (context, super props, `register` edilenler, `identify` ile verilen kişi özellikleri) değerlendirilir.
2. Eşleşen ilk kuralda: `bucket = murmur3_32(salt + ":" + key + ":" + id) mod 10000`. `bucket < rollout` ise kullanıcı dahildir.
3. Varyant: `murmur3_32(salt + ":v:" + key + ":" + id) mod 10000` ağırlık aralıklarına düşürülür.
4. `id` = `user_id` varsa o, yoksa `anon_id`. (Identify anında değer değişebilir; kararlılık isteyen flag'ler için kural `"stable_on": "anon_id"` seçebilir.)

`murmur3_32` her platformda elle yazılır (~40 satır) ve ortak test vektörleriyle doğrulanır.

Operatörler: `eq, ne, in, not_in, gt, gte, lt, lte, contains, regex, is_set, semver_gte, semver_lt`.

### 12.3 SDK davranışı
- Açılışta diskteki son kural setiyle hemen çalışır; arka planda `If-None-Match` ile yeniler. Ön plandayken 5 dk'da bir yoklar.
- `flag("key")` ilk çağrıldığında oturum başına bir kez `flag_exposed` event'i yazılır (`flag`, `variant`, `ruleset_v`). Deney analizi bu event üzerinden yapılır.
- Kural seti hiç indirilemediyse çağıranın verdiği varsayılan döner.

### 12.4 Sınırlar
- Yalnızca istemcinin bildiği özelliklerle hedefleme. Sunucuda hesaplanan kohortlara göre hedefleme yoktur.
- Kural seti herkese açıktır; gizli bilgi içermemelidir.

---

## 13. Session replay

### 13.1 Yaklaşım
Ekran görüntüsü değil **wireframe**: görünüm ağacından dikdörtgenler, tipler ve (maskelenmiş) metin. Küçük veri, varsayılan olarak gizlilik dostu.

### 13.2 Kayıt formatı

```
Chunk   := Magic("LAR1") session_id(16) seq(varint) start_ts(varint) Frame*
Frame   := varint(len) kind(u8) dt_ms(varint) payload
kind    := 0 full_snapshot · 1 delta · 2 touch · 3 scroll · 4 screen_change · 5 viewport
Node    := id(varint) parent(varint) type(u8) x,y,w,h(varint×4)
           flags(u8) color(u32)? text(string)?
```

- `full_snapshot`: tüm düğümler. Ekran değişiminde ve 60 sn'de bir.
- `delta`: eklenen, kaldırılan, değişen düğümler.
- Örnekleme: en fazla 2 kare/sn; ağaç değişmediyse kare yazılmaz.
- Parça: 5 sn ya da 64 KB. zstd/gzip ile sıkıştırılıp `/v1/r?s=<session>&n=<seq>` ucuna gönderilir.

### 13.3 Yakalama
- **Compose (Multiplatform dahil):** semantics ağacı gezilir; sınırlar, rol ve metin buradan alınır. Bu yolun büyük kısmının ortak kodda yazılabilmesi beklenir; Faz 6 başında prototiple doğrulanır.
- **Android View / UIKit:** görünüm hiyerarşisi gezilir (platforma özel).
- **Web:** `MutationObserver` + ilk DOM anlık görüntüsü; ayrı opsiyonel modül.

### 13.4 Maskeleme (varsayılanlar)
- Tüm metin giriş alanları: içerik `•` ile değiştirilir.
- Tüm görseller: yer tutucu dikdörtgen.
- Diğer metinler: varsayılan açık; `maskAllText = true` ile kapatılır.
- Bileşen bazında `Modifier.analyticsMask()` / `analyticsUnmask()`.

### 13.5 Sunucu tarafı
- Ingestion gövdeyi **akış halinde** doğrudan object storage'a yazar (`replay/p=…/d=…/<session>/<seq>.bin.zst`), sonra WAL'a `format=3` indeks kaydı ekler (oturum, sıra, bayt, süre).
- Shipper bunu `session_replay_chunk` event'ine çevirir; oturum listesi sıradan event sorgusudur.
- Oynatıcı (UI): parçaları sırayla indirir, kareleri canvas'a çizer, event zaman çizgisini üstüne bindirir.
- Örnekleme: proje ayarı (oturumların %X'i), karar SDK'da oturum başında `hash(session_id)` ile.

---

## 14. Control plane ve UI

### 14.1 Veri (SQLite; çok node gerekiyorsa Postgres)
`orgs`, `users`, `memberships`, `projects`, `tokens`, `flags`, `flag_versions`, `insights` (kayıtlı sorgular), `dashboards`, `deletion_requests`, `audit_log`.

### 14.2 Yayınlama
Değişiklikler object storage'a anlık görüntü olarak yazılır; veri düzlemi control plane'e doğrudan bağlanmaz:
- `control/tokens.json` → ingestion
- `flags/<token>.json` → SDK'lar (CDN)
- `tombstones/…` → query, compactor

Control plane kesintisi; event toplamayı, flag değerlendirmeyi ve sorguyu **etkilemez**.

### 14.3 Kimlik doğrulama
- UI: e-posta + parola (argon2id) ya da OIDC; oturum çerezi; TOTP opsiyonel.
- API: proje bazlı `sk_` anahtarları (kapsam: `query`, `admin`).
- Roller: `owner`, `admin`, `member`, `viewer`.
- Tüm yönetim işlemleri `audit_log`'a yazılır.

### 14.4 UI (v1)
Canlı event akışı (son manifest'ten), trend/funnel/retention oluşturucu, dashboard, kişi sayfası (event zaman çizgisi + replay), flag yönetimi, proje ayarları, veri silme istekleri.

---

## 15. Güvenlik ve gizlilik

- **Aktarım:** her yerde TLS 1.2+. HSTS.
- **Depolama:** bucket sunucu tarafı şifreleme; bucket herkese kapalı. `flags/` yalnızca CDN üzerinden okunur.
- **Kiracı izolasyonu:** her nesne anahtarı `p=<project_id>` önekiyle başlar; sorgu sağlayıcısı proje kimliğini istek bağlamından alır, kullanıcı girdisinden almaz. İzolasyon testi CI'da zorunludur (Bölüm 17.6).
- **IP:** saklanmaz; yalnızca ülke kodu.
- **PII:** SDK belgelerinde hangi alanların kişisel veri olduğu belirtilir; proje ayarında anahtar bazlı "yazımda at" listesi (shipper uygular).
- **Girdi güvenliği:** tüm çözücüler (Go çerçeve denetimi, Rust kayıt çözücü, Parquet okuyucu girdileri) fuzz testinden geçer. Rust tarafında `unsafe` yasak (`#![forbid(unsafe_code)]`), bağımlılıklar hariç.
- **Kaynak tükenmesi:** her uçta gövde, süre ve eşzamanlılık sınırı; gzip bombasına karşı açılmış boyut sınırı.
- **Sırlar:** ortam değişkeni ya da sır yöneticisi; depoda sır yok. Object storage anahtarları bileşen bazında en az yetkiyle (ingestion yalnızca `wal/` ve `replay/` yazabilir; query yalnızca okur).
- **Tedarik zinciri:** `cargo audit`, `govulncheck`, bağımlılık kilit dosyaları, imzalı container imajları.

---

## 16. Gözlemlenebilirlik

### 16.1 Metrikler (Prometheus)

**Ingestion:** `requests_total{code,format}`, `request_seconds` (histogram), `bytes_in_total`, `records_in_total`, `fsync_seconds`, `wal_local_bytes`, `wal_upload_lag_seconds`, `segments_uploaded_total`, `rate_limited_total`, `dup_batches_total`, `tokens_age_seconds`.

**Shipper:** `segments_pending`, `ship_lag_seconds` (en eski işlenmemiş segmentin yaşı), `records_out_total`, `records_rejected_total{reason}`, `parquet_bytes_total`, `manifest_conflicts_total`, `identity_links_total`, `lease_held`.

**Compactor:** `l0_files{project}` (üst N), `compaction_seconds`, `bytes_rewritten_total`, `gc_deleted_total`, `tombstones_pending`.

**Query:** `queries_total{type,status}`, `query_seconds`, `bytes_scanned_total`, `cache_hit_ratio`, `queue_depth`, `memory_pool_bytes`.

### 16.2 SLO'lar (hedef)

| SLO | Hedef |
|---|---|
| Ingestion erişilebilirliği (2xx / geçerli istek) | %99,9 / ay |
| Onay gecikmesi p99 | < 50 ms |
| Tazelik p95 (event → sorgulanabilir) | < 90 sn |
| Onaylanmış event kaybı | 0 (kanarya ile ölçülür) |
| Sorgu p95 (30 günlük funnel, orta ölçekli proje) | < 3 sn |

### 16.3 Uçtan uca kanarya
Ayrı bir işçi, özel bir projeye dakikada sıralı numaralı event'ler gönderir ve sorgu API'sinden geri okur. Eksik ya da çift numara → alarm. "Sessiz kayıp yok" iddiasının sürekli kanıtıdır.

### 16.4 Alarmlar
- `wal_upload_lag_seconds > 120` (5 dk)
- `ship_lag_seconds > 300`
- `wal_local_bytes` disk sınırının %50'si
- 5xx oranı > %1 (5 dk)
- Kanarya eksik/çift kayıt
- `lease_held == 0` (bir shard'da shipper yok)
- `tokens_age_seconds > 600`
- Compaction birikimi: bir günde L0 > 64

### 16.5 Log ve iz
Yapısal JSON log; istek kimliği uçtan uca. Event içeriği loglanmaz. Sorgu servisinde yavaş sorgu logu (plan özeti + taranan bayt).

---

## 17. Test stratejisi

### 17.1 Wire test vektörleri
`spec/vectors/` altında: girdi (mantıksal batch, JSON) ↔ beklenen baytlar (hex). KMP SDK, Web SDK, Go doğrulayıcı ve Rust çözücü aynı vektörleri CI'da geçmek zorundadır. Format değişikliği önce vektörlerde yapılır.

### 17.2 PostHog uyum testi
CI'da resmi PostHog SDK'ları (en az: web, Android, iOS, sunucu SDK'larından biri) ingestion'a yönlendirilir; capture, identify, alias, batch, flags çağrıları çalıştırılır. Doğrulama: SDK hata vermiyor, kuyruk boşalıyor, event'ler sorguda beklenen alanlarla görünüyor. SDK sürümleri sabitlenir ve periyodik olarak güncellenip yeniden koşulur.

### 17.3 Diferansiyel test (sorgu doğruluğu)
Rastgele üretilmiş veri kümeleri üzerinde her özel operatör (funnel, retention, tekil sayım) aynı Parquet dosyalarında DataFusion'ın düz SQL ile ifade edilmiş referans sorgusuyla karşılaştırılır. Uyumsuzluk CI'ı kırar. Üreteç kenar durumları hedefler: aynı milisaniyede event'ler, pencere sınırında event'ler, kimlik birleşmesi öncesi/sonrası, geç gelen veri, tekrar eden `uuid`.

### 17.4 Özellik tabanlı ve fuzz testleri
- Çözücüler: `cargo fuzz`, Go `testing.F`.
- Özellikler: `decode(encode(x)) == x`; compaction öncesi ve sonrası sorgu sonuçları aynı; shipper'ın yeniden çalıştırılması sonucu değiştirmez; segmentlerin işlenme sırası sonucu değiştirmez.

### 17.5 Hata enjeksiyonu
- Ingestion'ı fsync ile yanıt arasında öldür → kanarya kayıp/çift göstermemeli.
- Shipper'ı adım 8–11 arasında her noktada öldür → sonuç değişmemeli.
- Object storage'ı 10 dk kes → ingestion kabul etmeye devam eder, sonra yetişir.
- Manifest'e eşzamanlı yazım → yalnızca biri kazanır, diğeri yeniden dener.
- SDK: gönderim sırasında süreci öldür, diski doldur, saati ileri/geri al.

### 17.6 İzolasyon testi
İki proje; A'nın anahtarıyla B'nin verisine ulaşmayı deneyen sorgu kümesi (SQL enjeksiyonu, yol manipülasyonu, filtre ifadeleri). Tümü boş ya da `403` dönmelidir.

### 17.7 Yük testi
- Ingestion: tek node'da kademeli artan yük; doyma noktası, p99 gecikme, CPU/disk profili.
- Uçtan uca: hedef hacmin 3 katında 24 saat; tazelik ve compaction birikimi izlenir.
- Sorgu: sentetik 1 milyar event'lik proje üzerinde standart sorgu seti; soğuk ve sıcak önbellek.

### 17.8 SDK testleri
Ortak mantık `commonTest`'te; platform katmanları cihaz/simülatör üzerinde enstrümantasyon testleriyle. Sahte sunucu tüm durum kodlarını ve ağ hatalarını üretir.

---

## 18. Dağıtım ve operasyon

### 18.1 Paketleme
Her bileşen tek statik binary + minimal container imajı (`distroless`/`scratch`). Yapılandırma ortam değişkenleriyle.

### 18.2 Topolojiler

**Başlangıç (tek makine + object storage):**
ingestion, shipper (tek shard), compactor, query, control plane aynı makinede. Tüm kalıcı veri object storage'da olduğundan makine kaybı yalnızca son ~10 sn'lik WAL'ı ve control SQLite dosyasını riske atar (ikincisi sürekli yedeklenir).

**Ölçekli:**

| Katman | Adet | Not |
|---|---|---|
| Yük dengeleyici | 2 | TLS sonlandırma, sağlık kontrolü `/readyz` |
| Ingestion | N ≥ 2 | Farklı arıza alanlarında; hızlı yerel disk |
| Shipper | shard sayısı + yedek | Kira mekanizması yedeği otomatik devreye alır |
| Compactor | shard başına 1 | CPU ve bellek ağırlıklı |
| Query | M ≥ 2 | NVMe önbellek; bellek ağırlıklı |
| Control plane | 1 + sıcak yedek | Veri düzlemi ona bağımlı değil |

### 18.3 Ölçekleme tetikleyicileri

| Belirti | Eylem |
|---|---|
| Ingestion CPU > %60 ya da fsync p99 yükseliyor | Ingestion node ekle |
| `ship_lag` sürekli artıyor | Shard sayısını artır |
| L0 birikimi | Compactor kaynağını artır |
| Sorgu kuyruğu doluyor | Query node ekle |
| Tek proje sorguları yavaş | Ön toplamlar (11.6); L2'yi kişi aralığına böl ve paralel tara |

Shard sayısı değişimi: yeni shard haritası `control/shards.json`'a sürümlü yazılır; shipper'lar segment zaman damgasına göre eski/yeni haritayı uygular.

### 18.4 Kapasite planlama yöntemi
Girdiler: günlük event sayısı `E`, ortalama sıkıştırılmış Parquet bayt/event `b`, saklama günü `R`, WAL saklama günü `Rw`, WAL bayt/event `w`.

```
veri depolama  ≈ E × b × R × 1,3      (compaction geçişi payı)
wal depolama   ≈ E × w × Rw
ingestion node ≈ tepe_event_sn / node_kapasitesi × 2   (yedeklilik)
```

`b`, `w` ve `node_kapasitesi` Faz 1–2 sonunda gerçek veriyle ölçülür; ölçülmeden donanım kararı verilmez.

### 18.5 Yedekleme ve felaket kurtarma
- **Object storage:** sürümleme açık (yanlışlıkla silmeye karşı), isteğe bağlı bölgeler arası çoğaltma.
- **Yeniden inşa:** `data/` kaybolursa son 30 gün `wal/` üzerinden yeniden üretilebilir (`shipper --rebuild --from … --to …`).
- **Control DB:** sürekli çoğaltma (SQLite için akış yedeği ya da Postgres için WAL arşivi) + günlük anlık görüntü. Geri yükleme tatbikatı üç ayda bir.
- Hedefler: RPO ≤ 10 sn (ingestion yerel diski), RTO ≤ 30 dk (yeni makinelerde binary + yapılandırma).

### 18.6 Sürüm ve geçiş
- Wire ve dosya formatlarının sürüm alanı vardır (`LA1`, `LAW1`, manifest `schema_version`). Okuyucular N ve N−1'i destekler.
- Dağıtım sırası: önce okuyucular (query, shipper), sonra yazarlar (ingestion), en son SDK.
- Ingestion sıfır kesintiyle yuvarlanarak güncellenir (Bölüm 6.10).
- SDK'lar semver; kırıcı değişiklik yalnızca majör sürümde.

### 18.7 Runbook başlıkları
Object storage kesintisi · ingestion diski doluyor · shipper gecikmesi · manifest çakışma fırtınası · bozuk segment (karantinaya al, atla, raporla) · hatalı shipper sürümü sonrası yeniden inşa · token sızıntısı (token döndürme) · silme isteği takibi · kanarya alarmı.

---

## 19. Ölçülecek varsayımlar ve açık riskler

| # | Varsayım / risk | Nasıl doğrulanır | Faz |
|---|---|---|---|
| 1 | Seçilen object storage koşullu yazmayı doğru uygular | Eşzamanlı yazım testi | 0 |
| 2 | Kişi sıralı düzen funnel'ı belirgin hızlandırır | Sentetik veriyle DataFusion düz SQL'e karşı benchmark | 2 |
| 3 | Sorgu anı kimlik yeniden yazımı ucuzdur (azınlık satır) | Gerçekçi identify oranlarıyla ölçüm | 3 |
| 4 | Tek dosyada kişi sıralaması trend sorgularını kabul edilemez yavaşlatmaz | Trend benchmark'ı; gerekirse ön toplam | 2–4 |
| 5 | Ingestion node kapasitesi (hedef: ≥ 100 bin event/sn) | Yük testi | 1 |
| 6 | 10 sn'lik yerel WAL penceresi kabul edilebilir | Risk kararı; değilse 6.6 seçenekleri | 1 |
| 7 | Compose semantics ağacı replay için yeterli ayrıntıyı verir | Prototip | 6 |
| 8 | `HttpURLConnection` / `NSURLSession` ile arka plan gönderimi güvenilir | Cihaz testleri | 3 |
| 9 | Özellik terfisi şema patlamasına yol açmaz | Gerçek veride sütun sayısı izleme | 2 |
| 10 | PostHog SDK'ları adaptörün yanıtlarını kabul eder | Uyum testi | 1 |
| 11 | Çok sayıda küçük proje, nesne ve manifest sayısını yönetilemez yapmaz | 10.000 projelik sentetik test | 5 |

---

## 20. Yol haritası (faz ve çıkış ölçütleri)

**Faz 0 — Sözleşmeler**
Wire spec, WAL spec, Parquet şeması, manifest spec, test vektörleri, object storage doğrulaması.
*Çıkış:* vektörler depoda; iki bağımsız kodlayıcı/çözücü (Go, Rust) vektörleri geçiyor.

**Faz 1 — Ingestion**
Sıcak yol, WAL, grup fsync, segment yükleme, token kaydı, hız sınırı, PostHog adaptörü, metrikler.
*Çıkış:* yük testi sonuçları belgeli; hata enjeksiyon testleri geçiyor; uyum testi yeşil.

**Faz 2 — Shipper + sorgu çekirdeği**
WAL → Parquet, manifest, `TableProvider`, trends, funnel operatörü, diferansiyel test altyapısı.
*Çıkış:* varsayım 2 ve 4 ölçüldü; kanarya uçtan uca çalışıyor.

**Faz 3 — KMP SDK `core` + kimlik**
SDK, kimlik eşleme, kişi özellikleri, sorgu anı çözümleme.
*Çıkış:* Android ve iOS örnek uygulamada 7 günlük kayıpsız çalışma (kanarya SDK üzerinden).

**Faz 4 — Compaction, saklama, silme, retention**
L1/L2, GC, tombstone, retention operatörü, sonuç önbelleği.
*Çıkış:* compaction öncesi/sonrası eşdeğerlik testi; silme akışı uçtan uca doğrulandı.

**Faz 5 — Control plane, UI, flags, Web SDK**
Proje/token yönetimi, sorgu oluşturucu, dashboard, flag yönetimi ve SDK modülü, Web SDK.
*Çıkış:* izolasyon testi geçiyor; flag hash'i tüm platformlarda aynı vektörleri geçiyor.

**Faz 6 — Session replay**
Wireframe yakalama, `/v1/r`, oynatıcı.
*Çıkış:* varsayım 7 doğrulandı; maskeleme testleri geçiyor.

**Faz 7 — Sertleştirme ve production**
Bölüm 21'deki tüm maddeler.

---

## 21. Production'a hazır olma listesi

**Doğruluk**
- [ ] Wire vektörleri tüm istemci ve sunucularda geçiyor
- [ ] Diferansiyel testler (funnel, retention, tekil sayım) CI'da zorunlu
- [ ] Shipper ve compaction idempotency testleri geçiyor
- [ ] Kanarya 14 gün kesintisiz, sıfır kayıp/çift

**Dayanıklılık**
- [ ] Bölüm 17.5'teki tüm hata enjeksiyon senaryoları geçiyor
- [ ] Object storage 10 dk kesinti tatbikatı yapıldı
- [ ] `data/` yeniden inşa tatbikatı yapıldı ve süre ölçüldü
- [ ] Control DB geri yükleme tatbikatı yapıldı

**Performans**
- [ ] Ingestion doyma noktası ve p99 belgeli
- [ ] Hedef hacmin 3 katında 24 saatlik uçtan uca test
- [ ] Standart sorgu seti için soğuk/sıcak süreler belgeli
- [ ] Kapasite planı ölçülmüş `b`, `w`, node kapasitesiyle yazıldı

**Güvenlik ve gizlilik**
- [ ] İzolasyon testi CI'da zorunlu
- [ ] Çözücüler için fuzz korpusu ve sürekli fuzz
- [ ] Bileşen bazında en az yetkili object storage anahtarları
- [ ] Silme akışı (tombstone → yeniden yazım → replay) doğrulandı
- [ ] Bağımlılık taramaları CI'da; imajlar imzalı
- [ ] Dış güvenlik incelemesi (ingestion ve sorgu uçları)

**Operasyon**
- [ ] Tüm SLO'lar için pano ve alarm
- [ ] Bölüm 18.7'deki runbook'lar yazıldı ve tatbik edildi
- [ ] Sıfır kesintili yuvarlanan güncelleme denendi
- [ ] Format sürüm geçişi (N−1 okuma) denendi
- [ ] Nöbet ve olay yönetimi süreci tanımlı

**SDK**
- [ ] Android, iOS, JVM hedeflerinde yayın; ikili boyut ölçüldü
- [ ] Kuyruk bozulma ve disk dolu senaryoları test edildi
- [ ] iOS privacy manifest ve Android veri güvenliği beyanı hazır
- [ ] Entegrasyon belgeleri ve örnek uygulamalar

---

## Ek A — Depo düzeni

```
/spec            wire.md, wal.md, parquet.md, manifest.md, flags.md, replay.md, vectors/
/ingest          Go: ingestion servisi
/control         Go: control plane API
/engine          Rust workspace
  /codec           wire + WAL çözücü
  /shipper         WAL → Parquet, kimlik, kişi özellikleri
  /compactor       compaction, GC, tombstone
  /query           TableProvider, operatörler, HTTP API
  /difftest        veri üreteci + diferansiyel test koşucusu
/sdk-kmp         core, flags, replay, sample-android, sample-ios
/sdk-web         TypeScript
/ui              SPA
/deploy          container tanımları, örnek yapılandırmalar, panolar, alarmlar
/canary          uçtan uca kanarya işçisi
```

## Ek B — Sözlük

| Terim | Anlam |
|---|---|
| WAL | Write-ahead log; ingestion'ın yerel, ekleme-yalnız dosyası |
| Segment | Kapanıp object storage'a yüklenen WAL dosyası |
| L0/L1/L2 | Parquet dosyalarının compaction seviyeleri |
| Manifest | Bir proje-gün için geçerli dosyaların sürümlü listesi |
| `person_key` | Çözümlenmiş kişi anahtarı; sıralama ve funnel bunun üzerindedir |
| Tombstone | Silme isteği işareti; sorguda anında, dosyalarda compaction'da uygulanır |
| Terfi | Sık görülen bir özelliğin JSON'dan gerçek sütuna alınması |
