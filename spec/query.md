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

Aynı `uuid`, compaction öncesinde birden çok dosyada bulunabilir (node'lar arası yeniden gönderim, plan yeniden yürütme). Sorgu katmanı bir günün okunan akış sayısı > 1 ise o günü `(person_key, ts, uuid)` sıralı birleştirir ve **aynı kişinin daha önce görülmüş bir `uuid`'sini taşıyan satırları atar**; sıra gereği kalan kopya en erken `ts`'lidir. Bu, compaction'ın uyguladığı kuralın aynısıdır: bir gün compaction'dan önce ve sonra aynı okunur. Tek dosyalı günlerde (L2) ek maliyet yoktur; bellek kullanımı bir kişinin o gündeki satır sayısıyla sınırlıdır.

Kopyalar çoğunlukla bire bir aynıdır. Farklı `ts` taşıyan kopya yalnızca şu üç koşul birlikteyken oluşur: cihaz saati 5 sn'den fazla kaymış (`wire.md` §5 düzeltmesi devrede), batch'in ilk gönderimi kalıcı olmuş ama yanıtı istemciye ulaşmamış, ve yeniden gönderim ingestion'ın `batch_id` LRU'suna takılmamış (başka node, süresi dolmuş kayıt). Düzeltilmiş `ts` iki denemede birkaç ms farklı çıkar.

**Bilinen sınır.** Satır filtreleri tekilleştirmeden **önce** uygulanır (dosya ve sayfa budaması için). `ts`'ye bağlı bir koşul farklı `ts`'li iki kopyayı ayırırsa geç kopya tek başına kalır ve görünür. Bu yalnızca compaction öncesinde ve şu durumlarda olur: gün içi (RFC 3339) `from`/`to` sınırı tam iki kopyanın arasına düşerse event aralığa geç `ts`'iyle girer; `/q/events` sayfalamasında imleç iki kopyanın arasına düşerse event sonraki sayfada bir kez daha listelenir. Gün sınırlı aralıklar, trend kovaları, funnel ve retention etkilenmez. İki kopya farklı UTC günlerine düşerse (gece yarısı çevresinde birkaç ms) compaction da birleştirmez.

## 11. Netleştirmeler (uygulamanın seçtiği davranışlar)

- **Gövde**: bilinmeyen alan `400` (yazım hatası sorguyu sessizce değiştirmesin). `> 1 MiB` gövde `413`.
- **Funnel**: `window_seconds` varsayılanı 7 gün; `breakdown_limit` varsayılanı 1000; 50.000'den fazla kırılım değeri `400`. `any` sırasında tek event birden çok adımı sağlayabilir, kırılım değeri çapa event'ten alınır, `median_seconds[k]` = `k−1` ve `k` farklı adıma ilk ulaşma anları arasındaki süre. Medyan adım başına 4096 örneğe (kırılım değeri başına 256) kadar kesindir, ötesinde t-digest.
- **Trends**: kırılım istenmediyse seride `breakdown` anahtarı yoktur (`null` = null kovası). İlk N, seri başına eşleşen satır sayısına (`sum` için toplama) göre seçilir; `avg` için `$other` ağırlıklı ortalamadır.
- **Retention**: aralıktaki tüm dönemler listelenir (boş kohortlar dahil). Varsayılanlar: `period = day`, `periods = 8`, `return` = herhangi bir event.
- **Events**: varsayılan `order = desc`, `limit = 100`. `cursor` projeye ve sorguya bağlıdır; başka sorguda `400`.
- **SQL**: Boolean → `"true"`/`"false"`; satır sınırına takılan yanıtta `"truncated": true`; özyinelemeli CTE kapalı. `events.props` üzerindeki `prop*()` çağrıları, anahtar terfi edilmişse `coalesce(p_<anahtar>, …)` olarak yeniden yazılır (terfi sonuçları değiştirmez).
- **Sınırlar**: bellek bütçesi aşımı `422 memory_limit`; süre sınırı kuyruk beklemesini de kapsar; tarama sınırı gerçekten okunan sütun baytlarına uygulanır (footer'lar hariç).
- **Replay**: yabancı önek `403`, kendi öneki altında bozuk anahtar `400`, olmayan nesne `404`.
- **İç başlık**: `X-LA-Internal` yanlışsa ya da sır yapılandırılmamışsa, geçerli bir `Bearer` olsa bile `401`. `X-LA-Project` sırsız gelirse `401`. Kapatılmış proje `403`.
- **Önbellek**: anahtar şema sürümünü de içerir; kişi tablosu sürümü yalnızca sorgu kişi verisi kullanıyorsa girer. Önbellekten dönen yanıtta `bytes_scanned = 0`. Önbellek ve eşzamanlılık sınırları süreç başınadır (replikalar arasında paylaşılmaz).
- **Tombstone**: silinen kişi `persons` tablosunda ve `/q/schema` kişi özelliklerinde de görünmez. Kişinin replay nesneleri compactor silene kadar depoda durur (indeks event'i gizlendiği için listelenmez).
- **Sıra**: tarama `(person_key, ts, uuid)` sırasını bildirir (`docs/design.md` §11.1'in üst kümesi).
- **Yapılmadı (v1.1)**: `/q/paths`, ön toplamalar.
