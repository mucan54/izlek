# Durum: ne yapıldı, ne doğrulandı, ne doğrulanmadı

Tarih: 2026-11-18 · Kapsam: `docs/design.md` v0.1'in tamamı (Faz 0–7).

Bu belge tasarımın "production'a hazır olma listesi"ni (§21) ve "ölçülecek varsayımlar"ını (§19) kodun bugünkü hâliyle eşler. **Buradaki her ölçüm tek bir paylaşımlı geliştirme konteynerinde (4 vCPU, yerel disk, yük üreteci aynı makinede) yapılmıştır; kapasite iddiası değildir.**

## 1. Özet

- Tasarımdaki bütün bileşenler yazıldı: ingestion, shipper, compactor, sorgu servisi, control plane, UI, Web SDK, KMP SDK, kanarya, PostHog adaptörü, flag'ler, session replay.
- Tek makinede gerçek binary'lerle uçtan uca çalışıyor; bu, `file://` depoda ve S3 protokolü üzerinden (depodaki geliştirme S3 sunucusu) otomatik testle gösterildi (`e2e/`).
- **Production'a hazır değil.** Eksikler aşağıda; en önemlileri: bulut sağlayıcısının kendi S3'ü (yalnızca MinIO denendi), çok node, gerçek mobil cihazlar, uzun süreli yük ve kanarya, tatbikatların çoğu, dış güvenlik incelemesi.

## 2. Otomatik testler (bu depoda, son koşu)

| Paket | Komut | Sonuç |
|---|---|---|
| Vektörler | `python3 spec/vectors/gen.py --check` | Üreteçle aynı (wire 30, saat, murmur3, flags 117, replay) |
| Go (ingest, control, canary, golib) | `go test ./...` | 371 test koşusu (alt testler dahil), hata yok; `-race` ile de koşuldu |
| PostHog SDK uyumu | `go test -tags compat ./ingest/compat/` | posthog-node 5.74.0 ve posthog-js 1.451.1 geçiyor; posthog-node 6.x **bilinen uyumsuz** |
| Rust: codec, core, shipper, compactor | `cargo test` | 151 test |
| Rust: query + diferansiyel | `cargo test -p la-query -p la-difftest` | 70 test (40 tohumluk diferansiyel paket dahil) |
| Web SDK | `npm test` | 74 birim + Chromium'da 13 adım; çekirdek 3 751 B gzip (bütçe 4 096) |
| UI | `npm test` | 75 birim + sahte API'ye karşı Chromium'da 24 adım |
| KMP SDK | `./gradlew jvmTest` | 108 test (yalnızca JVM hedefi) |
| Uçtan uca | `node e2e/run.mjs`, `--s3` | 16 adım, iki kipte de geçiyor |

