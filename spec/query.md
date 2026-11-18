# Sorgu API'si v1

Kaynak: `docs/design.md` §11. Tüm uçlar `POST` + JSON'dur (aksi belirtilmedikçe); yanıtlar JSON'dur.

## 1. Kimlik ve izolasyon

- `Authorization: Bearer sk_…` → `control/keys.json` içinde `sha256(sk)` aranır; `query` kapsamı gerekir.
- İç çağrı (control plane/UI vekili): `X-LA-Internal: <LA_QUERY_INTERNAL_SECRET>` + `X-LA-Project: <pid>`. Sır yapılandırılmamışsa bu yol **kapalıdır**.
- Proje kimliği yalnızca bu iki kaynaktan gelir. İstek gövdesindeki hiçbir alan proje seçemez.
- Hatalar: `401` (kimlik yok/geçersiz), `403` (kapsam yok), `400` (geçersiz sorgu), `408` (süre aşımı), `422` (taranan bayt sınırı), `429` (kuyruk dolu).

Hata gövdesi: `{"error": {"code": "bad_request", "message": "…"}}`.

## 2. Ortak alanlar

```json
{ "from": "2026-11-01", "to": "2026-11-18", "filter": Expr }
```

- `from`/`to`: `YYYY-MM-DD` (UTC günü; `to` **dahil**) ya da RFC 3339 zaman damgası (`to` hariç). En çok 400 gün.
- Tüm zaman kovaları v1'de UTC'dir.

### İfade ağacı

```
Expr := {"and": [Expr…]} | {"or": [Expr…]} | {"not": Expr}
      | {"prop": Ref, "op": Op, "value": any}
Op   := eq | ne | in | not_in | gt | gte | lt | lte | contains | is_set
Ref  := sabit sütun adı | "props.<anahtar>" | "person.<anahtar>"
```

Sabit sütunlar: `event`, `person_key`, `anon_id`, `user_id`, `session_id` (hex32), `platform`, `os`, `os_version`, `device_model`, `device_type`, `app_version`, `locale`, `country`, `sdk`, `url_path`, `referrer_host`, `screen`.

Anlam (flag koşullarıyla aynı gevşek tipleme, `flags.md` §4): değer yok/null ise `is_set` yanlış, `ne`/`not_in` doğru, diğerleri yanlış. `eq`: ikisi de sayıysa sayısal, değilse metin karşılaştırması. `gt…lte`: yalnızca ikisi de sayıya çevrilebiliyorsa.

`props.x`: `parquet.md` §3. `person.x`: kişinin **güncel** özelliği (`manifest.md` §4).

## 3. `POST /q/trends`

```json
{ "from": "…", "to": "…", "interval": "day",
  "series": [ { "event": "purchase", "agg": "count", "label": "Satın alma" },
              { "event": null, "agg": "unique" },
              { "event": "purchase", "agg": "sum", "prop": "props.amount", "filter": Expr } ],
  "filter": Expr, "breakdown": "platform", "breakdown_limit": 25 }
```

- `interval`: `hour` | `day` | `week` (Pazartesi başlangıçlı) | `month`.
- `agg`: `count` | `unique` (tekil `person_key`) | `sum` | `avg` | `min` | `max` (son dördü `prop` ister; sayıya çevrilemeyen değerler atlanır).
- `event: null` → tüm event'ler.
- `breakdown`: bir `Ref`. Değerler metne çevrilir; null → `null` kovası. Toplamı en büyük `breakdown_limit` değer döner, kalanı `"$other"` altında toplanır (`unique` için `$other` üretilmez).

```json
{ "buckets": ["2026-11-01T00:00:00Z", "…"],
  "series": [ { "index": 0, "label": "Satın alma", "breakdown": "ios", "values": [12, 0, 7] } ],
  "meta": Meta }
```

Boş kovalar `0` (`avg/min/max` için `null`) ile doldurulur. `unique` kova içinde tekildir (kovalar arası toplanamaz).

## 4. `POST /q/funnel`

```json
{ "from": "…", "to": "…",
  "steps": [ { "event": "signup" }, { "event": "purchase", "filter": Expr } ],
  "window_seconds": 604800, "order": "ordered", "filter": Expr, "breakdown": "platform" }
```

2 ≤ adım ≤ 20. `order`: `ordered` (varsayılan) | `strict` | `any`.

**Anlam (`ordered`)** — kişinin, genel `filter`'ı geçen event'leri `(ts, uuid)` sırasıyla `e_1 … e_n` olsun. Kişi adım `k`'ye (1 tabanlı) ulaşmıştır ⇔ `i_1 < i_2 < … < i_k` indisleri vardır ki `e_{i_j}` adım `j` koşulunu sağlar ve `ts(e_{i_k}) − ts(e_{i_1}) ≤ window_seconds × 1000`. (Aynı milisaniyedeki iki **farklı** satır ardışık adımları sağlayabilir; aynı satır iki adım sayılmaz.)

**`strict`**: ek olarak `i_{j+1} = i_j + 1` (araya filtreyi geçen başka event girmez).

**`any`**: kişi `k`'ye ulaşmıştır ⇔ bir çapa event `e_a` (herhangi bir adımı sağlayan) vardır ki `[ts(e_a), ts(e_a) + W]` aralığındaki event'ler (konumu ≥ a) en az `k` **farklı** adımı sağlar.

