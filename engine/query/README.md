# la-query — sorgu servisi

`docs/design.md` §11 ve `spec/query.md` sözleşmesinin Rust/DataFusion gerçeklemesi. Diferansiyel testler ve
benchmark'lar `engine/difftest` altındadır.

## Mimari

```
HTTP (axum)  ──►  Service (kimlik, kuyruk, süre sınırı, sonuç önbelleği, metrikler)
                    │
                    ▼
               Snapshot  = gün manifest'leri + kimlik eşlemesi + kişi tablosu + tombstone'lar + şema
                    │        (istek başına dondurulur; sürümleri sonuç önbelleği anahtarına girer)
                    ▼
               Tarama katmanı (scan.rs)
                 dosya eleme:   min_ts/max_ts; person_key =, IN, aralık filtreleri için min_person/max_person
                 dosya başına:  Parquet (sütun + satır grubu/sayfa eleme + satır filtresi, tombstone dahil)
                                → artık filtre → projeksiyon (hesaplanmış sütunlar birleştirmenin ALTINDA)
                 kimlik sürümü eski dosya (design §9.2, 2. aşama):
                   · tek dosyalı gün: bağdan etkilenmeyen kişiler dosya sırasıyla akar; etkilenen kişilerin
                     satırları (yeniden yazılan `a:` satırları + hedef kullanıcının mevcut satırları) ayrı
                     okunur, yeniden yazılır, sıralanır; iki akış yalnızca person_key üzerinden birleştirilir
                   · isteğin person_key'i göremediği sırasız taramada dosya olduğu gibi okunur
                   · çok dosyalı gün: dosya başına "değişmeyen" + "yeniden yazılıp sıralanan" akışlar
                 gün başına:    akış sayısı > 1 ise (person_key, ts, uuid) birleştirme + bitişik eşitleri atma
                                (tek dosyalı günde ek maliyet yok)
                 çıktı:         tek, opak plan düğümü (EventsScanExec); sıralı kipte gün başına bir bölüm ve
                                bildirilmiş sıra, sırasız kipte günler ≤ N bölüme zincirlenir
                    │
        ┌───────────┼─────────────┬───────────────┬──────────────┐
        ▼           ▼             ▼               ▼              ▼
     funnel      retention     trends          events          sql
   (akış, kişi  (akış, kohort  (tek geçiş;     (gün gün        (izin listesi:
    başına       + bit kümesi)  tekil sayım     top-N,          tek SELECT, yalnızca
    O(S×kırılım))               sıradan         keyset          events/persons)
                                yararlanır)     imleç)
```

- **Kişi aralığına bölme:** funnel, retention ve haftalık/aylık tekil sayım, sorgu ≥ `LA_QUERY_PARALLEL_MIN_ROWS`
  satır kapsıyorsa `person_key` uzayını (en büyük dosyanın satır grubu minimumlarından) `LA_QUERY_PARTITIONS`
  aralığa böler; her aralık kendi birleştirilmiş akışını ayrı görevde işler, durumlar toplanır. Günlük/saatlik
  tekil sayımda gün bölümleri zaten bağımsızdır.
- **Dosya önbelleği (`filecache.rs`):** anahtar = `<nesne anahtarı>@<boyut>`; yerel diskte LRU (tüm dosya, arka planda
  doldurulur) + bellekte footer/sayfa indeksi LRU. Soğuk okuma yalnızca gereken aralıkları ister. `file://` ve
  `s3://` depolarının ikisiyle de `Store::object_store()` üzerinden çalışır.
- **Modüller:** `http` · `service` (sınırlar, önbellek, metrik, yavaş sorgu logu) · `state` (kontrol dosyaları, proje
  başına önbellekler, `Snapshot`) · `session` (sorgu başına DataFusion oturumu + bellek havuzu, bölüm sürücüleri) ·
  `scan` (planlayıcı, `DedupExec`, `EventsScanExec`, `events`/`persons` tablo sağlayıcıları) · `filecache` ·
  `filter` (ifade ağacı → DataFusion ifadesi) · `udf` · `json` · `funnel` · `q_funnel` · `q_retention` · `q_trends` ·
  `q_events` · `q_sql` · `q_misc` (schema, replay) · `request` · `intern` · `config` · `error`.

## Yapılandırma (ortam değişkenleri)

Depo değişkenleri `spec/manifest.md` §0 ile aynıdır: `LA_STORE_URL` (`s3://bucket[/önek]` | `file:///dizin`),
`LA_S3_ENDPOINT`, `LA_S3_REGION`, `LA_S3_ACCESS_KEY`, `LA_S3_SECRET_KEY`, `LA_S3_BUCKET`.

