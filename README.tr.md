# izlek

> Bu belge deponun Türkçe özetidir. Güncel ve ayrıntılı sürüm: [README.md](README.md) (İngilizce).

Kendi sunucunuzda çalışan hafif ürün analitiği platformu: event toplama, kimlik birleştirme, trend / funnel / retention sorguları, client-side feature flag'ler ve wireframe tabanlı session replay. Tek kalıcı bağımlılığı S3 uyumlu bir object storage (ya da tek makinede bir dizin) olan, veritabanı kümesi ve kuyruk gerektirmeyen bir mimaridir.

Tasarım belgesi: [`docs/design.md`](docs/design.md) · Sözleşmeler: [`spec/`](spec/) · **Neyin doğrulandığı, neyin doğrulanmadığı: [`docs/status.md`](docs/status.md)**

> **Durum:** v0.1. Bütün bileşenler yazıldı ve tek makinede gerçek binary'lerle uçtan uca çalışıyor. Production'a hazır **değildir**: bir bulut sağlayıcısının S3'ünde (CI yalnızca MinIO'yu dener), çok node'lu kurulumda, gerçek Android/iOS cihazlarında ve uzun süreli yük altında denenmedi. Ayrıntı `docs/status.md` içindedir.

## Mimari

```
SDK (KMP · Web · PostHog uyumlu istemciler)
        │  LA1 ikili batch / PostHog JSON
        ▼
  la-ingest (Go) ──fsync──▶ yerel WAL ──zstd──▶ object storage  wal/
                                                      │
                                   la-shipper ship (Rust): WAL → sıralı Parquet (L0), kimlik ve kişi tabloları
                                   la-shipper compact:     L0 → L1 → L2, saklama süresi, silme (tombstone), GC
                                                      │
                                                      ▼  data/  identity/  persons/  replay/
  la-query (Rust, DataFusion): trends · funnel · retention · events · SQL · replay
        ▲
  la-control (Go): kullanıcı/proje/token/flag yönetimi, anlık görüntü yayını, sorgu vekili, UI (Preact SPA)
```

- Servisler birbirine bağlanmaz; her şey object storage üzerinden akar. Eşgüdüm yalnızca "yoksa oluştur" (`PUT If-None-Match: *`) ile yapılır: sürümlü manifest'ler, shipper planları, lease zinciri.
- `200` yalnızca event diske `fsync` edildikten sonra döner. Shipper planları deterministik dosya adlarıyla yeniden yürütülebilir; aynı event sorguda bir kez sayılır.
- Flag'ler SDK'da yerel değerlendirilir; control plane yalnızca kural setini yayınlar.

## Depo düzeni

| Dizin | İçerik | Dil |
|---|---|---|
| [`spec/`](spec/) | Wire, WAL, manifest, Parquet, flag, replay, sorgu ve control API sözleşmeleri; ortak test vektörleri (`spec/vectors/`, bağımsız Python üreteci) | — |
| [`ingest/`](ingest/README.md) | Ingestion servisi, PostHog adaptörü, yük üreteci | Go |
| [`engine/`](engine/) | `codec`, `core`, [`shipper`](engine/shipper/README.md) + `compactor`, [`query`](engine/query/README.md), `difftest` (diferansiyel test) | Rust |
| [`control/`](control/README.md) | Control plane (SQLite), yayıncı, sorgu vekili | Go |
| [`ui/`](ui/README.md) | Yönetim arayüzü: insight'lar, panolar, flag'ler, kişiler, replay oynatıcı | TypeScript (Preact) |
| [`sdk-web/`](sdk-web/README.md) | Web SDK (çekirdek 3,7 KB gzip), flags, replay | TypeScript |
| [`sdk-kmp/`](sdk-kmp/README.md) | Kotlin Multiplatform SDK: core, flags, replay, örnekler | Kotlin |
| [`canary/`](canary/README.md) | Sentetik uçtan uca yoklayıcı (kayıp/çift/tazelik) | Go |
| [`e2e/`](e2e/README.md) | Gerçek binary'lerle uçtan uca test | Node |
| [`deploy/`](deploy/) | Yerel başlatıcı, Docker/Compose taslakları, Prometheus alarm kuralları | — |
| [`docs/runbooks/`](docs/runbooks/README.md) | Alarm başına operasyon adımları | — |

## Hızlı başlangıç (tek makine)

Gerekenler: Go 1.24+, Rust (sürüm `engine/rust-toolchain.toml` içinde sabit), Node 22.

```sh
deploy/local/run.sh
```

Betik her şeyi derler, ilk kullanıcıyı/projeyi oluşturur (bilgiler `.local/bootstrap.txt` içine yazılır) ve beş süreci başlatır: ingestion `:8080`, sorgu `:8081`, control plane + UI `:8090`, shipper, compactor. Ardından:

```html
<script src="izlek.min.js"></script>
<script>
  izlek.init({ token: 'pk_…', host: 'http://localhost:8080' })
  izlek.capture('sepete_eklendi', { urun_id: 42, fiyat: 129.9 })
</script>
```

ya da herhangi bir HTTP istemcisiyle (PostHog uyumlu uç):

```sh
curl -H 'Content-Type: application/json' http://localhost:8080/batch/ \
  -d '{"api_key":"pk_…","batch":[{"event":"merhaba","distinct_id":"kullanici-1"}]}'

curl -H 'Authorization: Bearer sk_…' -H 'Content-Type: application/json' http://localhost:8081/q/sql \
  -d '{"from":"2026-11-01","to":"2026-11-30","sql":"SELECT event, count(*) FROM events GROUP BY 1"}'
```

Event'ler varsayılan ayarlarla yaklaşık 15–30 sn içinde sorguda görünür (segment rotasyonu 10 sn + shipper turu).

Dağıtım: `deploy/compose/` Docker Compose dosyasını, kökteki `docker-compose.coolify.yml` ise Coolify'a tek kaynak olarak kurulabilen sürümünü içerir (rehber: `deploy/coolify/README.md`; ikisi de CI'da düz Docker Compose ile deneniyor, gerçek bir Coolify kurulumunda denenmedi); çok makineli kurulumda tüm servislere aynı `LA_STORE_URL=s3://…` verilir. Ortam değişkenleri her bileşenin README'sindedir.

## Testler

```sh
python3 spec/vectors/gen.py --check                # vektörler üreteçle aynı mı
go vet ./... && go test -race ./...                # ingest, control, canary, golib
(cd engine && cargo test --workspace)              # codec, core, shipper, compactor, query, diferansiyel
(cd sdk-web && npm ci && npm test)                 # birim + gerçek Chromium + boyut bütçesi
(cd ui && npm ci && npm test)                      # birim + sahte API'ye karşı tarayıcı testi
(cd sdk-kmp && ./gradlew jvmTest)                  # KMP ortak kodu (JVM hedefi)
node e2e/run.mjs && node e2e/run.mjs --s3          # gerçek binary'lerle tüm hat
```

Aynı adımlar `.github/workflows/ci.yml` içinde tanımlıdır.

## Lisans

Henüz belirlenmedi.
