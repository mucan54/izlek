# Ingestion servisi (`la-ingest`)

Tasarım: `docs/design.md` §3, §6, §13.5, §16.1 · Sözleşmeler: `spec/wire.md`, `spec/wal.md`, `spec/manifest.md` §0/§1/§8, `spec/replay.md` §4, `spec/flags.md` §6.

Sıcak yol yalnızca doğrular ve diske ekler: token → hız sınırı → sınırlı okuma (+gzip) → çerçeve denetimi → `batch_id` tekilleştirme → WAL + grup fsync → `200`. **`200` yalnızca fsync başarıyla döndükten sonra yazılır.**

## Dizinler

| Yol | İçerik |
|---|---|
| `ingest/cmd/la-ingest` | Servis binary'si |
| `ingest/cmd/la-loadgen` | `/v1/e` için kademeli yük üreteci |
| `ingest/server` | HTTP katmanı: `/v1/e`, `/v1/r`, PostHog adaptörü, sağlık, metrik, kapanış |
| `ingest/wire` | LA1 çerçeve doğrulayıcı (bellek ayırmaz) + test/yük için kodlayıcı |
| `ingest/wal` | LAW1 segment formatı, tek yazar + grup fsync, kurtarma, yükleyici |
| `ingest/tokens` | `control/tokens.json` kaydı (ETag ile yoklama, diskte son iyi kopya) |
| `ingest/ratelimit`, `ingest/dedupe`, `ingest/metrics` | Token-bucket, LRU, Prometheus metin çıktısı |
| `ingest/geoip` | Elle yazılmış MMDB okuyucu + `X-Forwarded-For` çözümü (`geoiptest`: test için sentetik MMDB üreteci) |
| `ingest/compat` | Gerçek PostHog SDK'larıyla uyum testleri (build tag `compat`) |
| `golib/objstore`, `golib/devs3`, `golib/murmur3` | Ortak depo istemcisi, geliştirme S3 sunucusu, murmur3_32 |

## Çalıştırma

```sh
go build -o bin/la-ingest ./ingest/cmd/la-ingest
go build -o bin/devs3     ./golib/devs3/cmd/devs3

# 1) Tek makine, dosya deposu
mkdir -p /tmp/la/store/control
echo '{"version":1,"tokens":{"pk_dev":{"project_id":1,"status":"active","replay":true}}}' > /tmp/la/store/control/tokens.json
LA_STORE_URL=file:///tmp/la/store LA_WAL_DIR=/tmp/la/wal LA_NODE_ID=ing-01 bin/la-ingest

# 2) S3 uyumlu depo (yerelde devs3; imza doğrulamaz, bucket'ı kendiliğinden açar)
bin/devs3 -listen 127.0.0.1:9000 &
LA_STORE_URL=s3://la-dev LA_S3_ENDPOINT=http://127.0.0.1:9000 LA_S3_ACCESS_KEY=x LA_S3_SECRET_KEY=x \
  LA_WAL_DIR=/tmp/la/wal LA_NODE_ID=ing-01 bin/la-ingest
```

`control/tokens.json` hiç yüklenemediyse (depoda yok, diskte son iyi kopya da yok) node `401` değil **`503 + Retry-After`** döner ve `/readyz` başarısız olur; dosya gelince kendiliğinden toparlar. (`401` SDK'nın batch'i atmasına yol açar; token kümesi bilinmeden bu karar verilemez.)

Kapanış (`SIGTERM`/`SIGINT`): `/readyz` 503 → `LA_DRAIN_SECONDS` bekle → açık istekleri bitir → segmenti kapat → yükle → çık (kod 0). Yükleme `LA_SHUTDOWN_UPLOAD_SECONDS` içinde bitmezse segmentler yerel diskte kalır, çıkış kodu 1 olur; sonraki açılış yükler. İkinci sinyal beklemeden çıkar.

## Yapılandırma (ortam değişkenleri)