| Değişken | Varsayılan | Anlam |
|---|---|---|
| `LA_QUERY_LISTEN` | `0.0.0.0:8081` | Dinlenen adres |
| `LA_QUERY_INTERNAL_SECRET` | (yok) | `X-LA-Internal` sırrı; boşsa iç çağrı yolu kapalıdır |
| `LA_QUERY_CACHE_DIR` | (yok) | Yerel dosya önbelleği dizini; yoksa disk katmanı kapalı (footer önbelleği hep açık) |
| `LA_QUERY_CACHE_BYTES` | 20 GiB | Disk önbelleği üst sınırı (LRU); tek dosya bunun yarısından büyükse önbelleğe alınmaz |
| `LA_QUERY_FOOTER_CACHE_BYTES` | 256 MiB | Bellekteki Parquet footer + sayfa indeksi önbelleği |
| `LA_QUERY_RESULT_CACHE_BYTES` | 64 MiB | Sonuç önbelleği (0 = kapalı); tek girdi en çok ¼'ü |
| `LA_QUERY_MEMORY_BYTES` | 2 GiB | Sorgu başına bellek havuzu (en az 16 MiB) |
| `LA_QUERY_TIMEOUT_MS` | 60000 | Sorgu süre sınırı (kuyrukta bekleme dahil) → `408` |
| `LA_QUERY_SCAN_LIMIT_BYTES` | 0 | Projenin `scan_limit_bytes` değeri yoksa/0 ise uygulanan sınır (0 = sınırsız) → `422` |
| `LA_QUERY_MAX_CONCURRENT` | 4 | `projects.json`'da olmayan proje için eşzamanlı sorgu sınırı |
| `LA_QUERY_QUEUE_MAX` | 16 | Proje başına bekleyebilecek sorgu sayısı; aşılırsa `429` |
| `LA_QUERY_PARTITIONS` | çekirdek sayısı (1–16) | Sorgu başına paralellik |
| `LA_QUERY_PARALLEL_MIN_ROWS` | 1000000 | Sıralı operatörlerin kişi aralığına bölünmeye başladığı satır sayısı |
| `LA_QUERY_PUSHDOWN_FILTERS` | `true` | Filtreleri Parquet çözücüsünün içinde değerlendir (geç somutlaştırma) |
| `LA_QUERY_MANIFEST_TTL_MS` | 10000 | Gün listesi, manifest, şema, kimlik/kişi sürümü önbelleği |
| `LA_QUERY_TOMBSTONE_TTL_MS` | 30000 | Tombstone listesi yenileme aralığı (en çok 60000) |
| `LA_QUERY_CONTROL_REFRESH_MS` | 15000 | `control/keys.json` + `projects.json` ve etkin projelerin tombstone'ları için zamanlayıcı |
| `LA_QUERY_SLOW_MS` | 5000 | Yavaş sorgu logu eşiği |
| `LA_QUERY_SQL_MAX_ROWS` | 10000 | `/q/sql` satır üst sınırı (en çok 10000) |
| `LA_LOG` | `info` | `tracing` filtresi (JSON log) |

Kontrol dosyaları ya da tombstone listesi okunamazsa son iyi kopya kullanılmaya devam eder; servis ilk başarılı
okumaya kadar `/readyz`'de `503` döner.

## Sözleşmenin ötesindeki API notları

- **Bilinmeyen alan → `400`.** Gövdedeki hiçbir alan proje seçemez; yazım hatası da sessizce yok sayılmaz.
- **Hata kodları:** `bad_request` 400 · `unauthorized` 401 · `forbidden` 403 · `not_found` 404 · `timeout` 408 ·
  `scan_limit` 422 · `memory_limit` 422 (bellek havuzu aşıldı) · `queue_full` 429 · `internal` 500.
- **İç başlıklar:** `X-LA-Internal` gönderilmişse istek o yola bağlanır; sır yanlış ya da yapılandırılmamışsa geçerli
  bir `Bearer` olsa bile `401`. `X-LA-Internal` olmadan `X-LA-Project` → `401`.