**Kırılım**: değer, zinciri başlatan adım-1 event'inden alınır; durum kırılım değeri başına tutulur; kişi, adım-1 yaptığı her kırılım değerinde ayrı sayılır.

```json
{ "steps": [ { "event": "signup", "count": 1200, "median_seconds": null },
             { "event": "purchase", "count": 310, "median_seconds": 5400.0 } ],
  "breakdowns": [ { "value": "ios", "steps": [ … ] } ],
  "meta": Meta }
```

`count[k]` = en az adım `k`'ye ulaşan kişi sayısı. `median_seconds[k]`: adım `k`'ye ilk ulaşıldığı andaki `ts(e_{i_k}) − ts(e_{i_{k−1}})` değerlerinin medyanı (t-digest; yaklaşık).

## 5. `POST /q/retention`

```json
{ "from": "…", "to": "…", "period": "week", "periods": 8,
  "start": { "event": "signup", "filter": Expr }, "return": { "event": null }, "filter": Expr }
```

- Kişinin kohortu = `[from, to]` içindeki **ilk** `start` event'inin (`t0`) düştüğü dönem `c`.
- `returned[k]` (0 ≤ k < `periods`) = kohorttaki kişilerden, `ts ≥ t0` olan ve dönem `c + k`'ye düşen en az bir `return` event'i olanların sayısı. (`return` = `start` ise `returned[0] = size`.)
- `period`: `day` | `week` | `month`. `periods` ≤ 64.

```json
{ "cohorts": [ { "period_start": "2026-11-02T00:00:00Z", "size": 420, "returned": [420, 130, 88] } ],
  "meta": Meta }
```

`returned` dizisi, `to`'yu aşan dönemler için kısalır.

## 6. `POST /q/events`

```json
{ "from": "…", "to": "…", "filter": Expr, "limit": 100, "order": "desc", "cursor": "…" }
```

`limit` ≤ 1000. Sıra `(ts, uuid)`. `cursor` opak; yanıttaki `next_cursor` (yoksa null) ile devam edilir.

```json
{ "events": [ { "uuid": "0190…", "ts": 1795046400000, "recv_ts": 1795046400123, "event": "purchase",
                "person_key": "u:42", "anon_id": "…", "user_id": "42", "session_id": "…|null",
                "platform": "ios", "os": "…", "os_version": "…", "device_model": "…", "device_type": "…",
                "app_version": "…", "locale": "…", "country": "TR", "sdk": "kmp/0.1.0",
                "url_path": null, "referrer_host": null, "screen": "Home", "flags": 0,
                "props": { "amount": 12.5 } } ],
  "next_cursor": null, "meta": Meta }
```

`props` içinde terfi edilmiş sütun değerleri de yer alır (mantıksal görünüm).

## 7. `POST /q/sql`

```json
{ "from": "…", "to": "…", "sql": "SELECT event, count(*) FROM events GROUP BY 1 ORDER BY 2 DESC LIMIT 10" }
```

- Tek bir `SELECT` (CTE ve alt sorgu serbest). DDL/DML, `COPY`, `SET`, `EXPLAIN ANALYZE`, dosya/URL tablo fonksiyonları ve katalog/`information_schema` erişimi **yasaktır** (plan üzerinde izin listesi).
- Tablolar: `events` (`parquet.md` şeması; `[from, to]` ile sınırlı, tombstone ve kimlik çözümü uygulanmış), `persons` (`person_key`, `props` JSON).
- Fonksiyonlar: yerleşik skaler/toplam fonksiyonlar + `prop(props, 'k')` (metin), `prop_num(props, 'k')` (float), `prop_bool(props, 'k')`, `hex(uuid)`.
- Sonuç en çok 10.000 satır.

```json
{ "columns": [ { "name": "event", "type": "Utf8" } ], "rows": [ ["purchase", 12] ], "meta": Meta }
```

Tür eşlemesi: Int/UInt/Float → JSON sayı (güvenli aralık dışı Int64 → string), Timestamp → unix ms, Binary → hex, diğerleri string.

## 8. Diğer uçlar

| Uç | Amaç |
|---|---|
| `GET /q/replay?key=replay/p=<pid>/…` | Replay parçasını ham (sıkıştırılmış) döner; `Content-Encoding` ayarlanır. Anahtar, isteğin projesinin önekiyle başlamıyorsa `403`. |
| `POST /q/schema` | `{ "events": [adlar…], "props": [{key, type, promoted}], "person_props": [anahtarlar…] }` — UI otomatik tamamlama için (son 7 gün şema istatistiklerinden). |
| `GET /healthz`, `/readyz`, `/metrics` | — |

## 9. `Meta`

```json
{ "elapsed_ms": 42, "bytes_scanned": 1048576, "files": 12, "cached": false,
  "manifests": { "2026-11-18": 42 } }
```

Sonuç önbelleği anahtarı = `sha256(proje ‖ uç ‖ kanonik istek JSON'u ‖ gün→manifest sürümleri ‖ kimlik/kişi manifest sürümleri ‖ tombstone listesi özeti)`.

## 10. Tekilleştirme

Aynı `uuid`, compaction öncesinde birden çok dosyada bulunabilir (node'lar arası yeniden gönderim, plan yeniden yürütme). Sorgu katmanı bir günün dosya sayısı > 1 ise o günü `(person_key, ts, uuid)` sıralı birleştirir ve **bitişik eşit `(person_key, ts, uuid)`** satırlarından yalnızca birini geçirir. Tek dosyalı günlerde (L2) ek maliyet yoktur.
