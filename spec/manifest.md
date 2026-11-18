# Object storage düzeni, manifest'ler ve kontrol dosyaları

Kaynak: `docs/design.md` §7–§10, §14.2. Tüm JSON dosyaları UTF-8'dir; bilinmeyen alanlar okuyucularca yok sayılır.

## 0. Depo soyutlaması

Bileşenler tek bir ortam değişkeni ailesiyle aynı depoya bağlanır:

| Değişken | Anlam |
|---|---|
| `LA_STORE_URL` | `s3://<bucket>[/<önek>]` ya da `file:///mutlak/dizin` (tek makine / test) |
| `LA_S3_ENDPOINT` | S3 uyumlu uç (path-style adresleme) |
| `LA_S3_REGION` | varsayılan `us-east-1` |
| `LA_S3_ACCESS_KEY`, `LA_S3_SECRET_KEY` | kimlik |
| `LA_S3_BUCKET` | `LA_STORE_URL` yoksa `s3://$LA_S3_BUCKET` varsayılır |

Gerekli depo işlemleri: `GET` (aralıklı dahil), `PUT`, **`PUT If-None-Match: *`** (yoksa-oluştur), `DELETE`, önekle `LIST`. Başka koşullu işlem (If-Match) **gerekmez**; tüm eşgüdüm yoksa-oluştur üzerine kuruludur.

`file://` arka ucunda yoksa-oluştur, geçici dosya + `link(2)` (ya da `O_EXCL`) ile atomik uygulanır; Go ve Rust (`object_store::local`) aynı dizin düzenini kullanır: nesne anahtarı = kök altındaki göreli yol.

## 1. Anahtar düzeni

```
wal/dt=<YYYY-MM-DD>/hh=<HH>/<node>-<seq:012>.wal.zst
wal/_plan/<shardkey>/<n:010>.json               ; shipper iş planı (§6)
wal/_done/<shardkey>/<node>-<seq:012>.ok        ; boş nesne

data/p=<pid>/d=<YYYY-MM-DD>/L0-<shardkey>-<n:010>.parquet
data/p=<pid>/d=<YYYY-MM-DD>/L1-<v:08>-<i:03>.parquet
data/p=<pid>/d=<YYYY-MM-DD>/L2-<v:08>-<i:03>.parquet
data/p=<pid>/d=<YYYY-MM-DD>/_m/<v:08>.json

identity/p=<pid>/map-<etiket>.parquet
identity/p=<pid>/_m/<v:08>.json

persons/p=<pid>/props-<etiket>.parquet
persons/p=<pid>/_m/<v:08>.json

tombstones/p=<pid>/<ts_ms:013>-<id>.json

replay/p=<pid>/d=<YYYY-MM-DD>/<session_hex32>/<seq:06>.bin.zst | .bin.gz

flags/<token>.json
control/tokens.json
control/keys.json
control/projects.json
control/shards.json
control/schema/p=<pid>.json
control/lease/<ad>/<epoch:012>.json
```

- `<pid>`: ondalık proje kimliği (`u32`, ≥ 1).
- `<shardkey>`: `<shards_version>-<shard>` (ör. `1-0`).
- L1/L2 adındaki `<v>`: dosyayı **ekleyen** manifest sürümü; `<i>`: o compaction çıktısındaki parça sırası.
- Gün (`d=`) her zaman event'in düzeltilmiş `ts` değerinin **UTC** günüdür.

## 2. Gün manifest'i

`data/p=<pid>/d=<day>/_m/<v:08>.json`

```json
{
  "version": 42,
  "project": 1017,
  "day": "2026-11-18",
  "created_at": 1795046400000,
  "plans": { "1-0": 1234 },
  "tombstone_ts": 0,
  "files": [
    {
      "key": "data/p=1017/d=2026-11-18/L2-00000042-000.parquet",
      "level": 2, "rows": 18234112, "bytes": 412334551,
      "min_ts": 1795046400000, "max_ts": 1795132799999,
      "min_person": "a:0190…", "max_person": "u:zz…",
      "identity_version": 7, "schema_version": 12
    }
  ]
}
```