- **Funnel:** `window_seconds` verilmezse 7 gün. `breakdown_limit` (varsayılan 1000) isteğe bağlıdır; kırılımlar
  adım-1 sayısına göre azalan sıralıdır; 50.000'den fazla farklı kırılım değeri `400`. Medyan, adım başına ≤ 4096
  (kırılım değeri başına ≤ 256) örnekte **tam**, üstünde t-digest'tir.
  `any` kipinde: tek bir event birden çok adımı sağlayabilir; kırılım değeri **çapa** event'inden alınır;
  `median_seconds[k]` = kişinin `k−1` farklı adıma ulaştığı an ile `k` farklı adıma ulaştığı an arasındaki süre.
- **Retention:** `period` varsayılanı `day`, `periods` 8, `return` verilmezse her event. Aralıktaki tüm dönemler
  (boş kohortlar dahil) listelenir.
- **Trends:** kırılım yoksa seri nesnesinde `breakdown` anahtarı bulunmaz (`null` = null kovası). İlk N değer seri
  başına seçilir: `sum` için toplam değere, diğerleri için eşleşen satır sayısına göre; eşitlikte değer sırası.
  `$other`: `count/sum` toplanır, `avg` ağırlıklı ortalamadır, `min/max` birleştirilir, `unique` için üretilmez.
  En çok 20 seri, 10.000 kova.
- **Events:** varsayılan `order` `desc`, `limit` 100. İmleç proje + `from/to/filter/order` ile bağlıdır; başka bir
  sorguda ya da projede `400`.
- **SQL:** `events` fiziksel şemadır (`p_*` sütunları görünür). `prop/prop_num/prop_bool(props, 'k')`, `k` terfi
  edilmişse ve `p_k` sütunu aynı kapsamda görünüyorsa otomatik olarak `coalesce(p_k, …)` biçimine çevrilir (terfi
  sorgu sonucunu değiştirmez). `prop_num`: sayı ya da sayı görünümlü metin; `prop_bool`: yalnızca JSON boolean.
  Özyinelemeli CTE kapalıdır. Satır sınırı aşılırsa yanıtta `"truncated": true` bulunur. Tür eşlemesinde Boolean,
  sözleşmedeki "diğerleri string" kuralına göre `"true"/"false"` döner.
- **Replay:** yabancı önek `403`; kendi öneki altında biçime uymayan anahtar `400`; nesne yoksa `404`.
- **İstek kimliği:** `X-Request-Id` başlığı (harf/rakam/`-_.`, ≤ 64) yanıtta geri döner ve loglara yazılır;
  yoksa servis üretir.
- **Tombstone:** silinen kişiler event sonuçlarının yanı sıra `persons` tablosundan ve `/q/schema` kişi
  özelliklerinden de hemen çıkarılır.
