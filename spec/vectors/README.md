# Test vektörleri

Tüm dosyalar `gen.py` (bağımsız Python referans uygulaması) tarafından üretilir. Elle düzenlenmez:

```
python3 spec/vectors/gen.py          # yeniden üret
python3 spec/vectors/gen.py --check  # depodaki dosyalar güncel mi (CI)
```

| Dosya | İçerik | Kim geçmeli |
|---|---|---|
| `wire.json` | LA1 batch: `valid` (mantıksal ↔ hex), `tolerant` (hex → mantıksal), `invalid` | KMP SDK, Web SDK (kodlama); Go ingestion (çerçeve); Rust codec (çözme + kodlama) |
| `clock.json` | Saat düzeltmesi girdi/çıktıları | Rust shipper |
| `murmur3.json` | `murmur3_32` (seed 0) | KMP SDK, Web SDK, Rust (shard), Go (control) |
| `flags.json` | Kural seti + değerlendirme örnekleri | KMP SDK, Web SDK |
| `replay.json` | LAR1 parça: mantıksal ↔ hex | KMP SDK, Web SDK (kodlama); UI oynatıcı (çözme) |

## `wire.json`

```
valid[]    { name, batch, hex }       encode(batch) == hex  ve  decode(hex) == batch
tolerant[] { name, hex, batch }       decode(hex) == batch  (yeniden kodlama farklı olabilir)
invalid[]  { name, hex, frame_ok, header_ok, bad_records, http_status }
```

Mantıksal batch:

```json
{ "batch_id": "<hex32>", "sent_at": 1795046400123, "sdk": "kmp/0.1.0", "anon_id": "…", "user_id": "",
  "session_id": "<hex32; 32 sıfır = yok>",
  "context": [ { "k": "platform", "t": "string", "v": "android" } ],
  "records": [ { "kind": 0, "uuid": "<hex32>", "ts": 1795046399000, "name": "app_opened", "props": [] } ] }
```

Özellik `t`/`v`: `null` → `null` · `bool` → `true/false` · `int` → **ondalık string** (int64 uçları için) · `float` → ondalık string (`"NaN"`, `"Infinity"`, `"-Infinity"` dahil; NaN kanonik `0x7ff8000000000000`) · `string` → string · `json` → JSON metni (string).

`invalid[]` alanları:
- `frame_ok = false` → ingestion sıcak yolu reddeder (`http_status`: 400 ya da 413).
- `frame_ok = true` → ingestion `200` döner; tam çözücü `header_ok = false` ise tüm batch'i, aksi halde yalnızca `bad_records` indislerindeki kayıtları reddeder (diğer kayıtlar geçerlidir).

## `flags.json`

`{ ruleset, cases[] { flag, props, user_id, anon_id, expect } }` — `expect`: `null`, `true` ya da varyant anahtarı. `user_id: ""` = tanımsız kullanıcı.

## `replay.json`

`valid[] { name, chunk, hex }`, `tolerant[] { name, hex, chunk }`. Düğümde `color`/`text` alanları yalnızca ilgili `flags` biti kuruluysa bulunur.
