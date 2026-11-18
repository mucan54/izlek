# la-shipper

WAL segmentlerini (`wal/`) kişi sıralı Parquet dosyalarına (`data/`) çeviren shipper, bu dosyaları
birleştiren compactor ve `data/`'yı WAL'dan yeniden üreten rebuild aracı. Tek binary, üç mod.
Normatif kaynaklar: `spec/wal.md`, `spec/manifest.md`, `spec/parquet.md`, `spec/wire.md`;
tasarım: `docs/design.md` §8–§10, §18.5.

```
la-shipper [ship] [--once]
la-shipper compact [--once] [--no-lease]
la-shipper rebuild --from <YYYY-MM-DD> --to <YYYY-MM-DD> [--project <id>] [--with-side-tables] [--force]
la-shipper rebuild --abort
```

| Mod | Ne yapar |
|---|---|
| `ship` (varsayılan) | Shard kirasını (`shipper-<shardkey>`) tutar, son 48 saatin `wal/dt=…/hh=…/` öneklerini tarar, plan keser ve yürütür. `--once`: bekleyen her şeyi kirasız işler ve çıkar. |
| `compact` | `compactor-<shardkey>` kirasıyla L0→L1→L2 compaction, GC, saklama süresi, WAL yaşam döngüsü, kişi silme, kimlik/kişi tablosu birleştirme. `--once`: tek tam tur; `--no-lease`: kirasız. |
| `rebuild` | Bir **event günü** aralığının dosyalarını WAL'dan yeniden üretir (aşağıda). |

## Plan yürütme (kısaca)

`çöz → kimlik delta'sı → kişi delta'sı → L0 dosyaları → gün manifest'leri (plans[shardkey]=n) → şema istatistikleri → _done`.
Her adım `(shardkey, n)` başına idempotenttir; plan herhangi bir yerde kesilip yeniden yürütülebilir:

- L0 nesnesi deterministik adla **yoksa-oluştur** ile yazılır. Ad doluysa (önceki yürütme oraya kadar gelmiş)
  mevcut nesne okunur ve manifest girdisi **ondan** türetilir; üzerine yazılmaz. Böylece girdi her zaman
  işaret ettiği baytları anlatır.