| Alan | Anlam |
|---|---|
| `version` | 1'den başlar, her yazımda +1 |
| `plans` | `shardkey → n`: bu güne uygulanmış en yüksek shipper plan numarası. Shipper plan `n`'yi eklerken `n ≤ plans[shardkey]` ise **atlar** (plan daha önce uygulanmış, L0'ı belki compaction'a girmiştir). |
| `tombstone_ts` | Tüm dosyalara uygulanmış en büyük tombstone `ts_ms` değeri |
| `files[].identity_version` | Dosya yazılırken tamamen uygulanmış kimlik manifest sürümü |
| `files[].schema_version` | Dosyanın yazıldığı proje şema sürümü |
| `files[].min_person/max_person` | Dosyadaki en küçük/büyük `person_key` (bayt sıralı) |

**Yazım protokolü** (tüm manifest türleri için aynı):

1. `_m/` altını listele, en büyük `v`'yi oku (yoksa boş manifest, `version = 0`).
2. Değişikliği uygula, `version = v + 1`.
3. `_m/<v+1>.json`'ı **yoksa-oluştur** ile yaz.
4. "Zaten var" hatasında 1'e dön (üstel geri çekilme + jitter, `manifest_conflicts_total` artır).

Okuyucular en yüksek sürümü kullanır; 10 sn bellek önbelleği serbesttir. Manifest'te listelenen dosyalar değişmezdir.

Tek istisna: L0 dosyaları **deterministik adla** yeniden yazılabilir (plan yeniden yürütme); içerik mantıksal olarak aynıdır.

**Dosya içi düzen:** her veri dosyası `(person_key, ts, uuid)` artan sıralıdır (bayt karşılaştırması; `ts` sayısal).

## 3. Kimlik eşlemesi

`identity/p=<pid>/_m/<v:08>.json`

```json
{ "version": 7, "project": 1017,
  "applied": { "1-0": 1234 },
  "files": [ { "key": "identity/p=1017/map-1-0-0000001234.parquet",
               "rows": 12, "min_version": 7, "max_version": 7 } ] }
```

Parquet şeması (`map-*.parquet`), `anon_id` sıralı:

| Sütun | Tip | Not |
|---|---|---|
| `anon_id` | STRING | |
| `user_id` | STRING | |
| `version` | INT64 | Bağın eklendiği kimlik manifest sürümü |
| `ts` | TIMESTAMP(ms, UTC) | `identify` kaydının zamanı |

