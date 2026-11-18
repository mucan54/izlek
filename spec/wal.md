# WAL segment formatı v1 (`LAW1`)

Kaynak: `docs/design.md` §6.2–6.4, §8.

## 1. Segment

```
Segment  := "LAW1" Entry*
Entry    := u32le(len) u32le(crc32c) Envelope Body
Envelope := format:u8 project_id:u32le recv_ts_ms:u64le country:u8[2] flags:u8     ; 16 bayt
```

- `len` = `Envelope + Body` bayt sayısı (≥ 16, ≤ 16 + 1.048.576 + 4.096).
- `crc32c` = CRC-32C (Castagnoli), `Envelope + Body` üzerinde.
- `country`: ISO-3166-1 alpha-2, ASCII büyük harf; bilinmiyorsa `"ZZ"`.
- `flags`: bit0 = web kaynağı (istek `Origin` başlığı taşıyordu).

Okuyucu kuralı: bir girdinin `len` alanı dosya sonunu aşıyorsa ya da CRC tutmuyorsa okuma **orada biter** (yarım yazılmış kuyruk). Yerel dosyada bu kuyruk kırpılır; yüklenmiş segmentte kuyruk yok sayılır ve `wal_corrupt_tail_total` metriği artar.

## 2. `format` değerleri

### 2.1 `format = 1` — native batch
`Body` = açılmış (gzip'siz) `LA1` batch'i, Magic dahil (`wire.md`).

### 2.2 `format = 2` — PostHog JSON
`Body` = açılmış JSON metni; istek gövdesi sıkıştırma/base64 katmanları ingestion'da soyulur, JSON'un kendisi değiştirilmez. Üç biçim geçerlidir:

1. Nesne + `batch` dizisi: `{"api_key": "...", "batch": [Event…], "sent_at": "…"}`
2. Tek event nesnesi: `{"api_key"|"token": "...", "event": "...", "properties": {...}, …}`
3. Event dizisi: `[Event…]`

Token çözümü (ingestion, sırayla): üst düzey `api_key` → üst düzey `token` → ilk event'in `properties.token` → `Authorization: Bearer` → `?t=`.

Eşleme (shipper) için bkz. §3.

### 2.3 `format = 3` — replay indeks kaydı
```
Body := session_id:bytes16 seq:varint start_ts:varint duration_ms:varint
        bytes:varint encoding:u8 anon_id:string user_id:string key:string
```
- `encoding`: 1 = zstd, 2 = gzip.
- `key`: parçanın object storage anahtarı (`manifest.md` §1).

Shipper bunu şu event'e çevirir: `event = "session_replay_chunk"`, `session_id`, `anon_id`, `user_id`, `ts = start_ts` (saat düzeltmesi kuralının yalnızca "gelecekte ise sabitle" ve "late" adımları uygulanır), `uuid = uuid8(sha256("la-replay" ‖ session_id ‖ varint(seq))[0..16])`, `props = {"seq", "bytes", "duration_ms", "key", "encoding"}`.

`uuid8(b)`: 16 baytta sürüm nibble'ı 8, varyant bitleri `10` yapılır (`b[6] = (b[6] & 0x0F) | 0x80; b[8] = (b[8] & 0x3F) | 0x80`).

## 3. PostHog → native eşlemesi (shipper)

Her `Event` nesnesi için:

| Alan | Kural |
|---|---|
| `distinct_id` | `event.distinct_id` → `event.properties.distinct_id` → `event.$distinct_id`. String değilse JSON metnine çevrilir. Boşsa kayıt atılır. |
| `name` | `event.event` |
| `uuid` | `event.uuid` geçerli UUID ise o; değilse `uuid8(sha256("la-ph" ‖ u32le(project_id) ‖ ham_event_baytları)[0..16])` |
| `ts` | Aşağıdaki sırayla: (a) `event.offset` (ms, sayı) varsa `recv_ts − offset`; (b) `event.timestamp` + (`event.sent_at` ya da zarf `sent_at`) varsa `wire.md` §5 saat düzeltmesi; (c) yalnız `timestamp` varsa o (gelecekteyse `recv_ts`); (d) hiçbiri yoksa `recv_ts`. Zaman damgaları ISO-8601/RFC 3339 ya da unix (sn/ms) sayı olabilir. |
| `session_id` | `properties.$session_id` UUID ise o; değilse yok |
| kimlik | `properties.$is_identified == true` ise `user_id = distinct_id`, `anon_id = properties.$device_id` (string ise) yoksa `distinct_id`. Aksi halde `anon_id = distinct_id`, `user_id` yok. |
| sabit sütunlar | `$os`→`os`, `$os_version`→`os_version`, `$device_model`→`device_model`, `$device_type`→`device_type`, `$app_version`→`app_version`, `$locale`→`locale`, `$lib`+`/`+`$lib_version`→`sdk`, `$pathname`→`url_path`, `$referring_domain`→`referrer_host`, `$screen_name`→`screen`. `platform`: `$lib` değerinden (`web`, `posthog-android`→`android`, `posthog-ios`→`ios`, diğerleri `$lib` değeri). Bu anahtarlar `props`'ta **da** korunur. |
| `props` | `properties` içinden `token`, `distinct_id`, `$set`, `$set_once`, `$unset`, `$anon_distinct_id` dışındaki her şey; anahtar adları korunur. Değerler: `null/bool/sayı/string` doğrudan, dizi/nesne JSON olarak. |
| `flags` | `compat` biti (bit2) kurulur. |

Ek kayıtlar:

- `event == "$identify"` ve `properties.$anon_distinct_id` (string, `distinct_id`'den farklı): `identify` bağı `anon=$anon_distinct_id → user=distinct_id` **ve** öz-bağ `anon=distinct_id → user=distinct_id` (sonraki `distinct_id = user` event'leri aynı kişiye çözülsün diye). Event'in kendisi `user_id = distinct_id` ile yazılır.
- `event == "$create_alias"`: `properties.alias` (string) için bağ `anon=alias → user=distinct_id` ve öz-bağ.
- `properties.$set` / `event.$set` (nesne): `set_person` kaydı. `$set_once`: aynı, ama yalnızca anahtar daha önce yoksa (kişi tablosunda `ts = 0` ile yazılır; son-yazan-kazanır kuralında en zayıf olur).
- `$identify` / `$create_alias` / `$set` event'leri de sıradan event olarak yazılır.

## 4. Nesne anahtarı

Kapanan segment zstd (seviye 3) ile sıkıştırılır ve şuraya yüklenir:

```
wal/dt=<YYYY-MM-DD>/hh=<HH>/<node_id>-<seq:012>.wal.zst
```

`dt/hh` segmentin **kapanış** anıdır (UTC). `<node_id>` `[a-z0-9-]{1,32}`; `seq` node başına artan, 12 haneye sıfırla doldurulmuş sayı (yeniden başlatmada yerel dizin ve saat tabanlı başlangıçla tekilliği korunur: `seq` başlangıcı = `max(yereldeki en büyük seq + 1, unix_sn)`).

Yükleme koşulsuz `PUT`'tur (aynı anahtar aynı içerik demektir; tekrar yükleme güvenlidir).