- `plans[shardkey] ≥ n` olan gün atlanır (L0 çoktan compaction'a girmiş olabilir; geri gelmez).
- Kimlik/kişi delta'ları ve şema istatistikleri kendi `applied[shardkey]` alanlarıyla korunur.

## Ortam değişkenleri

Depo: `LA_STORE_URL` (`s3://bucket[/önek]` | `file:///dizin`), `LA_S3_ENDPOINT`, `LA_S3_REGION`,
`LA_S3_ACCESS_KEY`, `LA_S3_SECRET_KEY`, `LA_S3_BUCKET` (`spec/manifest.md` §0).

| Değişken | Varsayılan | Anlam |
|---|---|---|
| `LA_NODE_ID` | `<mod>-<host>-<pid>` | Kira ve plan `holder` adı |
| `LA_METRICS_ADDR` | `0.0.0.0:9102` (ship), `:9103` (compact) | `/metrics`, `/healthz`; `off` kapatır |
| `LA_LOG` | `info` | `tracing` filtresi; loglar JSON satırlarıdır, event içeriği loglanmaz |
| `LA_SHIPPER_BATCH_SECS` | `30` | Plan kesme aralığı |
| `LA_SHIPPER_BATCH_MB` | `64` | Plan başına açılmış bayt bütçesi (sıkıştırılmış boyut × ölçülen oran ile tahmin edilir) |
| `LA_SHIPPER_POLL_SECS` | `2` | Döngü periyodu |
| `LA_SHIPPER_SCAN_HOURS` | `48` | Taranan saat penceresi |
| `LA_SHIPPER_FULL_SCAN_SECS` | `60` | Tüm pencerenin yeniden listelenme sıklığı (arada yalnızca son 3 saat listelenir) |
| `LA_SHIPPER_MAX_SEGMENT_MB` | `1024` | Bundan büyük açılan segment karantinaya alınır |
| `LA_SHIPPER_SHARDS` | `*` | Bu sürecin çalışabileceği shard'lar: `0,1` (indeks) ya da `2-1` (shardkey) |
| `LA_SHIPPER_UNKNOWN_PROJECTS` | `reject` | `control/projects.json` yayınlanmışsa listede olmayan projenin kayıtları `unknown_project` ile atılır; `accept` kabul eder |
| `LA_COMPACTOR_L0_FILES` / `_L0_MB` | `8` / `256` | L1 tetikleyicisi (daha fazla L0 dosyası ya da baytı) |
| `LA_COMPACTOR_L1_MAX_FILES` | `32` | Açık günde L1 sayısı bunu aşınca L0+L1 tek L1'e birleştirilir (`0` = kapalı) |
| `LA_COMPACTOR_L2_DELAY_HOURS` | `6` | Gün kapandıktan sonra L2 gecikmesi |
| `LA_COMPACTOR_TARGET_MB` | `512` | Çıktı dosyası hedef boyutu (kişi sınırında bölünür) |
| `LA_COMPACTOR_MAX_INPUTS` | `32` | Bir işin birleştirdiği en çok dosya |
| `LA_COMPACTOR_BATCH_ROWS` | `4096` | Birleştirme batch boyutu |
| `LA_COMPACTOR_MEM_INPUT_MB` | `32` | Bundan küçük girdiler bellekten (iş başına en çok 4 katı), büyükleri spool dizininden okunur |
| `LA_COMPACTOR_MAX_MOVED_ROWS` | `200000` | Anahtarı değişen satırlar bunu aşınca diske sıralı run olarak dökülür |
| `LA_COMPACTOR_MAX_PERSON_UUIDS` | `2000000` | Kişi başına hatırlanan `uuid` sayısı (ötesinde yalnızca bitişik tekrarlar ayıklanır) |
| `LA_COMPACTOR_SPOOL` | sistem temp | Girdi/çıktı spool dizini |
| `LA_COMPACTOR_GC_GRACE_HOURS` | `24` | GC bekleme süresi |
| `LA_COMPACTOR_KEEP_MANIFESTS` | `20` | Saklanan manifest sürümü |
| `LA_COMPACTOR_HOT_DAYS` | `3` | Her turda bakılan gün sayısı |
| `LA_COMPACTOR_INTERVAL_SECS` | `60` | Tur periyodu |
| `LA_COMPACTOR_FULL_SCAN_MINUTES` | `60` | Tüm günlerin taranma + GC sıklığı |
| `LA_COMPACTOR_SHARDS` | `*` | Güncel shard haritasındaki shard indeksleri |
| `LA_WAL_RETENTION_DAYS` | `30` | `wal/` saklama; rebuild ufku |
| `LA_TOMBSTONE_RETENTION_DAYS` | `400` | Tombstone saklama |

## Metrikler

`la_shipper_`: `segments_pending{shard}`, `ship_lag_seconds{shard}`, `records_out_total`,
`records_rejected_total{reason}`, `parquet_bytes_total`, `manifest_conflicts_total`, `identity_links_total`,
`identity_conflicts_total`, `lease_held{shard}`, `segments_quarantined_total{reason}`, `wal_corrupt_tail_total`,
`entries_skipped_total{reason}`, `props_dropped_total`, `plans_executed_total`, `plans_reexecuted_total`,
`plan_seconds`, `columns_promoted_total`, `rebuild_fenced{shard}`, `errors_total`.

`la_compactor_`: `l0_files{project}` (ilk 20), `compaction_seconds{level}`, `bytes_rewritten_total`,
`gc_deleted_total`, `tombstones_pending`, `compactions_total{level}`, `rows_rewritten_total`,
`duplicates_dropped_total`, `tombstoned_rows_total`, `replay_objects_deleted_total`, `days_expired_total`,
`commit_conflicts_total`, `lease_held{shard}`, `manifest_conflicts_total`, `errors_total`.

## Operasyon notları

**Karantina.** Açılamayan (zstd), sihri bozuk, eksik ya da çok büyük segment atlanır:
`wal/_quarantine/<shardkey>/<segment>.json` kaydı (neden, plan) yazılır, `segments_quarantined_total` artar,
segment yine `_done` alır (hat durmaz). Yarım kuyruk (CRC/len) hata değildir: geçerli önek işlenir,
`wal_corrupt_tail_total` artar. Düzeltilmiş segmenti yeniden işletmek için: nesneyi yeni bir adla yükleyin
(ya da `_done` işaretini silin) — `uuid` tekrarları compaction/sorguda ayıklanır.

**Shard değişimi.** `control/shards.json`'a daha yüksek `version` ve ileri bir `from_ms` ile yeni harita ekleyin.
Segmentin `dt/hh` saatine göre eski ya da yeni harita uygulanır; iki haritanın shardkey'leri 48 saatlik pencere
boyunca birlikte çalışır (her biri kendi plan dizisi ve `_done` diziniyle). Geçiş penceresinde aynı projenin şema
dosyasına iki shardkey yazabilir: ikisini aynı süreçte çalıştırın (`LA_SHIPPER_SHARDS=*`). Rebuild sırasında harita
değiştirmeyin.

**Kira.** `shipper-<shardkey>` / `compactor-<shardkey>`, 30 sn yenileme, 90 sn TTL. Kira yalnızca boşa işi
önler; aynı planı iki sürecin yürütmesi aynı sonucu verir. Yedek süreç, sahip düşünce en geç TTL kadar sonra devralır.

**Rebuild.** `--from/--to` event günleridir (düzeltilmiş `ts`'nin UTC günü). Canlı bucket'ta güvenlidir;
ingestion, sorgu ve compaction çalışmaya devam eder, **shipping süre boyunca bekler**:

1. *Çit:* her shardkey'de en yüksek plan bitmemişse bitirilir, ardından sıradaki numarayla bir **rebuild planı**
   (plan JSON'unda `rebuild` alanı) yoksa-oluştur ile yazılır. Plan protokolü gereği en yüksek plan bitmeden yeni
   plan kesilmez; shipper bitmemiş rebuild planını "bekle" olarak yorumlar (`rebuild_fenced = 1`). Böylece
   `_done` işaretli (işlenmiş) segment kümesi donar.
2. *Yeniden oynatma:* `dt ≥ --from` olan işlenmiş segmentler güncel kimlik eşlemesi, güncel şema ve tüm
   tombstone'larla çözülür; aralıktaki satırlar yerel diske sıralı run olarak yazılır.
3. *Takas:* her `(proje, gün)` için run'lar L1 dosyalarına birleştirilir ve tek bir manifest sürümü o günün
   **tüm** dosyalarını bunlarla değiştirir; `plans` korunur ve `plans[shardkey]` rebuild planının numarasına
   yükseltilir (eski bir planın yeniden yürütülmesi L0'ını geri getiremez). Yarışan compaction ya kaybeder
   (girdileri gitmiştir, vazgeçer) ya da önce kazanır (çıktısı da değiştirilir). Eski dosyalar 24 saatlik GC'ye kalır.
4. *Çitin kalkması:* `wal/_done/<shardkey>/_rebuild-<n>.ok`. Shipper, bekleyen segmentlerle normal devam eder.

WAL'da satırı olmayan günlere dokunulmaz (raporlanır). `identity/` ve `persons/` yalnızca `--with-side-tables`
ile yazılır. Yarıda kalan rebuild aynı komutla sürdürülür; `--abort` yalnızca çiti kaldırır. `--from`, WAL saklama
ufkundan eskiyse reddedilir (`--force` ile geçilir).

**Kişi silme.** Shipper, bilinen tombstone'lara uyan satırları L0'a hiç yazmaz ve gün manifest'inin
`tombstone_ts` değerini uyguladığından ileride bırakmaz (`min`). Compactor etkilenen dosyaları yeniden yazar
(L0/L1 → tek L1, her L2 kendi yerinde), silinen `session_replay_chunk` satırlarının `props.key` nesnelerini
(yalnızca `replay/p=<pid>/` altındaysa) siler, tüm dosyalar temizse `tombstone_ts`'yi ilerletir; sonra kişi/kimlik
satırlarını siler. Kimlik bağı silinmeden önce `u:U` genişletmesi `<ts>-<id>-x.json` adlı ek bir tombstone'a
dondurulur (geç gelen veride de tanınsın diye). İlerleme: `tombstones/p=<pid>/_applied.state`.

**GC.** Bir veri dosyası ancak (a) güncel manifest'te yoksa, (b) 24 saat önce güncel olan manifest'te de yoksa ve
(c) nesnenin kendisi 25 saatten eskiyse silinir. Compaction çıktıları, onları ekleyen manifest sürümünün adını
taşır; manifest yarışı kaybedilirse çıktılar silinip yeni adla yeniden yüklenir.

## Ölçümler

> **Bu konteynerde, sentetik veriyle ölçüldü** (4 çekirdek, diğer derlemelerle paylaşılıyor, yük ortalaması 6–9;
> `--release`; depo `file://`). Gerçek veride yeniden ölçülmeden kapasite kararı verilmemelidir (design §18.4).
> Üreteç: `cargo run --release -p la-shipper --example bench`. Profil: 40 event'lik native batch'ler, 7 context
> anahtarı, event başına 5–7 özellik, UUIDv7 kimlikler, %30 kimlikli cihaz, tek gün.

| Ölçüm | Değer |
|---|---|
| WAL ham bayt/event | 109 |
| WAL zstd bayt/event (`w`) | **30,9** |
| L0 Parquet bayt/event | 23,5 (500 bin satırlık dosyalar) |
| L2 Parquet bayt/event (`b`) | **23,0** |
| Shipper verimi | **≈ 100 bin event/sn** (tek iş parçacığı; açma + çözme + sıralama + Parquet + yerel yazım; 250–500 bin event'lik planlar; ölçümler 91–102 bin arasında) |
| Shipper tepe RSS | 414 MB (250 bin event'lik planlar), 815 MB (500 bin event'lik planlar) — üreteç dahil; ≈ 1,5 KB/event |
| Compaction, 5 M satırlık gün | 10 L0 (117 MB) → 1 L2 (115 MB): **9,7 sn ≈ 510 bin satır/sn** (12 MB/sn); ≈ 225 bin satırın anahtarı kimlik eşlemesiyle değişti |
| Compaction tepe RSS | **362 MB** (varsayılan ayarlar: girdiler bellekte); 251 MB (`LA_COMPACTOR_MEM_INPUT_MB=1`, 32 MB'lık çıktılar) |

Shipper belleği plan boyutuyla orantılıdır (açılmış segmentler + satır başına ≈ 1,5 KB). Bu profildeki
gibi küçük event'lerde (≈ 110 bayt) 256 MB'lık bütçe ≈ 2,3 M event ≈ 3,5 GB demektir; bu yüzden varsayılan
64 MB'tır (≈ 0,6 M event, hesapla ≈ 0,9 GB — bu değer ölçülmedi, yukarıdaki satır başı maliyetten türetildi).
Belleği bol makinelerde `LA_SHIPPER_BATCH_MB` yükseltilerek plan (ve L0 dosyası) sayısı azaltılabilir.

Compaction belleği ise gün boyutundan bağımsızdır: `girdi sayısı × batch` + bellekteki girdiler (≤ 128 MB) + anahtarı
değişen satır tamponu (≤ `MAX_MOVED_ROWS`) + kişi başına `uuid` kümesi + yüklenmekte olan tek çıktı dosyası.

## Testler

```
cd engine
cargo test -p la-shipper -p la-compactor
cargo clippy -p la-shipper -p la-compactor --all-targets -- -D warnings
```

Kapsam: üç format için çözme/eşleme, saat vektörleri, sınırlar ve atılma nedenleri, bot filtresi, terfi (uçtan uca),
her adım sınırında çökme enjeksiyonu, eşzamanlı plan yürütme, compaction sonrası plan yeniden yürütme, sıra/batch
bağımsızlığı, compaction öncesi/sonrası mantıksal eşitlik, eşzamanlı ekleme/commit, tombstone akışı, GC, saklama,
WAL yaşam döngüsü, bozuk segment, rebuild.

## Bilinen eksikler ve bilinçli tercihler

- **Sıra bağımsızlığının tek istisnası:** aynı `anon_id` için çakışan identify bağlarında ilk **işlenen** kazanır
  (aynı plan içinde `(ts, uuid)` en küçük olan). Kişi tablosunda eşit `ts`'li yazımlar da işlenme sırasına bağlıdır.
- **Compaction, sorgudan daha güçlü tekilleştirir:** sorgu yalnızca eşit `(person_key, ts, uuid)` satırlarını
  birleştirir; compaction kişi başına her `uuid`'den en erkenini tutar. Aynı `uuid` farklı `ts` ile iki kez
  gelmişse (saat düzeltmeli yeniden gönderim) compaction'dan sonra bir satır azalır.
- L2'ye geç gelen dosyalar birikince **tüm gün** yeniden yazılır (parça bazlı birleştirme yok).
- Geçiş penceresinde iki shardkey ayrı süreçlerde aynı projenin şema dosyasına yazarsa son yazan kazanır
  (istatistik kaybı olabilir).
- Yalnızca `memory` ve `file://` arka uçlarıyla test edildi; gerçek S3/MinIO'da koşullu yazma ve hata/yeniden deneme
  davranışı ayrıca doğrulanmalı. Depo hatalarında yeniden deneme, döngünün bir sonraki turudur.
- Tek süreçte shardkey'ler sırayla işlenir; plan içi çözme tek iş parçacıklıdır. Shipper satırları `EventRow`
  olarak bellekte tutar (sütunlu kurulum yok): bkz. yukarıdaki bellek notu.
- 256 MB bütçesi sıkıştırılmış boyuttan tahmin edilir; tek bir dev segment bütçeyi aşabilir.
- Fuzz hedefleri (`cargo fuzz`) ve uzun süreli yük testi yok.