| Değişken | Varsayılan | Anlam |
|---|---|---|
| `LA_LISTEN` | `:8080` | Dış dinleyici |
| `LA_ADMIN_LISTEN` | boş | Verilirse `/metrics` yalnızca bu (iç ağ) dinleyicide sunulur; dış dinleyicide 404 olur |
| `LA_NODE_ID` | `ing-01` | `[a-z0-9-]{1,32}`; segment adlarına girer, node başına tekil olmalı |
| `LA_WAL_DIR` | `/var/lib/la/wal` | Yerel WAL dizini (ayrıca `seq.hwm` ve varsayılan token kopyası burada) |
| `LA_WAL_SEGMENT_BYTES` | `67108864` | Segment rotasyon boyutu |
| `LA_WAL_SEGMENT_SECONDS` | `10` | Segment rotasyon süresi (kesirli olabilir) |
| `LA_WAL_FSYNC_MS` | `2` | Grup fsync bekleme süresi; `0` = bekleme yok (fsync sürerken biriken istekler gruplanır) |
| `LA_WAL_FSYNC_BYTES` | `1048576` | Bu kadar bayt birikince beklemeden fsync |
| `LA_WAL_MAX_LOCAL_BYTES` | WAL dosya sisteminin %70'i | Yüklenmemiş yerel WAL bu sınırı aşınca `/readyz` başarısız, yazma uçları `503 + Retry-After: 5` |
| `LA_WAL_UPLOAD_CONCURRENCY` | `2` | Eşzamanlı segment yükleme sayısı |
| `LA_STORE_URL` | — | `s3://<bucket>[/<önek>]` ya da `file:///mutlak/dizin` |
| `LA_S3_ENDPOINT` | AWS | S3 uyumlu uç (path-style) |
| `LA_S3_REGION` | `us-east-1` | SigV4 bölgesi |
| `LA_S3_ACCESS_KEY`, `LA_S3_SECRET_KEY` | — | Kimlik |
| `LA_S3_BUCKET` | — | `LA_STORE_URL` yoksa `s3://$LA_S3_BUCKET` |
| `LA_TOKENS_KEY` | `control/tokens.json` | Token dosyasının anahtarı |
| `LA_TOKENS_POLL_SECONDS` | `30` | `If-None-Match` ile yoklama aralığı |
| `LA_TOKENS_CACHE` | `$LA_WAL_DIR/tokens.json` | Son iyi kopyanın yolu |
| `LA_COMPAT_POSTHOG` | `true` | PostHog uçlarını aç/kapat (`false` → hepsi 404) |
| `LA_GEO_DB` | boş | Ülke MMDB dosyası (GeoLite2-Country / DB-IP düzeni). Boşsa ülke hep `ZZ`. Verilip okunamazsa servis başlamaz |
| `LA_TRUSTED_PROXIES` | `0` | Önümüzdeki güvenilir vekil sayısı. `0`: TCP eş adresi kullanılır, `X-Forwarded-For` yok sayılır. `N`: başlığın sağdan N. girdisi istemcidir (daha soldakiler istemci girdisidir, güvenilmez) |
| `LA_RATE_TOKEN` | `2000` | Token başına istek/sn (token'ın `rate_limit` alanı ezer; `0` = kapalı) |
| `LA_RATE_IP` | `50` | IP başına istek/sn (IPv6 için /64; `0` = kapalı). Patlama payı 1 sn'lik değerdir. **Yük dengeleyici arkasında `LA_TRUSTED_PROXIES` doğru ayarlanmazsa tüm istemciler dengeleyicinin tek IP'si sayılır ve bu sınıra birlikte takılır** |
| `LA_DEDUPE_ENTRIES` / `LA_DEDUPE_TTL_SECONDS` | `100000` / `600` | `(project_id, batch_id)` LRU'su |
| `LA_MAX_INFLIGHT` | `1024` | Eşzamanlı işlenen istek sınırı (aşan `503 + Retry-After: 1`); her istek en çok ~2 MB tampon tutar |
| `LA_DRAIN_SECONDS` | `5` | Kapanışta `/readyz` başarısız olduktan sonra bekleme |
| `LA_SHUTDOWN_UPLOAD_SECONDS` | `60` | Kapanışta yüklemeyi bekleme üst sınırı |
| `LA_LOG_LEVEL` | `info` | `debug` her isteği loglar (içerik ve token asla loglanmaz) |

## Uçlar

| Uç | Davranış |
|---|---|
| `POST /v1/e` | LA1 batch. `Authorization: Bearer pk_…` ya da `?t=`. `Content-Encoding: gzip` isteğe bağlı. 200 / 400 / 401 / 403 / 413 / 429 / 503 (`spec/wire.md` §3–4). Boş batch: 200, yazılmaz. Tekrar eden `batch_id`: 200, yazılmaz |
| `POST /v1/r?s=&n=&ts=&d=&a=[&u=][&t=]` | Replay parçası. `Content-Encoding: gzip`/`zstd` zorunlu (yoksa 400), gövde ≤ 1 MiB, token `replay: true` değilse 403. Gövde akış halinde depoya yazılır, sonra `format=3` indeks kaydı fsync edilir |
| `POST /batch`, `/i/v0/e`, `/e`, `/capture` (`/` ile de) | PostHog adaptörü → `format=2`. `Content-Encoding: gzip`, `?compression=gzip-js`, form `data=<base64 ya da JSON>`, çıplak base64, `GET ?data=`, düz JSON. Token sırası: `api_key` → `token` → ilk event `properties.token` → `Bearer` → `?t=`. Yanıt `200 {"status": 1}` |
| `GET /flags/<token>.json` | Control plane'in yayınladığı kural seti (`spec/flags.md` §1). Bilinmeyen/devre dışı token ve yayınlanmamış küme: 404. `ETag` + `Cache-Control: public, max-age=60`, `If-None-Match` → 304. Token başına bellekte tutulur, depodan en çok 30 sn'de bir yenilenir; depo hatasında son kopya sunulur. `LA_COMPAT_POSTHOG`'dan bağımsızdır |
| `/decide`, `/flags` (her yöntem), `GET /array/<token>/config` | Sabit boş flag yanıtı (`spec/flags.md` §6) |
| `GET /healthz`, `/readyz`, `/metrics` | Canlılık; hazır olma (kapanış, geri basınç, token yok → 503 + neden); Prometheus (`la_ingest_*`) |

CORS: `Origin` taşıyan isteklerde izinliyse `Access-Control-Allow-Origin: <origin>`; `OPTIONS` ön uçuşu `Access-Control-Max-Age: 86400`. Ön uçuşta token bilinmediğinden (başlık gönderilmez) köken, **herhangi bir** etkin token kabul ediyorsa geçer (`?t=` varsa o token'a göre); asıl istek kendi token'ının `allowed_origins` listesine göre denetlenir (uymazsa 403). `Origin` başlığı olmayan istek (mobil/sunucu) listeye takılmaz.

## Dayanıklılık

- Tek yazar goroutine; `write` + `fsync` (+ yeni segmentte dizin fsync) bitmeden hiçbir istek onaylanmaz. Yazma/fsync hatasında o gruptaki istekler `503` alır, segmentin kalıcı kısmı kapatılıp yüklenir, sonraki yazım **yeni** segmente gider (hata vermiş dosyada fsync yeniden denenmez).
- Yerel dosyalar: `<node>-<seq:012>.wal` (açık), `<node>-<seq:012>.<kapanış_unix_ms>.sealed` (yüklenmeyi bekliyor). Kapanış anı dosya adında durur; böylece çökme sonrası yeniden yükleme **aynı** nesne anahtarına gider (`wal/dt=…/hh=…` kapanış saatinden türetilir).
- Yükleme: zstd seviye 3, koşulsuz `PUT`; yerel dosya yalnızca `PUT` başarılı olunca silinir; hata → üstel backoff (0,5 sn → 30 sn, jitter) ile sonsuz tekrar.
- Açılışta kurtarma: `.wal` dosyaları taranır, yarım/CRC'si bozuk kuyruk kırpılır (bu baytlar hiç onaylanmamıştır), dosya mühürlenip yüklenir. Hiç geçerli girdisi olmayan dosya silinir; başı tanınmayan dosyaya dokunulmaz (log + sonraki açılışta yeniden denenir).
- `seq` başlangıcı = `max(yereldeki en büyük seq + 1, unix_sn)`; ek olarak `seq.hwm` dosyasındaki en yüksek değer de hesaba katılır (yerel dizin boşken saat geride kalsa bile numara yinelenmez).
- **Çökme + istemci tekrarı:** kurtarma, diskteki segmentlerden `(project_id, batch_id)` LRU'sunu yeniden doldurur. fsync ile yanıt arasında çöken node aynı WAL diziniyle açılırsa tekrar gönderilen batch yazılmadan `200` alır (kayıp yok, çift yok). LRU'nun **göremediği** durumlar: (a) segment çökmeden önce yüklenip yerelden silinmişse, (b) tekrar başka bir node'a giderse, (c) LRU süresi/kapasitesi dolmuşsa, (d) aynı batch eşzamanlı iki istekle gelirse. Bu durumlarda batch WAL'a iki kez yazılır; `uuid` bazlı ayıklama shipper/compaction/sorgu tarafındadır (`spec/wire.md` §4). PostHog (`format=2`) ve replay (`format=3`) girdileri bu katmanda tekilleştirilmez (shipper deterministik `uuid` türetir).

## Testler

```sh
go vet ./...
go test ./...                                   # tümü
go test -race ./golib/... ./ingest/...          # eşzamanlılık ağırlıklı paketler

# Fuzz (her biri ~30 sn)
go test ./ingest/wire/   -run '^$' -fuzz '^FuzzValidate$'      -fuzztime 30s -fuzzminimizetime 2s
go test ./ingest/wal/    -run '^$' -fuzz '^FuzzReader$'        -fuzztime 30s -fuzzminimizetime 2s
go test ./ingest/server/ -run '^$' -fuzz '^FuzzScanPostHog$'   -fuzztime 30s -fuzzminimizetime 2s
go test ./ingest/server/ -run '^$' -fuzz '^FuzzDecodePostHog$' -fuzztime 30s -fuzzminimizetime 2s
go test ./ingest/geoip/  -run '^$' -fuzz '^FuzzParse$'         -fuzztime 30s -fuzzminimizetime 2s

# PostHog SDK uyumu (Node.js + Playwright/Chromium gerekir)
(cd ingest/compat && npm ci) && go test -tags compat -count=1 -v ./ingest/compat/

# Gerçek bir S3 uygulamasına karşı depo sözleşmesi (isteğe bağlı; ör. MinIO)
LA_TEST_S3_ENDPOINT=http://127.0.0.1:9000 LA_TEST_S3_ACCESS_KEY=… LA_TEST_S3_SECRET_KEY=… go test ./golib/objstore/
```

Kapsanan başlıklar: wire vektörlerinin tamamı (çerçeve düzeyi ve HTTP üzerinden, düz + gzip); WAL gidiş-dönüş ve yarım kuyruk kurtarma; fsync'ten önce onay yok (enjekte edilen dosya ile); yazma/fsync hatası; rotasyon + yükleme + yeniden başlatma; tekilleştirme; geri basınç; hız sınırları; token yenileme ve son iyi kopya; PostHog gövde biçimleri; replay yükleme; kapanış; hata enjeksiyonu (fsync–yanıt arası çökme, depo kesintisi `fail`/`pause`); eşzamanlı koşullu PUT'ta tek kazanan (devs3 ve `file://`).

### PostHog uyum testleri (`ingest/compat`)

Sabitlenmiş sürümler (`package.json` + `package-lock.json`):

| SDK | Sürüm | Sonuç |
|---|---|---|
| `posthog-node` | 5.74.0 | capture, identify, alias, flush, shutdown, flag okuma: hata yok, kuyruk boş, 15 event (+ SDK'nın `$feature_flag_called` event'i) `format=2` olarak WAL'da (`POST /batch/`) |
| `posthog-js` (headless Chromium, ayrı köken → gerçek CORS) | 1.451.1 | otomatik + elle `$pageview`, capture, identify: konsol/ağ hatası yok, kuyruklar boş, event'ler WAL'da (`POST /e/`, `/flags/`, `GET /array/<token>/config`). Bu sürüm gövdeyi gzip'ler ama **ne `Content-Encoding: gzip` ne de `?compression=gzip-js` gönderir** (`Content-Type: text/plain`); adaptör gzip'i sihirli sayıdan (`1f 8b`) tanır |
| `posthog-node` | 6.0.4 | **Uyumsuz (bilinen eksik).** 6.x yalnızca capture v1 ucuna (`/i/v1/analytics/events`) gönderir, eski uca dönüş seçeneği yoktur. Tasarım §3.7 v1 capture uçlarını kapsam dışı bırakır → 404, SDK "undelivered" hatası raporlar. `TestPostHogNodeV6CaptureV1NotSupported` bu durumu belgeler |

Notlar: posthog-js otomasyon kokan user-agent'larda (headless Chromium) tüm event'leri kendisi atar; test sayfası SDK'nın önerdiği `opt_out_useragent_filter: true` ayarını kullanır, başka ayar değiştirilmez. **Android ve iOS PostHog SDK'ları bu Linux konteynerinde çalıştırılamaz; denenmedi.** Gövde biçimleri (`/batch` JSON, gzip, form/base64) birim testlerinde kapsanır ama gerçek mobil SDK doğrulaması yapılmamıştır.

## Yük ölçümü

> **Bu bir kapasite iddiası değildir.** Tek bir ölçümdür: bu konteyner (4 vCPU Intel Xeon @ 2.10 GHz, ext4 / virtio disk), `file://` depo, yük üreteci **aynı makinede** (aynı 4 çekirdeği paylaşıyor) ve ölçüm sırasında makinede başka derlemeler de çalışıyordu (yük ortalaması ≈ 5). Varsayılan ayarlar (`LA_WAL_FSYNC_MS=2`), yalnızca `LA_RATE_IP=0`, `LA_MAX_INFLIGHT=4096` ve token `rate_limit` yükseltildi. Batch = 20 event, 3 115 bayt (≈155 bayt/event), gzip yok; her adım 8 sn, kapalı döngü.

```sh
la-loadgen -url http://127.0.0.1:18080 -token pk_load -steps 1,4,16,64,256,512,1024,2048 -step-duration 8s -events 20
```

| Gönderici | istek/sn | event/sn | onay p50 (ms) | onay p99 (ms) | fsync ort. (ms) | fsync p99 (ms) | 200 dışı |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 294 | 5 872 | 3,31 | 5,37 | 0,72 | 2,81 | 0 |
| 4 | 1 136 | 22 717 | 3,44 | 5,25 | 0,78 | 2,23 | 0 |
| 16 | 3 757 | 75 148 | 3,81 | 6,27 | 1,19 | 3,22 | 0 |
| 64 | 11 450 | 228 993 | 4,97 | 16,92 | 1,71 | 14,78 | 0 |
| 256 | 23 454 | 469 075 | 9,59 | 35,93 | 2,23 | 14,97 | 0 |
| 512 | 32 053 | 641 050 | 13,62 | 57,28 | 3,07 | 27,16 | 0 |
| 1024 | 35 475 | 709 492 | 25,66 | 73,44 | 3,48 | 21,77 | 0 |
| 2048 | 35 717 | 714 340 | 52,53 | 134,52 | 4,22 | 25,50 | 0 |

- Doyma: ≈ 35 500 istek/sn ≈ 710 000 event/sn (≈ 110 MB/sn gövde); 1024 → 2048 göndericide verim artmıyor, yalnızca gecikme büyüyor.
- Onay gecikmesi tabanı ≈ 3,3 ms (2 ms grup bekleme + fsync). p99 < 50 ms hedefi bu ölçümde 256 göndericiye (≈ 23 000 istek/sn) kadar tutuyor.
- fsync histogram değerleri `la_ingest_fsync_seconds` kovalarından doğrusal ara değerle kestirilmiştir.
- Koşu boyunca 1 151 801 batch (23 036 020 event, 3,59 GB) kabul edildi; 54 segment yüklendi (414 MB zstd, ≈ 8,7×; yük üreteci yalnızca `batch_id` ve kayıt `uuid`'lerini değiştirir, gerçek veride oran farklı olur); `SIGTERM` sonrası yerel WAL boş, çıkış kodu 0.
- event/sn doğrudan batch boyutuna bağlıdır (sunucu kayıt içeriğini çözmez); farklı batch boyutları, gzip'li gövdeler, ayrı makinede yük üreteci ve gerçek object storage ile ölçüm **yapılmadı**.

## Spec'e göre yorumlar ve sapmalar

1. **`GET /array/<token>/config`** spec'teki uç listesinde yok; posthog-js 1.451.1 her `init`'te ister ve 404'te konsola hata yazar (sonra `/flags`'e düşer). `/flags` ile aynı sabit gövde dönülüyor (`LA_COMPAT_POSTHOG` ile birlikte kapanır).
2. **`allowed_origins` + `Origin` başlığı yok** → kabul (tarayıcı dışı istemci başlığı zaten uydurabilir; liste yalnızca tarayıcı kökenlerini sınırlar).
3. **Token kümesi hiç yüklenemediyse 503** (401 değil). Bilinmeyen `status` değeri o token için `disabled` sayılır.
4. **İşaretsiz gzip:** PostHog uçlarında gövde `1f 8b` ile başlıyorsa başlık/sorgu parametresi olmasa da açılır (posthog-js 1.451.1 böyle gönderiyor; spec yalnızca `Content-Encoding` ve `?compression=gzip-js` biçimlerini sayıyor). **PostHog ham gövde sınırı 2 MiB**, açılmış JSON sınırı 1 MiB (form/base64 taşıma payı). Boş `batch`/dizi: 200, yazılmaz. `compression=lz64` desteklenmez (400).
5. **Replay:** gövdenin ilk baytları bildirilen kodlamanın sihirli sayısıyla (gzip `1f 8b`, zstd `28 b5 2f fd`) uyuşmazsa 400; `n` > 999999 → 400 (anahtar `<seq:06>`); `a` boş ya da > 200 bayt → 400. Depo hatası → 503 ve indeks kaydı yazılmaz.
6. **Çerçeve bozuksa 400, 500'den fazla kayıt olsa bile** (413 yalnızca çerçeve bütünüyle geçerliyse).
7. **`wal_upload_lag_seconds`** = mühürlenmiş ama yüklenmemiş en eski segmentin yaşı (yoksa 0). `tokens_age_seconds` = depoya karşı son başarılı doğrulamadan (200/304) beri geçen süre; hiç yüklenmediyse −1.
8. **`dt/hh`**: çökme sonrası kurtarılan (mühürlenmemiş) segment için kapanış anı kurtarma anıdır.
9. Desteklenmeyen `Content-Encoding` → 400. `LA_ADMIN_LISTEN` tasarımda yok; `/metrics`'i iç ağa ayırmak için eklendi.

## Bilinen eksikler

- PostHog capture v1 (`/i/v1/analytics/events`; posthog-node ≥ 6) ve PostHog session replay ucu yok (tasarımda kapsam dışı).
- Android/iOS PostHog SDK'ları ve posthog-js'in harici betik yükleyen paketi (`/static/*.js`, `config.js`) denenmedi.
- SigV4 imzası AWS belgelerindeki bilinen-yanıt vektörleriyle doğrulandı; **imza doğrulayan gerçek bir S3 uygulamasına karşı koşulmadı** (devs3 imza doğrulamaz). Seçilen sağlayıcıda koşullu yazma (tasarım §19 varsayım 1) ayrıca doğrulanmalı.
- fsync sonrası güç kesintisi dayanıklılığı (donanım/dosya sistemi garantisi) test edilemez; testler süreç çökmesini ve yarım yazılmış kuyruğu simüle eder.
- Hız sınırları node yereldir (tasarım gereği). IP sınırlayıcı en çok ~1 M adres izler; dolunca yeni adresler için sınır uygulanmaz.
- Eşzamanlı gelen aynı `batch_id` iki kez yazılabilir (yukarıda (d)).
- TLS sonlandırma yok (yük dengeleyicide beklenir).
