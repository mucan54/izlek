# Uçtan uca test

Gerçek binary'lerle tek makinede tüm hattı çalıştırır; sahte servis yoktur:

```
Web SDK (Chromium) ─┐
KMP SDK (JVM örneği) ├→ la-ingest → WAL → la-shipper → Parquet → la-query
PostHog JSON, kanarya┘                la-control (API, yayın, sorgu vekili, UI)
```

```sh
(cd engine && cargo build -p la-shipper -p la-query)
(cd sdk-web && npm ci && npm run build)
(cd ui && npm ci && npm run build)
node e2e/run.mjs            # file:// depo
node e2e/run.mjs --s3       # aynı senaryo, S3 protokolü üzerinden (golib/devs3)
```

Seçenekler: `--skip-kmp`, `--skip-ui`, `--keep` (çalışma dizinini silme), `--verbose`. Rust binary'leri başka yerdeyse `LA_E2E_SHIPPER` / `LA_E2E_QUERY`; Gradle çevrimdışı çalışamıyorsa `LA_E2E_GRADLE_ARGS=""`. Tarayıcı için makinede kurulu Playwright kullanılır (depo bağımlılığı değildir). Başarısızlıkta servis loglarının sonu basılır ve çalışma dizini bırakılır.

## Doğrulananlar

| Adım | Neyi kanıtlar |
|---|---|
| bootstrap | Kullanıcı/org/proje/token/anahtar oluşur, kontrol dosyaları yayınlanır, gizli anahtarın yalnızca özeti yayınlanır, ikinci çalıştırma hiçbir şey oluşturmaz |
| kanarya | `la-canary -once`: gönderilen her event sorguda tam bir kez görünür |
| uç kimlik denetimi | Geçersiz token/anahtar `401`, yazma token'ı sorgu anahtarı değildir, sahte iç başlık reddedilir, DDL ve dosya okuyan SQL `400` |
| flag | API ile oluşturulan flag ingestion'ın sunduğu `/flags/<token>.json` içinde görünür |
| Web SDK | Chromium'da varsayılan yapılandırmayla: `identify` öncesi event'ler aynı kişiye bağlanır, `data-track`, kişi özelliği, flag değeri sunucununkiyle aynı, `flag_exposed`, replay yüklemesi |
| replay | Parça `/q/events` ile listelenir, `/q/replay` ile iner, çözülür; yazılan parola ve metin kutusu içeriği kayıtta yoktur; başka projenin öneki `403` |
| PostHog uyumu | `/batch` JSON, `$identify` anonim geçmişi birleştirir, aynen yeniden gönderilen batch çift sayılmaz |
| KMP SDK | JVM örneği event gönderir; Kotlin değerlendirici yayınlanan kural setinde Go ile aynı varyantı seçer |
| sorgu API'si | trends, funnel (kimlik birleşimi dahil), retention, sayfalama, şema, kişi özelliği filtresi |
| compaction | `compact --once` sonrası tüm sayımlar aynıdır, günün dosya sayısı azalır |
| silme isteği | Tombstone kişiyi (anonim geçmişiyle) hemen gizler, sonradan gelen event'i yazılmaz, istek `done` olur, başkaları etkilenmez |
| UI | Control plane'in sunduğu SPA: hatalı/doğru giriş, canlı event'ler, trend grafiği, flag listesi, replay oynatıcı; CSRF başlığı olmadan `403` |
| token yaşam döngüsü | Devre dışı → `403`, silinmiş → `401`; kural seti dosyası kalkar |
| kapanış | `SIGTERM`'de ingestion `0` ile çıkar, yerelde segment kalmaz, son event sorguya ulaşır |
| proje kapatma | Anahtar çalışmaz olur, compactor projenin tüm öneklerini siler |

## Kapsamadıkları

Gerçek bir S3 sağlayıcısı (imza doğrulaması, koşullu yazma), birden çok ingestion node'u, yük ve dayanıklılık (çökme enjeksiyonu birim/entegrasyon testlerindedir), Android/iOS cihazları, resmi PostHog SDK'ları (`ingest/compat`, ayrı etiketli testler).
