# Control plane (`la-control`)

Tasarım: `docs/design.md` §14, §15, §10.5, §16 · Sözleşmeler: `spec/control.md` (API), `spec/manifest.md` §0/§1/§8/§9 (yayınlanan dosyalar), `spec/flags.md` (kural seti ve değerlendirme).

Kullanıcı, org, proje, token, gizli anahtar, flag, kayıtlı sorgu ve silme isteklerini SQLite'ta tutar; her değişikliği object storage'a **anlık görüntü** olarak yayınlar; UI için sorgu servisine vekillik eder. Veri düzlemi control plane'e bağlanmaz: control plane kapalıyken event toplama, flag değerlendirme ve sorgu çalışmaya devam eder.

## Dizinler

| Yol | İçerik |
|---|---|
| `control/cmd/la-control` | Binary: `serve`, `bootstrap`, `backup`, `publish` |
| `control/api` | HTTP katmanı: kimlik doğrulama, rol denetimi, tüm `/api/…` uçları, sorgu vekili, statik UI, sağlık/metrik |
| `control/db` | SQLite şeması, migration'lar, sorgular (WAL, yabancı anahtarlar açık, tek yazar bağlantısı + salt-okur havuz) |
| `control/publish` | Yayıncı (`control/*.json`, `flags/<token>.json`, tombstone'lar) ve silme isteği denetçisi |
| `control/auth` | argon2id parola özeti, TOTP (RFC 6238), kimlik üreteçleri, giriş hız sınırı |
| `control/app` | Bileşenleri bağlar; `bootstrap` mantığı |
| `control/config`, `control/metrics` | Ortam değişkenleri; Prometheus metin çıktısı |
| `golib/flags` | Flag kural seti modeli, doğrulayıcı ve değerlendirici (diğer Go kodları da kullanabilir) |

## Çalıştırma

```sh
CGO_ENABLED=0 go build -o bin/la-control ./control/cmd/la-control

# Tek makine, dosya deposu
export LA_CONTROL_DB=/tmp/la/control.db LA_STORE_URL=file:///tmp/la/store
export LA_QUERY_URL=http://127.0.0.1:8081 LA_QUERY_INTERNAL_SECRET=degistir
export LA_UI_DIR=$PWD/ui/dist LA_COOKIE_SECURE=false   # yalnızca yerel HTTP için false
bin/la-control bootstrap --email admin@example.com --password 'en-az-10-karakter' --org Acme --project Shop
bin/la-control            # = la-control serve
```

İlk kullanıcı `bootstrap` ile ya da UI'daki kurulum ekranıyla (`POST /api/setup`, yalnızca hiç kullanıcı yokken) oluşturulur.

Kapanış (`SIGTERM`/`SIGINT`): dinleyiciler açık istekleri bitirir (en çok 20 sn), yayınlanmamış değişiklik varsa son bir yayın denenir (10 sn), çıkış kodu 0. Kalan olursa sonraki açılış yayınlar; veritabanı doğruluk kaynağıdır.

### `la-control bootstrap`

```
la-control bootstrap --email E --password P --org AD --project AD [--rotate-key] [--json] [--no-publish]
```

- Kullanıcıyı, kullanıcının sahibi olduğu org'u, projeyi, bir `pk_` token'ı (replay açık) ve `query`+`admin` kapsamlı bir `sk_` anahtarı oluşturur; **var olanı yeniden oluşturmaz** (idempotent). Var olan kullanıcının parolasına dokunmaz. Parola `LA_BOOTSTRAP_PASSWORD` ile de verilebilir.
- stdout: `user_id=…`, `org_id=…`, `project_id=…`, `token=pk_…` ve **yalnızca anahtarın oluşturulduğu çalıştırmada** `secret=sk_…` (veritabanında yalnızca SHA-256'sı durur; yeniden gösterilemez. Yenisi için `--rotate-key`). `--json` tek satır JSON basar (`created` alanı bu çalıştırmada oluşturulanları listeler). Tanılama iletileri stderr'dedir.
- `LA_STORE_URL`/`LA_S3_BUCKET` ayarlıysa sonunda bir kez yayınlar. Çıkış kodları: `0` tamam · `1` hata · `2` kullanım/yapılandırma · `3` veritabanı güncellendi ama yayın başarısız (çalışan servis kendiliğinden yeniden dener; ya da `la-control publish`).

### `la-control backup <yol>`

`VACUUM INTO` ile tutarlı, tek dosyalık bir kopya alır; servis çalışırken kullanılabilir, hedef dosya varsa üzerine yazmaz. Geri yükleme: servisi durdur, kopyayı `LA_CONTROL_DB` yoluna koy (`-wal`/`-shm` dosyalarını sil), başlat. Eski bir yedekten dönüldüğünde yayıncı depodaki sürüm numaralarını okur ve onların **üstünden** devam eder (`version`/`v` geri gitmez). Yedek dosyası parola özetlerini ve TOTP sırlarını içerir; `0600` ile yazılır, öyle saklayın.

## Yapılandırma (ortam değişkenleri)

| Değişken | Varsayılan | Anlam |
|---|---|---|
| `LA_CONTROL_LISTEN` | `:8090` | API + UI dinleyicisi |
| `LA_CONTROL_ADMIN_LISTEN` | boş | Verilirse `/metrics` yalnızca bu (iç ağ) dinleyicide sunulur; dış dinleyicide 404 olur |
| `LA_CONTROL_DB` | `/var/lib/la/control.db` | SQLite dosyası (WAL; yanında `-wal`, `-shm`). Dizin yoksa oluşturulur, dosya `0600` |
| `LA_QUERY_URL` | `http://127.0.0.1:8081` | Sorgu servisi |
| `LA_QUERY_INTERNAL_SECRET` | boş | Sorgu servisiyle paylaşılan sır (`X-LA-Internal`). Boşsa vekil `503 query_not_configured` döner |
| `LA_CONTROL_QUERY_TIMEOUT_SECONDS` | `120` | Vekil isteği için üst süre (aşılırsa `504`) |
| `LA_UI_DIR` | boş | Derlenmiş SPA dizini. Boşsa UI sunulmaz (API dışı yollar 404) |
| `LA_COOKIE_SECURE` | `true` | `true`: çerez `Secure`, yanıtlarda HSTS (TLS arkasında çalışıldığını bildirir). `false` yalnızca yerel geliştirme içindir |
| `LA_TRUSTED_PROXIES` | `0` | Önümüzdeki güvenilir vekil sayısı; giriş hız sınırı için istemci IP'si `X-Forwarded-For`'un sağdan N. girdisinden alınır (`0`: TCP eş adresi). Ingestion ile aynı anlam |
| `LA_CONTROL_PUBLISH_DEBOUNCE_MS` | `250` | Değişiklikten sonra yayın öncesi bekleme (art arda değişiklikler tek yayında toplanır). En çok `2000` |
| `LA_CONTROL_REPUBLISH_SECONDS` | `300` | Tam yeniden yayın aralığı (elle silinen/bozulan dosyayı onarır) |
| `LA_CONTROL_DELETION_CHECK_SECONDS` | `60` | Silme isteklerinin `done` denetimi aralığı |
| `LA_LOG_LEVEL` | `info` | `debug` statik dosya ve sağlık isteklerini de loglar |
| `LA_STORE_URL`, `LA_S3_ENDPOINT`, `LA_S3_REGION`, `LA_S3_ACCESS_KEY`, `LA_S3_SECRET_KEY`, `LA_S3_BUCKET` | — | Depo (`spec/manifest.md` §0). Control plane `control/`, `flags/`, `tombstones/` altına yazar, `data/…/_m/` altını okur |

Geçersiz değer servisin başlamasını engeller (çıkış kodu 2); hatalı değişkenlerin hepsi adlarıyla birlikte loglanır.

## Yayınlama

- Her yayın tüm belgeleri veritabanından **yeniden üretir**. `version` (ve proje başına flag `v`'si) yalnızca içerik değiştiğinde 1 artar ve veritabanında saklanır; değişiklik yoksa aynı baytlar yazılır (veri düzleminin `If-None-Match` yoklamaları 304 kalır).
- Değişiklik → en geç `debounce + yayın süresi` (varsayılan ~0,3 sn; sözleşme ≤ 2 sn). Depo erişilemezse API isteği yine başarılıdır; yayın 1 sn → 30 sn geri çekilmeyle sonsuza dek yeniden denenir, `la_control_publish_lag_seconds` büyür (`deploy/prometheus/alerts.yml` 120 sn'de uyarır).
- Token `disabled` olunca ya da silinince `flags/<token>.json` **silinir**; silinen token `tokens.json`'dan çıkar (ingestion 401), `disabled` token dosyada kalır.
- Proje kapatma (`DELETE /api/projects/{pid}`): token'lar `disabled`, `retention_days = replay_retention_days = 0` ve `projects.json` girdisine `"closed": true` eklenir; anahtarları `keys.json`'dan çıkar. Proje satırı silinmez, `id` yeniden kullanılmaz.
- Tombstone'lar proje içinde `ts_ms` sırasıyla yazılır ve `ts_ms` proje başına kesin artandır (compactor'ın `tombstone_ts` eşiği hiçbir isteği atlayamasın diye).
- **Bir depo öneki tek bir control plane'e aittir**: tam yayın, `flags/` altında etkin bir token'a ait olmayan `pk_….json` dosyalarını siler.

## Güvenlik notları

- Parola: argon2id `m=64 MiB, t=3, p=1`, en az 10 karakter; eşzamanlı özet sayısı 4 ile sınırlıdır (bellek). Bilinmeyen e-posta için de aynı maliyet harcanır (zamanlamadan hesap sızmaz).
- Oturum: `la_session` 256 bit rastgele, veritabanında yalnızca SHA-256'sı; `HttpOnly`, `SameSite=Lax`, `Secure`; 30 gün kayan süre. Parola değişince ve TOTP açılınca diğer oturumlar kapanır.
- CSRF: çerezli her durum değiştiren istekte `X-LA-CSRF` (değer `GET /api/me` → `csrf`). Ek olarak `Sec-Fetch-Site: cross-site` bildiren istekler (giriş ve kurulum dahil) reddedilir ve çerezli/kimliksiz isteklerde gövde `application/json` olmak zorundadır. `sk_` isteklerinde bunlar aranmaz.
- Giriş: IP + e-posta başına dakikada 5 **başarısız** deneme (ayrıca IP başına 50), sonrası `429 + Retry-After`. Oturum içi parola doğrulamaları (parola değiştirme, TOTP kapatma/açma) kullanıcı başına aynı sınırla korunur.
- TOTP: SHA-1, 30 sn, 6 hane, ±1 adım tolerans; kullanılmış adım yeniden kabul edilmez.
- `sk_` anahtarları yalnızca SHA-256 olarak saklanır ve yayınlanır; `Authorization: Bearer sk_…` yalnızca **admin** kapsamıyla ve yalnızca kendi projesinin `/api/projects/{pid}…` uçlarında geçerlidir (başka proje: 404, org/kullanıcı uçları: 403).
- Sorgu vekili yukarı yöne yalnızca `Accept`, `Accept-Encoding`, `Content-Type` başlıklarını geçirir; istemcinin `X-LA-*`, `Cookie`, `Authorization` başlıkları atılır. Sorgu servisinin `401`'i istemciye `502 upstream_auth` olarak döner (UI 401'i "oturum bitti" sayar).
- Denetim kaydı (`audit_log`) her yönetim işlemini aynı veritabanı işleminde yazar; parola, anahtar, özet, oturum değeri ve silinen kişilerin kimlikleri kayda girmez.
- Statik UI `os.Root` üzerinden sunulur (dizin dışına çıkış ve sembolik bağ kaçışı yok, nokta ile başlayan dosyalar sunulmaz). `/assets/*` → `immutable`, diğer her şey (`index.html` dahil) → `no-cache`.
- Yanıt başlıkları: `Content-Security-Policy` (API: `default-src 'none'`; UI: `default-src 'self'`), `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, `X-Frame-Options: DENY`, TLS arkasında HSTS.
- Hata iletileri (`error.message`) UI'da gösterildiği için Türkçedir; istemciler kararlı İngilizce `error.code` değerine bakmalıdır.

## Metrikler ve log

`/metrics` (`la_control_` önekli): `publish_lag_seconds`, `publish_success_total`, `publish_failures_total`, `publish_objects_total`, `publish_last_success_timestamp_seconds`, `deletions_pending`, `sessions`, `http_requests_total{code}`, `http_request_seconds`, `logins_total{result}`, `query_proxy_requests_total{result}`, `audit_entries_total`, `key_auth_failures_total`.

Loglar stdout'a yapısal JSON'dur; her istek `req_id` (gelen `X-Request-Id` kullanılır, sorgu servisine de iletilir), rota kalıbı, durum, süre ve aktörle loglanır. İstek gövdeleri ve sırlar loglanmaz. `/healthz` süreç canlılığı, `/readyz` veritabanı erişimidir (depo kesintisi hazır olmayı bozmaz).

## Testler

```sh
GOTOOLCHAIN=local go vet ./control/... ./golib/flags/...
GOTOOLCHAIN=local go test ./control/... ./golib/flags/...
GOTOOLCHAIN=local go test -race ./control/... ./golib/flags/...

# Fuzz (her biri ~30 sn)
go test ./control/api/ -run '^$' -fuzz '^FuzzFlagDefinition$' -fuzztime 30s
go test ./control/api/ -run '^$' -fuzz '^FuzzRequestBodies$'  -fuzztime 30s
go test ./golib/flags/ -run '^$' -fuzz '^FuzzParseRules$'     -fuzztime 30s
go test ./golib/flags/ -run '^$' -fuzz '^FuzzCond$'           -fuzztime 30s
```

Kapsam: rol matrisi (4 rol × her uç sınıfı, başka org → 404, anonim → 401, `sk_` anahtarları), kimlik doğrulama (kurulum, giriş/çıkış, hız sınırı, TOTP + RFC 6238 vektörleri, CSRF, oturum süresi, parola değişimi), yayın (dosyaların bayt düzeyinde biçimi, `tokens.json`'ın gerçek `ingest/tokens` ile yüklenmesi, depo kesintisi, yeniden başlatma sonrası sürümler), flag doğrulama ve `spec/vectors/flags.json`, silme istekleri, sorgu vekili, statik UI, `bootstrap`/`backup` komutları.

## Bilinen eksikler

- **OIDC yok** (v1'de belgelenmiş eksik); `users.oidc_subject` sütunu hazır.
- E-posta gönderimi, parola sıfırlama, davet akışı yok: üyenin ilk parolasını org sahibi verir; parolasını unutan kullanıcı için bir yönetici aracı henüz yok.
- Org oluşturma/yeniden adlandırma ucu yok (sözleşmede tanımlı değil); yeni org `bootstrap` ile açılır. Kullanıcı silme yok.
- Tek süreç varsayılır: giriş hız sınırı sayaçları bellektedir (yeniden başlatmada sıfırlanır, kopyalar arasında paylaşılmaz); SQLite tek makineyle sınırlar (çok node için tasarım Postgres öngörür, uygulanmadı).
- Sorgu vekili yalnızca sözleşmedeki uçları iletir; yanıtları yorumlamaz, önbelleklemez.
- `replay_sample_rate` yalnızca API'de saklanır; yayınlanan hiçbir dosyada tüketicisi yoktur.
- Sürekli çoğaltma (design §18.5 "akış yedeği") kapsam dışıdır; `backup` komutu günlük anlık görüntü içindir.