Kurallar:
- Bir `anon_id` için **ilk işlenen** bağ kalıcıdır; farklı `user_id` ile gelen sonraki bağ yazılmaz (`identity_conflicts_total`).
- Aynı plan içinde aynı `anon_id` için birden çok aday varsa `(ts, uuid)` en küçük olan seçilir.
- Shipper delta dosyası adı `map-<shardkey>-<n:010>.parquet`'tir; `applied[shardkey] ≥ n` ise delta zaten uygulanmıştır.
- Delta yazımı: mevcut sürüm `v` okunur → satırlar `version = v + 1` ile yazılır → manifest `v + 1` yoksa-oluştur. Çakışmada dosya yeni sürüm numarasıyla **üzerine yazılır** ve tekrar denenir (dosya henüz hiçbir manifest'te yoktur).
- Compactor, dosya sayısı > 16 olunca hepsini tek dosyada birleştirir (`map-c<v:08>.parquet`, satır `version` değerleri korunur).
- Shipper veri manifest'ini yazmadan **önce** kimlik delta'sını işler; veri dosyasının `identity_version`'ı o an okunan kimlik manifest sürümüdür.

Sorgu: `identity_version = k` olan veri dosyasına yalnızca `version > k` bağlar uygulanır ve yalnızca `person_key` değeri `"a:"` ile başlayan satırlara (bunlar dosyanın **başında bitişik** durur, çünkü `"a:" < "u:"`).

## 4. Kişi özellikleri

`persons/p=<pid>/_m/<v:08>.json` — kimlik manifest'iyle aynı biçim (`applied`, `files[{key, rows}]`).

Parquet şeması (`props-*.parquet`), `(person_key, key)` sıralı:

| Sütun | Tip | Not |
|---|---|---|
| `person_key` | STRING | Yazım anındaki anahtar (`a:`/`u:`) |
| `key` | STRING | |
| `value` | STRING | JSON kodlu değer (`"abc"`, `12`, `true`, `null`, …) |
| `ts` | TIMESTAMP(ms, UTC) | `$set_once` için 0 |
| `seq` | INT64 | Eşit `ts` için bağ bozucu: yazan kişi manifest sürümü |

Okuma: satırların `person_key`'i güncel kimlik eşlemesiyle yeniden çözülür (`a:X` → `u:U`), sonra `(person_key, key)` başına `(ts, seq)` en büyük olan kazanır. `value = null` özelliği siler. Compactor dosya sayısı > 16 olunca birleştirir ve kazanmayan satırları atar.

## 5. Proje şeması (terfi edilmiş sütunlar)

`control/schema/p=<pid>.json` — koşulsuz `PUT`; tek yazar o projenin shard'ındaki shipper'dır.

```json
{ "version": 12, "project": 1017,
  "columns": [ { "key": "plan", "type": "string", "since": 9 },
               { "key": "amount", "type": "float", "since": 12 } ],
  "stats": { "window_start": "2026-11-12", "days": { "2026-11-18": { "events": 120000,
             "keys": { "plan": { "n": 80000, "types": { "string": 80000 } } } } } } }
```

- `type`: `string` | `int` | `float` | `bool`. `int` ve `float` karışık görülen anahtar `float` sayılır.
- Sütun adı `p_<key>`; yalnızca `^[A-Za-z0-9_$]{1,64}$` kalıbına uyan anahtarlar terfi eder.
- Terfi koşulu: son 7 günde event'lerin ≥ %5'i ve ≥ 1.000 event'te, tek tutarlı tiple. En çok 200 sütun; sütunlar **silinmez** (geri alma yok), `version` her eklemede artar.
- `stats.days` başına en çok 2.000 anahtar tutulur (en sık görülenler).

## 6. Shipper planı ve `_done`

`wal/_plan/<shardkey>/<n:010>.json` (yoksa-oluştur):

```json
{ "n": 1234, "shardkey": "1-0", "created_at": 1795046400000, "holder": "ship-a",
  "segments": [ "wal/dt=2026-11-18/hh=21/ing-01-000001795046.wal.zst" ] }
```

Döngü:

1. En büyük `n`'li planı oku. Segmentlerinden herhangi birinin `_done` işareti yoksa **o planı** (yeniden) yürüt.
2. Aksi halde `_done`'sız segmentleri anahtar sırasıyla topla (30 sn ya da 256 MB açılmış), plan `n + 1`'i yoksa-oluştur ile yaz. "Zaten var" → 1'e dön.
3. Yürüt: çöz → kimlik delta'sı (§3) → kişi delta'sı (§4) → L0 dosyaları (ad plan numarasından) → gün manifest'leri (`plans[shardkey] = n`) → şema istatistikleri → `_done` işaretleri.

Aynı planın iki kez (hatta eşzamanlı) yürütülmesi aynı sonucu verir. Bir planın tüm adımları bitmeden sonraki plan oluşturulmaz.

Segment listesi her zaman `wal/dt=…/hh=…/` önekleri üzerinden yapılır; shipper son 48 saatin öneklerini tarar (`--rebuild` tüm aralığı tarar).

## 7. Kira (lease)

`control/lease/<ad>/<epoch:012>.json` (yoksa-oluştur), `<ad>` ör. `shipper-1-0`, `compactor-1-0`.

```json
{ "epoch": 57, "holder": "ship-a", "acquired_at": 1795046400000, "ttl_ms": 90000 }
```

- Sahip, her 30 sn'de `epoch + 1`'i kendi adına oluşturarak yeniler.
- Başka bir node yalnızca en yüksek epoch'un süresi dolmuşsa (`acquired_at + ttl_ms < şimdi`) `epoch + 1`'i oluşturmayı dener.
- "Zaten var" → kira başkasında.
- Eski epoch nesneleri (son 5 dışı) sahibi tarafından silinir.

Kira yalnızca **boşa işi** önler; doğruluk plan/manifest protokolüne dayanır.

## 8. Kontrol dosyaları (control plane yazar, koşulsuz `PUT`)

`control/tokens.json` (ingestion okur):

```json
{ "version": 12, "generated_at": 1795046400000,
  "tokens": { "pk_live_abc": { "project_id": 1017, "status": "active",
              "rate_limit": 2000, "allowed_origins": ["https://app.example.com"],
              "replay": true } } }
```
`status`: `active` | `disabled`. `rate_limit` 0/yok = varsayılan. `allowed_origins` boş/yok = denetim yok.

`control/keys.json` (query okur) — gizli anahtarın kendisi değil **SHA-256**'sı:

```json
{ "version": 3, "keys": { "<hex(sha256('sk_…'))>": { "project_id": 1017, "scopes": ["query"] } } }
```

`control/projects.json` (shipper, compactor, query okur):

```json
{ "version": 5, "projects": { "1017": { "name": "Acme", "retention_days": 365,
    "replay_retention_days": 30, "drop_keys": ["email"],
    "scan_limit_bytes": 0, "max_concurrent_queries": 4 } } }
```

`control/shards.json`:

```json
{ "maps": [ { "version": 1, "from_ms": 0, "shards": 1 } ] }
```
`shard = murmur3_32(ondalık(project_id)) mod shards`. Bir segmente, anahtarındaki `dt/hh` saatinin başlangıcı `from_ms ≤ t` koşulunu sağlayan en yüksek sürümlü harita uygulanır. Dosya yoksa tek shard (`1-0`) varsayılır.

## 9. Tombstone

`tombstones/p=<pid>/<ts_ms:013>-<id>.json`

```json
{ "id": "del_01J…", "ts_ms": 1795046400000,
  "person_keys": ["u:42"], "anon_ids": ["0190…"] }
```

Motor genişletmesi: `person_keys` içindeki her `u:U` için kimlik eşlemesinde `U`'ya bağlı tüm `anon_id`'ler de kapsanır. Bir satır; `person_key ∈ person_keys` **ya da** `anon_id ∈ anon_ids ∪ bağlılar` **ya da** `"u:" + user_id ∈ person_keys` ise silinmiş sayılır.

- Query: tombstone listesini ≤ 60 sn'de yeniler; `ts_ms > manifest.tombstone_ts` olanları satır filtresi olarak uygular.
- Compactor: etkilenen günleri yeniden yazar, `tombstone_ts`'yi ilerletir; kişi/kimlik satırlarını ve `replay/` nesnelerini siler.
- Tombstone dosyaları 400 gün saklanır (geç gelen/yeniden inşa edilen veriye de uygulanabilsin diye).

## 10. Çöp toplama ve saklama

- Veri dosyası: hiçbir **güncel** manifest'te yok **ve** 24 saatten eski → silinir.
- Manifest: son 20 sürüm dışı ve 24 saatten eski → silinir.
- Saklama: `d=<day>` günü `retention_days`'den eskiyse tüm gün öneki silinir.
- `wal/`: 30 gün (segment + `_done` + `_plan`; en yüksek plan her zaman korunur).
- `replay/`: `replay_retention_days`.
