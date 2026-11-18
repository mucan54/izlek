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
                 dosya başına:  Parquet (sütun + satır grubu/sayfa eleme + satır filtresi, tombstone dahil)
                                → artık filtre → projeksiyon (hesaplanmış sütunlar birleştirmenin ALTINDA)
                 kimlik sürümü eski dosya:  aynı dosya iki ayrık koşulla okunur —
                                anahtarı değişmeyen satırlar sırayla akar; bağı sonradan gelen `a:` satırları
                                yeniden yazılır, yeniden filtrelenir, ayrıca sıralanır
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
| `LA_QUERY_LISTEN` | `0.0.0.0:8090` | Dinlenen adres |
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
- **Meta:** önbellekten dönen yanıtta `cached: true`, `bytes_scanned: 0`. `bytes_scanned` çözülen sütun parçalarının
  baytıdır (yerel önbellekten okunsa da sayılır; footer'lar sayılmaz).
- **Metrikler (`la_query_` öneki):** `queries_total{type,status}`, `query_seconds{type}`, `bytes_scanned_total`,
  `cache_hit_ratio`, `queue_depth`, `memory_pool_bytes`; ek olarak `cache_hits_total`, `cache_misses_total`,
  `file_cache_{bytes,files,hits_total,misses_total,evictions_total}`, `footer_cache_bytes`, `remote_bytes_total`,
  `auth_failures_total`, `ready`.
- **Yavaş sorgu logu** (`la_query::slow`): proje, tür, süre, taranan bayt, dosya sayısı ve yalnızca operatör
  adlarından oluşan plan özeti. İstek metni loglanmaz (sabitler event içeriği taşıyabilir); yerine SHA-256 özeti yazılır.

## Ölçümler

BENCH_PLACEHOLDER

## Bilinen eksikler

- `/q/paths` (v1.1) ve trend ön toplam dosyaları (v1.1) yok.
- Sonuç önbelleği ve eşzamanlılık sınırı süreç içidir; birden çok replika arasında paylaşılmaz.
- Kişi tablosu sürüm değişince baştan yüklenir (artımlı değil); çok büyük `persons/` için pahalıdır.
- Aynı adla **aynı boyutta** ama farklı içerikle yeniden yazılmış bir L0 dosyası (plan yeniden yürütme) önbellekteki
  footer'la çelişebilir; boyut değişmişse okuyucu `HEAD` ile toparlar.
- Tombstone'lanan kişinin replay nesneleri compactor silene kadar `/q/replay` ile okunabilir (event'leri anında gizlenir).
- `uuid` tekrarlarının `recv_ts` gibi alanları farklıysa hangisinin kaldığı belirlenmiş değildir.
- Bir alt sorgu `p_k` sütununu taşımıyorsa dış sorgudaki `prop(props,'k')` terfi edilmiş değeri göremez.
- S3 yolu yalnızca `object_store` soyutlaması üzerinden doğrulandı (bellek içi ve `file://` depolarla test edildi);
  gerçek bir S3 uç noktasına karşı koşulmadı.
