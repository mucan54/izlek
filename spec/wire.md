# Wire protokolü v1 (`LA1`)

Kaynak: `docs/design.md` §3. Bu dosya normatiftir; belirsizlikte **test vektörleri** (`spec/vectors/`) kazanır.

## 1. İlkel tipler

| Tip | Kodlama |
|---|---|
| `varint` | İşaretsiz LEB128, en fazla 10 bayt, değer `u64`. Kodlayıcılar **en kısa** biçimi üretir. Çözücüler en kısa olmayan biçimi kabul eder ama 10 bayttan uzun ya da `u64`'ü taşan diziyi reddeder. |
| `zigzag` | `(n << 1) ^ (n >> 63)` sonucu `varint`. `int64` içindir. |
| `u8` | 1 bayt |
| `bytes16` | 16 ham bayt. UUID'ler metin biçimindeki sırayla (big-endian, RFC 9562) yazılır. |
| `string` | `varint(len)` + `len` bayt UTF-8. Geçersiz UTF-8 tam çözümde hatadır. |
| `f64` | IEEE-754, 8 bayt **little-endian** |

## 2. Batch

```
Batch   := Magic Header Record*
Magic   := 0x4C 0x41 0x31 0x00            ; "LA1\0"
Header  := varint(len) HeaderBody          ; 16 ≤ len ≤ 65536
Record  := varint(len) RecordBody          ; 1 ≤ len ≤ 65536
```

`HeaderBody` (sıra sabit):

| # | Alan | Tip |
|---|---|---|
| 1 | `batch_id` | bytes16 |
| 2 | `sent_at` | varint (unix ms; 0 = bilinmiyor) |
| 3 | `sdk` | string |
| 4 | `anon_id` | string |
| 5 | `user_id` | string (boş = yok) |
| 6 | `session_id` | bytes16 (tümü sıfır = yok) |
| 7 | `context` | PropList |

`RecordBody`:

| # | Alan | Tip |
|---|---|---|
| 1 | `kind` | u8: 0 event · 1 identify · 2 set_person · 3 reset |
| 2 | `uuid` | bytes16 |
| 3 | `ts` | varint (unix ms) |
| 4 | `name` | string (`kind≠0` için boş) |
| 5 | `props` | PropList |

**İleri uyum:** `HeaderBody` ve `RecordBody` içinde bilinen alanlardan sonra kalan baytlar yok sayılır (ileride alan eklenebilir). Bilinmeyen `kind` değeri taşıyan kayıt atlanır (hata değildir). Bilinmeyen `tag` kaydı geçersiz kılar (uzunluğu bilinemez).

```
PropList := varint(count) Prop{count}
Prop     := key:string tag:u8 value
```

| Tag | Tip | Değer kodlaması |
|---|---|---|
| 0 | null | yok |
| 1 | false | yok |
| 2 | true | yok |
| 3 | int64 | zigzag |
| 4 | float64 | f64 |
| 5 | string | string |
| 6 | json | string (geçerli JSON metni; dizi/nesne) |

Aynı anahtar bir PropList'te birden çok kez geçerse **son** değer geçerlidir. Kayıt `props`'u, başlık `context`'ini aynı anahtarda ezer.

### Ayrılmış anahtarlar
Aşağıdaki context/props anahtarları shipper tarafından sabit sütunlara taşınır (bkz. `parquet.md`) ve `props` JSON'una yazılmaz:

`platform`, `os`, `os_version`, `device_model`, `device_type`, `app_version`, `locale`, `sdk`*, `path`→`url_path`, `referrer`→`referrer_host` (yalnızca host), `screen`.

\* `sdk` başlık alanından gelir; context'teki `sdk` yok sayılır.

`screen()` çağrısı `name = "screen"` event'i ve `props.screen = <ad>` üretir. Web `pageview`/`pageleave` event'leri `path` taşır. `identify` kaydında `props["user_id"]` (string) zorunludur; diğer anahtarlar kişi özelliğidir. `set_person` kaydının tüm `props`'u kişi özelliğidir.

## 3. Sınırlar

| Sınır | Değer | Kim denetler |
|---|---|---|
| Açılmış batch | 1.048.576 bayt | ingestion → `413` |
| Sıkıştırılmış gövde | 1.048.576 bayt | ingestion → `413` |
| Batch başına kayıt | 500 | ingestion → `413` |
| Başlık | 65.536 bayt | ingestion → `400` |
| Tek kayıt | 65.536 bayt | ingestion → `400` |
| Kayıt başına özellik | 200 | shipper (kayıt atılır) |
| Anahtar | 128 bayt | shipper (kayıt atılır) |
| String değer / json | 8.192 bayt | shipper (değer kırpılmaz, özellik atılır) |
| Event adı | 128 bayt | shipper (kayıt atılır) |
| `anon_id`, `user_id` | 200 bayt | shipper (batch/kayıt atılır) |

Sıcak yol (ingestion) yalnızca **çerçeveyi** doğrular:

1. İlk 4 bayt = Magic.
2. Başlık `varint(len)` geçerli, `16 ≤ len ≤ 65536`, gövde içinde.
3. Kalan baytlar tam olarak `varint(len) + len` bayt kayıtlardan oluşur (`1 ≤ len ≤ 65536`); artık bayt kalmaz.
4. Kayıt sayısı ≤ 500.

Kayıt içerikleri çözülmez. `batch_id` = `HeaderBody[0..16]`.

Sıfır kayıtlı geçerli batch `200` alır ve WAL'a yazılmaz.

## 4. HTTP

`POST /v1/e`
- Kimlik: `Authorization: Bearer pk_…` ya da `?t=pk_…`.
- `Content-Type: application/octet-stream` (denetlenmez). `Content-Encoding: gzip` isteğe bağlı.
- Yanıt gövdesi boştur. Kodlar: `docs/design.md` §3.5. `429` ve `503` yanıtları `Retry-After: <saniye>` taşır.

İdempotency: sunucu `(project_id, batch_id)` çiftini node-yerel bir LRU'da tutar (varsayılan 100.000 girdi / 10 dk). Tekrarlanan batch `200` alır, yeniden yazılmaz. Node'lar arası tekrarlar `uuid` bazında compaction ve sorgu birleştirmesinde ayıklanır.

## 5. Saat düzeltmesi (shipper)

Girdi: `recv_ts`, `sent_at`, kayıt `ts` (hepsi unix ms).

```
skew = recv_ts - sent_at                     (sent_at == 0 ise skew = 0)
flags = 0
eğer |skew| > 5000:  ts = ts + skew ; flags |= CLOCK_CORRECTED
eğer ts > recv_ts:   ts = recv_ts   ; flags |= CLOCK_CORRECTED
eğer recv_ts - ts > 30 gün (2.592.000.000 ms): flags |= LATE
eğer ts < 0: ts = 0
```

Vektörler: `spec/vectors/clock.json`.

## 6. Test vektörleri

`spec/vectors/wire.json` — `valid[]`: mantıksal batch ↔ hex; `invalid[]`: hex + beklenen sonuç.
Kodlayıcılar `valid[].batch`'ten **bayt bayt aynı** `hex`'i üretmeli; çözücüler `hex`'ten aynı mantıksal batch'i çıkarmalıdır. Vektör biçimi `spec/vectors/README.md`'dedir.