Tek seferlik uzun koşular (CI'da değil): 400 tohumluk diferansiyel koşu (9 600 funnel, 2 400 retention, 3 200 trend karşılaştırması) hatasız; sorgu servisine bilerek eklenen 14 hatanın 14'ü diferansiyel paket tarafından yakalandı; her çözücü için ~30 sn'lik fuzz koşuları.

CI: `.github/workflows/ci.yml` yukarıdaki adımların hepsini tanımlar; ayrıca depo sözleşmesini ve uçtan uca testi imza doğrulayan gerçek bir S3 uygulamasına (MinIO) karşı koşar. İş akışı GitHub Actions'ta `afdff48` commit'inde baştan sona yeşil koştu (6 iş, yaklaşık 47 dakika; önbellek dolunca kısalması beklenir). Güncel sonuç için deponun Actions sekmesine bakın.

## 3. Ölçümler

| Ölçüm | Değer | Kaynak |
|---|---|---|
| Ingestion doyma noktası | ≈ 35 500 istek/sn ≈ 710 bin event/sn (20 event'lik batch, gzip'siz) | `ingest/README.md` |
| Ingestion onay gecikmesi | p50 3,3 ms; p99 < 50 ms ≈ 23 bin istek/sn'ye kadar | 〃 |
| WAL bayt/event (`w`, zstd) | ≈ 31 | `engine/shipper/README.md` |
| Parquet bayt/event (`b`, L2) | ≈ 23 | 〃 |
| Shipper verimi | ≈ 100 bin event/sn, tek iş parçacığı; bellek ≈ 1,5 KB/event × plan boyutu | 〃 |
| Compaction | 5 M satırlık gün 9,7 sn; tepe bellek 362 MB | 〃 |
| Funnel (6 M event, 3 adım, sıcak önbellek) | operatör 391 ms; aynı sonucu veren düz SQL 11 171 ms | `engine/query/README.md` |
| Trend (günlük sayım, 6 M event) | kişi sıralı 61 ms; zaman sıralı kopya 60 ms | 〃 |
| Kimlik yeniden yazımı (tüm dosyalar bayat) | funnel 403 → 726 ms (%5 kişi) → 1 538 ms (%20 kişi) | 〃 |

Depo kesintisi tatbikatı (`node e2e/outage.mjs --seconds 600`, 50 event/sn, tek ingestion node'u):

| Kip | Kesintide onaylanan event | Reddedilen istek | Onay p99 (önce → kesintide) | Yerel WAL tepe | Kesintide sorgular | Yetişme süresi | Sonuç |
|---|---:|---:|---|---:|---|---:|---|
| Depo `503` dönüyor | 28 960 | 0 | 9,7 → 8,3 ms | 4,9 MB | hepsi `500` | 15,2 sn | 29 675 / 29 675 görünür, çift yok |
| Depo yanıt vermiyor | 29 920 | 0 | 9,8 → 8,0 ms | 4,9 MB | hepsi istemci zaman aşımı (30 sn) | 7,1 sn | 30 635 / 30 635 görünür, çift yok |

Çok sayıda küçük proje (`node e2e/many-projects.mjs --projects 10000`; proje başına iki turda 5'er event, release shipper, depo olarak istek sayan geliştirme S3 sunucusu):

| Adım | Süre | Tepe bellek | `LIST` | `GET` | `PUT` |
|---|---:|---:|---:|---:|---:|
| Shipper, 10 000 projeyi tek planda yazıyor (1. tur) | 32 sn | 118 MB | 50 053 | 20 004 | 30 003 |
| Shipper, 2. tur | 38 sn | 153 MB | 50 053 | 40 004 | 30 002 |
| Shipper, yeni veri yokken bir tur | < 0,1 sn | — | 52 | 2 | 0 |
| Compactor, her projede iş varken tam tur | 89 sn | 30 MB | 130 020 | 60 002 | 20 000 |
| Compactor, iş yokken tam tur (saatte bir) | 24 sn | 15 MB | 100 020 | 20 002 | 0 |
| Compactor, iş yokken sıcak tur (dakikada bir) — **önce** | ≤ 8 sn | — | 30 014 | 10 002 | 0 |
| Compactor, iş yokken sıcak tur — **dokunulan projeler kaydıyla** (2 000 projede ölçüldü; proje sayısından bağımsız) | < 1 sn | — | 1 | 2 | 0 |
| Tek projede bir sorgu | 7–21 ms | — | 5 | 4 | 0 |

Nesne sayısı: iki turdan sonra proje başına 5 (2 veri dosyası, 2 manifest sürümü, şema), compaction sonrası GC'ye kadar 7. Beş event'lik bir Parquet dosyası ≈ 6,9 KB'tır (dosya başı sabit yük). Shipper ve compactor projeleri sırayla işler; süreler bu yüzden proje sayısıyla doğrusal büyür ve geliştirme sunucusunun hızına bağlıdır. İstek sayıları ise depodan bağımsızdır.

Veri sentetiktir (küçük event'ler); gerçek veride `b` ve `w` farklı çıkar. Hiçbir ölçüm gerçek object storage ya da ayrı makinedeki yük üreteciyle yapılmadı.

## 4. Tasarım varsayımları (§19)

| # | Varsayım | Durum |
|---|---|---|
| 1 | Object storage koşullu yazmayı doğru uygular | **MinIO için doğrulandı** (CI'da, 2025-07-23 sürümü): 24 eşzamanlı "yoksa oluştur" denemesinden her turda tam biri kazanıyor, 12 eşzamanlı manifest commit'inin hepsi ayrı sürüm oluyor; Go ve Rust istemcileri, SigV4 imzası doğrulanarak. **Başka sağlayıcıda doğrulanmadı**: seçilen sağlayıcıya karşı aynı testler koşulmalı (`LA_TEST_S3_ENDPOINT`, bkz. `engine/core/tests/s3_real.rs`, `golib/objstore`) |
| 2 | Kişi sıralı düzen funnel'ı hızlandırır | Bu ölçekte doğrulandı (≈ 28×, tam aynı anlam için) |
| 3 | Sorgu anı kimlik yeniden yazımı ucuzdur | **Kısmen.** Kişiden bağımsız sorgular etkilenmiyor; kişi sıralı operatörler tüm dosyalar bayatken 1,8–11,7× yavaşlıyor. Compaction bayatlığı giderir; iki bilinen iyileştirme yapılmadı (`engine/query/README.md`) |
| 4 | Kişi sıralaması trendleri kabul edilemez yavaşlatmaz | Bu ölçekte doğrulandı (fark ölçüm gürültüsü içinde; yalnızca çok dar zaman aralığında 13 ms ↔ 3 ms) |
| 5 | Ingestion ≥ 100 bin event/sn | Bu makinede aşıldı; ayrı yük üreteci ve gerçek depo ile tekrarlanmalı |
| 6 | 10 sn'lik yerel WAL penceresi kabul edilebilir | Risk kararı; karar verilmedi |
| 7 | Compose semantics ağacı replay için yeterli | **Doğrulanmadı.** `analytics-replay-compose` yazıldı, cihazda çalıştırılmadı |
| 8 | Arka plan gönderimi güvenilir (Android/iOS) | **Doğrulanmadı.** Cihaz testi yok |
| 9 | Özellik terfisi şema patlamasına yol açmaz | Sınır (200 sütun) uygulanıyor; gerçek veride izlenmedi |
| 10 | PostHog SDK'ları adaptörü kabul eder | Web ve Node (5.x) için doğrulandı; Node 6.x uyumsuz; Android/iOS denenmedi |
| 11 | 10 000 küçük proje yönetilebilir | **Büyük ölçüde.** İzolasyon ve sayımlar doğru, shipper ve compactor belleği düşük (aşağıdaki ölçüm). İlk ölçüm compactor'ın dakikalık turunun proje başına ≈ 3 `LIST` attığını gösterdi (10 000 projede günde ≈ 43 M `LIST`); bunun üzerine shipper dokunduğu projeleri kaydetmeye, compactor da dakikalık turda yalnızca onlara bakmaya başladı: boşta tur artık shard başına 1 `LIST`. Kalan maliyet saatlik tam turdur (proje başına ≈ 10 `LIST`, 10 000 projede günde ≈ 2,4 M) ve aktif projelerde plan başına ≈ 5 `LIST`; ikisi de azaltılabilir, yapılmadı |

## 5. Production'a hazır olma listesi (§21)

**Doğruluk**
- [x] Wire vektörleri Go, Rust, TypeScript ve Kotlin'de geçiyor
- [x] Diferansiyel testler var ve CI'da koşuyor
- [x] Shipper ve compaction idempotency testleri (plan yürütmenin her adımında çökme enjeksiyonu)
- [ ] Kanarya 14 gün kesintisiz — kanarya yazıldı; uzun süreli koşu yok

**Dayanıklılık**
- [~] Hata enjeksiyonu (§17.5): ingestion fsync↔yanıt arası çökme ✔ (entegrasyon testi), shipper adım adım çökme ✔, eşzamanlı manifest yazımı ✔, 10 dakikalık depo kesintisi ✔ (aşağıda), SDK süreç ölümü/kuyruk bozulması ✔ (JVM ve tarayıcıda). **Eksik:** SDK'da disk dolu ve saat atlaması cihaz üzerinde
- [~] Object storage kesinti tatbikatı — `e2e/outage.mjs` ile, **geliştirme S3 sunucusunun hata anahtarına karşı** iki kipte 10'ar dakika (§3'teki tablo). Gerçek sağlayıcının hata biçimleri (kısmi hatalar, yavaşlama, DNS) denenmedi
- [ ] `data/` yeniden inşa tatbikatı (komut ve testleri var; gerçek hacimde süre ölçülmedi)
- [ ] Control DB geri yükleme tatbikatı (`backup` komutu ve testi var)

**Performans**
- [~] Ingestion doyma noktası ve p99 belgeli — tek makine, paylaşımlı
- [ ] Hedef hacmin 3 katında 24 saatlik test
- [~] Standart sorgu seti: sıcak süreler ölçüldü; soğuk (uzak depodan) süreler ölçülmedi
- [ ] Kapasite planı — `b` ve `w` ölçüldü, plan yazılmadı

**Güvenlik ve gizlilik**
- [x] İzolasyon testi (proje sınırı; SQL izin listesi; replay önek denetimi) CI tanımında
- [~] Fuzz hedefleri var ve kısa koşuldu; sürekli fuzz altyapısı yok
- [ ] Bileşen bazında en az yetkili depo anahtarları — belgelenmedi, denenmedi
- [x] Silme akışı uçtan uca: tombstone → sorguda anında gizleme → yeniden yazım → replay nesneleri (e2e + compactor testleri). WAL kopyaları saklama süresi dolana dek durur (tasarım gereği)
- [ ] Bağımlılık taraması, imaj imzalama
- [ ] Dış güvenlik incelemesi

**Operasyon**
- [~] Alarm kuralları (`deploy/prometheus/alerts.yml`) gerçek metrik adlarıyla eşlendi; pano yok; kurallar bir Prometheus'ta yüklenip denenmedi
- [~] Runbook'lar yazıldı (`docs/runbooks/`), tatbik edilmedi
- [ ] Yuvarlanan güncelleme
- [ ] Format sürüm geçişi (N−1 okuma)
- [ ] Nöbet süreci

**SDK**
- [ ] Android/iOS yayını ve ikili boyut — yalnızca JVM hedefi derlenip test edildi; Android ve iOS hedefleri bu ortamda derlenemedi/çalıştırılamadı; 60 KB bütçesi ölçülmedi
- [~] Kuyruk bozulması testleri var (JVM, tarayıcı); disk dolu cihazda denenmedi
- [ ] iOS privacy manifest, Android veri güvenliği beyanı
- [~] Entegrasyon belgeleri ve örnekler var; UI'daki SDK kurulum parçacıklarındaki paket koordinatları yayınlanmış bir pakete karşılık gelmiyor

`[x]` yapıldı ve burada doğrulandı · `[~]` kısmen · `[ ]` yapılmadı.

## 6. Bilinen kusurlar ve sınırlar

1. **Farklı `ts`'li kopyalar** (`spec/query.md` §10): sorgu artık compaction'la aynı kuralı uygular (kişi başına `uuid`), dolayısıyla sayımlar compaction öncesi ve sonrası aynıdır. Kalan sınır durumu: saati kaymış bir cihazın yeniden gönderdiği event'in iki kopyası bir zaman filtresinin ya da `/q/events` imlecinin iki yanına düşerse, compaction'a kadar geç kopya ayrıca görünür.
2. **Kimlik yeniden yazımı pahalı** (yukarıda #3): çok `identify` alan canlı projelerde funnel/tekil sayım, compaction arayı kapatana dek yavaşlar.
3. **Kapatılan projenin WAL kopyaları** WAL saklama süresi (varsayılan 30 gün) dolana dek depoda kalır; diğer her şey ilk compactor turunda silinir.
4. **Tombstone'lu kişinin replay nesneleri** compactor silene dek depoda durur (listelenemez, ama anahtarı bilen proje anahtarı sahibi indirebilir).
5. **posthog-node ≥ 6** (capture v1 ucu) desteklenmiyor.
6. **Sorgu servisi replikaları** sonuç önbelleğini ve eşzamanlılık sınırını paylaşmaz (süreç başına).
7. **UI** flag `regex` değerini tarayıcının RegExp'iyle denetler; sunucu RE2 alt kümesini ister — UI'da geçerli görünen bir kalıbı API reddedebilir.
8. **Flag `str()`**: çok büyük/küçük ondalık sayıların üstel gösterimi diller arasında farklı olabilir; hiçbir vektör bunu kapsamıyor. Flag koşullarında böyle değerlerle `eq`/`contains` kullanmayın.
9. **Shipper belleği** plan boyutuyla orantılıdır (`LA_SHIPPER_BATCH_MB`, varsayılan 64).
10. **OIDC yok**; e-posta gönderimi yok (ilk parolayı yönetici verir).
11. **Docker imajları ve Compose dosyası** yalnızca CI'da (`deploy/compose/smoke.sh`: derle, kur, event gönder, sorguda gör, kanarya, temiz kapanış) doğrulandı; tek makine, dosya deposu. TLS vekili, S3 ve çok makineli kurulumla denenmedi.
12. `/q/paths` ve ön toplamalar (v1.1) yok.

## 6b. Tasarımda olmayıp eklenenler

- **MCP sunucusu ve OAuth yetkilendirme sunucusu** (`docs/mcp.md`): control plane `/mcp` ucunda 22 araç sunar (kurulum, flag, analitik). Kimlik: `admin` kapsamlı gizli anahtar ya da control plane'in kendi verdiği OAuth 2.1 token'ları. Doğrulama: Go testleri (yarış denetimiyle), resmî MCP istemci kitaplıklarıyla uyum testi (iki protokol kuşağı, tam OAuth akışı, Chromium'da onay sayfası) ve uçtan uca testte gerçek sorgu servisine karşı araç çağrıları. **Barındırılan claude.ai ve ChatGPT servislerine karşı hiç denenmedi.** Yeni bir saldırı yüzeyidir; dış güvenlik incelemesi kapsamına girmelidir.
- **Panel dili**: İngilizce ve Türkçe; API hata mesajları henüz yalnızca Türkçe.
- **Coolify compose dosyası** (`docker-compose.coolify.yml`): CI'da düz Docker Compose ile derlenip çalıştırıldı ve geçti (yığın kendini kuruyor, tüm konteynerler sağlıklı, üretilen parolayla giriş, ikinci deploy'da yeni anahtar üretilmiyor). **Gerçek bir Coolify kurulumunda denenmedi**: alan adı yönlendirmesi (`SERVICE_URL_*`) ve `exclude_from_hc` yalnızca Coolify belgelerine dayanıyor.
- **MinIO'da gözlenen bir durum**: 24 eşzamanlı koşullu yazmadan biri bir CI koşusunda bağlantı düzeyinde koptu (`EOF`). Kazanan yine tekti. İstemci için bu "sonuç bilinmiyor" demektir; manifest ve plan protokolleri yeniden denemede aynı sonuca vardığı için güvenlidir, test de artık bu durumu ayrı sayar ama asla iki kazanan kabul etmez.

## 7. Tasarıma eklenenler

Uygulama sırasında tasarımın açık bıraktığı yerlerde alınan kararlar ilgili spec dosyalarında "Netleştirmeler" başlığı altındadır. Davranışı değiştiren başlıcaları:

- Shipper **plan protokolü** (`spec/manifest.md` §6) ve yalnızca "yoksa oluştur" kullanan **lease zinciri** (§7).
- Kapatılan proje için `closed` bayrağı (`spec/manifest.md` §8) — `retention_days = 0` "sonsuza kadar sakla" demektir.
- Ingestion'ın `GET /flags/<token>.json` ucu (`spec/flags.md` §1) — tasarım yalnızca CDN'i sayıyordu.
- PostHog `$identify` için kullanıcının kendi kimliğine öz-bağ (anonim kimliği olmayan çağrılarda da kişi anahtarı tutarlı kalsın diye).
- Karantina dizini (bozuk WAL segmentleri), `rebuild --abort`, tombstone uygulama durumu dosyası.
