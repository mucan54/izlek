# `events` Parquet şeması

Kaynak: `docs/design.md` §8.3–8.4, §9.

## 1. Sütunlar (sıra sabit; `p_*` sütunları şema sırasıyla `flags` ile `props` arasındadır)

| # | Sütun | Arrow tipi | Null | Not |
|---|---|---|---|---|
| 1 | `uuid` | FixedSizeBinary(16) | hayır | |
| 2 | `ts` | Timestamp(ms, "UTC") | hayır | Düzeltilmiş (`wire.md` §5) |
| 3 | `recv_ts` | Timestamp(ms, "UTC") | hayır | |
| 4 | `event` | Utf8 | hayır | Sözlük kodlamalı sayfalar |
| 5 | `person_key` | Utf8 | hayır | `"u:"+user_id` ya da `"a:"+anon_id` |
| 6 | `anon_id` | Utf8 | hayır | |
| 7 | `user_id` | Utf8 | evet | |
| 8 | `session_id` | FixedSizeBinary(16) | evet | |
| 9 | `platform` | Utf8 | evet | |
| 10 | `os` | Utf8 | evet | |
| 11 | `os_version` | Utf8 | evet | |
| 12 | `device_model` | Utf8 | evet | |
| 13 | `device_type` | Utf8 | evet | |
| 14 | `app_version` | Utf8 | evet | |
| 15 | `locale` | Utf8 | evet | |
| 16 | `country` | Utf8 | evet | WAL zarfından; `"ZZ"` → null |
| 17 | `sdk` | Utf8 | evet | |
| 18 | `url_path` | Utf8 | evet | |
| 19 | `referrer_host` | Utf8 | evet | |
| 20 | `screen` | Utf8 | evet | |
| 21 | `flags` | Int32 | hayır | bit0 `clock_corrected` · bit1 `late` · bit2 `compat` · bit3 `web` |
| … | `p_<key>` | Utf8 / Int64 / Float64 / Boolean | evet | Terfi edilmiş (`manifest.md` §5) |
| son | `props` | Utf8 | hayır | Kalan özellikler; JSON nesnesi, anahtarlar bayt sıralı, boşluksuz |

Parquet fiziksel tipler: `uuid`/`session_id` → `FIXED_LEN_BYTE_ARRAY(16)`; zaman damgaları → `INT64` + `TIMESTAMP(MILLIS, isAdjustedToUTC=true)`.

Yazım ayarları: zstd (seviye 3), satır grubu ≤ 100.000 satır, sütun istatistikleri (chunk + sayfa), sayfa indeksi açık, sözlük açık (`uuid` hariç), veri sayfası ~1 MB. Dosyada `sorting_columns = [person_key ASC, ts ASC, uuid ASC]` bildirilir.

Dosya anahtar-değer üst verisi: `la.schema_version`, `la.identity_version`, `la.level`, `la.project`, `la.day`.

## 2. Satır üretimi (shipper)

Bir kayıt (context ∪ props; props kazanır) için:

1. `kind = 0` değilse event satırı üretilmez (identify/set_person ayrı tablolara gider; `reset` atılır).
2. `wire.md` "Ayrılmış anahtarlar" sabit sütunlara taşınır (string değilse `props`'ta kalır, sütun null).
   `referrer`: URL ise host kısmı, değilse değerin kendisi (≤ 255 bayt).
3. Projenin `drop_keys` listesindeki anahtarlar atılır.
4. Terfi edilmiş her anahtar için: değer tipi sütun tipine uyuyorsa sütuna yazılır ve `props`'tan çıkarılır. `int` → `float` sütununa yükseltilir. Uymuyorsa `props`'ta kalır, sütun null.
5. Kalanlar `props` JSON'una: `null/bool/int/float/string` doğrudan; `json` tag'i ayrıştırılıp gömülür (geçersizse string olarak). `NaN/±Inf` → `null`.
6. `person_key` (`docs/design.md` §9): satırda `user_id` varsa `"u:"+user_id`; yoksa kimlik eşlemesinde `anon_id` varsa `"u:"+eşlenen`; yoksa `"a:"+anon_id`.

Atılma nedenleri (`records_rejected_total{reason}`): `empty_name`, `empty_id`, `too_long`, `too_many_props`, `bad_utf8`, `bad_record`, `bot`, `unknown_project`.

Bot filtresi: zarf `web` biti kuruluysa ve context `ua` değeri bilinen bot kalıplarından birini içeriyorsa (`bot`, `crawler`, `spider`, `headlesschrome`, `lighthouse`, `pingdom`, `phantomjs` — büyük/küçük harf duyarsız).

## 3. Şema evrimi

- Dosyalar yazıldıkları `schema_version`'daki sütun kümesini taşır. Okuyucu, tablo şemasını **güncel** proje şemasından kurar; dosyada olmayan `p_*` sütunu null okunur.
- `props.x` mantıksal erişimi: `p_x` sütunu varsa `coalesce(p_x, props->x)`, yoksa `props->x`. (Terfi öncesi dosyalarda ve tip uyuşmazlığında değer `props`'tadır.)
- Sabit sütun eklemek format sürüm değişikliğidir (N−1 okuyucular bilinmeyen sütunu yok sayar).

## 4. Şema istatistikleri

Shipper her plan sonunda `control/schema/p=<pid>.json` içindeki `stats`'ı günceller; plan yeniden yürütmede çift sayımı önlemek için dosyada `"applied": {"<shardkey>": <n>}` tutulur (`n ≤ applied` ise istatistik eklenmez).