- **Meta:** önbellekten dönen yanıtta `cached: true`, `bytes_scanned: 0`. `bytes_scanned` çözülen sütun parçalarının
  baytıdır (yerel önbellekten okunsa da sayılır; footer'lar sayılmaz).
- **Metrikler (`la_query_` öneki):** `queries_total{type,status}`, `query_seconds{type}`, `bytes_scanned_total`,
  `cache_hit_ratio`, `queue_depth`, `memory_pool_bytes`; ek olarak `cache_hits_total`, `cache_misses_total`,
  `file_cache_{bytes,files,hits_total,misses_total,evictions_total}`, `footer_cache_bytes`, `remote_bytes_total`,
  `auth_failures_total`, `ready`.
- **Yavaş sorgu logu** (`la_query::slow`): proje, tür, süre, taranan bayt, dosya sayısı ve yalnızca operatör
  adlarından oluşan plan özeti. İstek metni loglanmaz (sabitler event içeriği taşıyabilir); yerine SHA-256 özeti yazılır.

## Ölçümler

Bu 4 çekirdekli konteynerde (15 GB RAM, yerel disk üzerinde `file://` depo; ölçüm sırasında makine başka
derlemelerle paylaşılıyordu, ±%10 oynama beklenir) `la-difftest bench` ile ölçüldü. Veri: proje başına **6.000.000
event**, 20 gün, 400.000 kişi (hafif çarpık dağılım), gün başına tek L2 dosyası (300.000 satır, 3 satır grubu),
proje başına ~210 MB Parquet. Her sorgu 5 kez, sıcak önbellekle koşuldu; medyan/min ms. Sonuç önbelleği kapalı,
4 bölüm.

```
la-difftest bench --events 6000000 --days 20 --persons 400000 --dir <dizin> --runs 5 --partitions 4
```

**Varsayım 2 — kişi sıralı düzen funnel'ı hızlandırır mı?** (signup → cart → purchase, 7 gün pencere)

| Sorgu | medyan ms | min ms | taranan MB | not |
|---|---:|---:|---:|---|
| funnel operatörü, 3 adım | 391 | 360 | 55 | sayılar 186037 / 96041 / 29906 |
| funnel operatörü + `platform` kırılımı | 426 | 424 | 56 | |
| funnel operatörü, `strict`, 2 adım (her satırı okur) | 789 | 750 | 55 | |
| düz SQL, tam anlam (kişi üzerinden self-join'ler) | 11171 | 11095 | 166 | aynı sayılar |
| düz SQL, "ilk signup" yaklaşımı (3 toplama + 2 join) | 658 | 653 | 295 | farklı anlam: 186037 / 89167 / 24701 |

Operatör, aynı anlamı veren düz SQL'den ~28 kat, anlamı gevşeten yaygın SQL yaklaşımından ~1,7 kat hızlı ve
3–5 kat daha az bayt okuyor. Varsayım doğrulandı.

**Varsayım 4 — kişi sıralaması trend sorgularını yavaşlatır mı?** (aynı satırlar, bir kopya `ts` sıralı yazıldı)

| Sorgu | kişi sıralı ms | zaman sıralı ms | taranan MB (kişi / zaman) |
|---|---:|---:|---:|
| günlük `purchase` sayısı | 61 | 60 | 38 / 25 |
| günlük tüm event sayısı, `platform` kırılımı | 127 | 121 | 38 / 25 |
| günlük `sum(props.amount)` (`purchase`) | 92 | 89 | 51 / 38 |
| 2 saatlik pencere, saatlik sayım (tek gün) | 13 | 3 | 1,8 / 0,4 |

| Tekil kişi sayımı (kişi sıralı dosyalar) | medyan ms |
|---|---:|
| günlük tekil, akış operatörü (tam sayım) | 120 |
| haftalık tekil, kişi aralıklarına bölünmüş birleştirilmiş akış | 699 |
| günlük tekil, aynı dosyalarda SQL `count(DISTINCT person_key)` (hash) | 542 |

Tüm aralığı kapsayan trendlerde fark gürültü düzeyinde (%0–5); kişi sıralı dosyada `ts` sütunu daha kötü
sıkıştığı için ~%50 daha fazla bayt okunuyor. Dar zaman penceresinde satır grubu `ts` ile elenemediğinden ~4 kat
yavaş, ama mutlak değer küçük (13 ms/gün-dosyası). Karşılığında tekil sayım 4,5 kat hızlanıyor. Varsayım bu ölçekte
doğrulandı; ön toplam gerekmedi. (Gün başına çok büyük dosyalarda dar pencere maliyeti doğrusal büyür — izlenmeli.)

**Varsayım 3 — sorgu anı kimlik yeniden yazımı ucuz mu?** Kişilerin %5'i / %20'si aralığın ortasında identify
oluyor; bağlar **tüm** dosyalar yazıldıktan sonra geliyor (her dosya "eski": en kötü durum; compaction'dan geçmiş
günlerde yalnızca sonradan gelen bağlar bu yola girer).

| Sorgu | bekleyen bağ yok | kişilerin %5'i (satırların %2,5'i) | kişilerin %20'si (satırların %10'u) |
|---|---:|---:|---:|
| funnel, 3 adım | 403 ms | 726 ms (1,8×) | 1538 ms (3,8×) |
| günlük tekil kişi | 117 ms | 534 ms (4,6×) | 1370 ms (11,7×) |
| günlük sayım (kişiden bağımsız) | 56 ms | 55 ms | 62 ms |
| taranan MB (funnel) | 55 | 179 | 179 |

Kişiye bakmayan sorgular hiç etkilenmiyor (tek dosyalı günde yeniden yazım atlanır). Kişi sıralı operatörlerde ise
maliyet **"azınlık satır" varsayımının öngördüğünden yüksek**: etkilenen kişilerin satırları dosyaya dağınık olduğundan
`ts`/`uuid` sayfalarının neredeyse tamamı açılıyor (3 kat bayt) ve tüm satırlar gün içi iki yollu birleştirmeden
geçiyor. Varsayım 3 bu gerçeklemeyle yalnızca kısmen doğrulandı. Bilinen iyileştirmeler (yapılmadı):
(1) "eski dosya" kararı şu an yalnızca sürüm karşılaştırması — dosyada gerçekten yeni bağlanan `anon_id` var mı
diye bakıp olmayan dosyalarda bu yolu tamamen atlamak; (2) iki akışı satır biçimli k-yollu birleştirme yerine
parti dilimleyen bir "araya ekleme" operatörüyle birleştirmek.

**Diferansiyel testler:** `cargo test` içinde 40 tohum (≈ 36 s); `la-difftest --seeds 400 --start 1000` (release)
uyumsuzluk olmadan geçti: 9600 funnel, 2400 retention, 3200 trends, 2000 events yürüyüşü, 1600 tarama denetimi
(130.666 mantıksal event, 13.193 dosya, 17.166 yinelenen kopya, 7.604 kimlik bağı; 1264 s). Tohum başına ~300 mantıksal event, ~30 dosya; funnel (3 kip × kırılımlı/kırılımsız), retention,
trends (tekil dahil), events sayfalama ve SQL tarama eşitliği düz DataFusion SQL referanslarıyla karşılaştırılır.
Karşılaştırmanın dişli olduğu, servise 14 kasıtlı hata enjekte edilerek sınandı (pencere sınırı, aynı satırın iki
adım sayılması, dedupe/kimlik yeniden yazımı/sıralama kapatma, tombstone, `ne`, terfi sütunu, imleç eşitliği…):
14'ü de yakalandı.

## Çalıştırma ve test

```
# servis
LA_STORE_URL=file:///var/lib/la/store LA_QUERY_CACHE_DIR=/var/cache/la-query la-query

# testler (yalıtılmış hedef dizini; disk darsa debug bilgisi kapatılabilir)
CARGO_TARGET_DIR=engine/target-query cargo test  -p la-query -p la-difftest
CARGO_TARGET_DIR=engine/target-query cargo clippy -p la-query -p la-difftest --all-targets -- -D warnings

# uzun diferansiyel koşu, elle deneme verisi, benchmark
la-difftest --seeds 400 --start 1000            # --store-env: LA_STORE_URL'deki depoya karşı (ör. S3)
la-difftest gen --dir /tmp/la-demo --seed 7     # anahtar: sk_demo
la-difftest bench --dir /tmp/la-bench
```

Test dosyaları: `tests/scan.rs` (şema evrimi, kimlik yeniden yazımı + sıra, dedupe, tombstone, dosya eleme, disk
önbelleği, sıralama bildirimi), `tests/api.rs` (her ucun biçimi ve hata kodları, sayfalama, sonuç önbelleği,
sınırlar), `tests/isolation.rs` (design §17.6: SQL, filtre ağacı, `from/to`, imleç, replay anahtarı, iç başlıklar),
`engine/difftest/tests/diff.rs`.

## Bilinen eksikler

- `/q/paths` (v1.1) ve trend ön toplam dosyaları (v1.1) yok.
- Kimlik yeniden yazımı kişi sıralı sorgularda pahalı (yukarıdaki ölçüm) ve "eski dosya" kararı yalnızca sürüme
  bakıyor: canlı bir projede son bağdan eski her dosya bu yola girer.
- Sonuç önbelleği ve eşzamanlılık sınırı süreç içidir; birden çok replika arasında paylaşılmaz.
- Kişi tablosu sürüm değişince baştan yüklenir (artımlı değil); çok büyük `persons/` için pahalıdır.
- Aynı adla **aynı boyutta** ama farklı içerikle yeniden yazılmış bir L0 dosyası (plan yeniden yürütme) önbellekteki
  footer'la çelişebilir; boyut değişmişse okuyucu `HEAD` ile toparlar.
- Manifest'in `tombstone_ts` değeri "tüm dosyalara uygulandı" anlamına gelir: sıkıştırılmış bir güne süzülmemiş L0
  ekleyen yazar bu değeri düşürmezse o dosyadaki silinmiş kişi satırları sorguda görünür (yazar tarafının sözleşmesi).
- Tombstone'lanan kişinin replay nesneleri compactor silene kadar `/q/replay` ile okunabilir (event'leri anında gizlenir).
- `uuid` tekrarlarının `recv_ts` gibi alanları farklıysa hangisinin kaldığı belirlenmiş değildir.
- Bir alt sorgu `p_k` sütununu taşımıyorsa dış sorgudaki `prop(props,'k')` terfi edilmiş değeri göremez.
- Gövde 1 MiB'ı aşarsa yanıt axum'un düz `413`'üdür (JSON hata zarfı değil).
- S3 yolu yalnızca depodaki geliştirme sunucusuna (`golib/devs3`, `s3://bucket/önek`) karşı doğrulandı: 4 tohumluk
  diferansiyel koşu (`--store-env`) ve HTTP üzerinden sorgu + disk önbelleği. Gerçek AWS S3 / MinIO'ya karşı koşulmadı.
