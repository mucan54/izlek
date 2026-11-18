# Feature flag kural seti ve değerlendirme

Kaynak: `docs/design.md` §12. Vektörler: `spec/vectors/murmur3.json`, `spec/vectors/flags.json`.

## 1. Dosya

`flags/<token>.json` — control plane yazar; CDN/ingestion dışı statik sunum. `ETag` + `Cache-Control: max-age=60`.

```json
{
  "v": 31,
  "flags": [
    {
      "key": "new_checkout",
      "enabled": true,
      "salt": "k3f9",
      "rules": [
        {
          "when": { "and": [
            { "prop": "platform", "op": "eq", "value": "ios" },
            { "prop": "app_version", "op": "semver_gte", "value": "3.2.0" }
          ]},
          "rollout": 2500,
          "variants": [ { "key": "control", "w": 5000 }, { "key": "b", "w": 5000 } ],
          "stable_on": "anon_id"
        }
      ],
      "default": null
    }
  ]
}
```

SDK indirme adresi: `<flagsHost>/flags/<token>.json` (`flagsHost` varsayılanı `host`).

## 2. Sonuç tipi

Bir flag'in değeri: `null` (kapalı/eşleşme yok ve `default` null), `true` (varyantsız açık) ya da varyant anahtarı (string). `default` alanı `null`, `true`, `false` ya da string olabilir; `false` ≡ `null`.

## 3. Algoritma

```
eval(flag, props, user_id, anon_id):
  eğer !flag.enabled: dön flag.default
  her rule için (sırayla):
    eğer rule.when var ve !match(rule.when, props): devam
    id = (rule.stable_on == "anon_id") ? anon_id : (user_id boş değilse user_id, yoksa anon_id)
    rollout = rule.rollout yoksa 10000
    b = murmur3_32(utf8(flag.salt + ":" + flag.key + ":" + id), seed=0) mod 10000
    eğer b >= rollout: dön flag.default        # eşleşen İLK kural karar verir
    eğer rule.variants boş/yok: dön true
    v = murmur3_32(utf8(flag.salt + ":v:" + flag.key + ":" + id), seed=0) mod 10000
    kümülatif = 0
    her variant için: kümülatif += w ; eğer v < kümülatif: dön variant.key
    dön son variant.key                         # ağırlıklar 10000'e tamamlanmıyorsa
  dön flag.default
```

`murmur3_32`: MurmurHash3 x86_32, seed 0, girdinin UTF-8 baytları üzerinde; sonuç işaretsiz 32 bit.

## 4. Koşullar

```
Cond := {"and": [Cond…]} | {"or": [Cond…]} | {"not": Cond}
      | {"prop": string, "op": Op, "value": any}
```
Boş `and` → doğru; boş `or` → yanlış.

`props`: SDK'nın bildiği düz anahtar-değer kümesi; öncelik (soldan sağa ezilir): otomatik context → super props (`register`) → kişi özellikleri (`identify`/`setPersonProps`). Ek sanal anahtarlar: `user_id`, `anon_id`.

| Op | Anlam (prop değeri `p`, kural değeri `v`) |
|---|---|
| `is_set` | `p` var ve null değil (`v` yok sayılır) |
| `eq` / `ne` | Gevşek eşitlik: ikisi de sayıysa sayısal; aksi halde `str(p) == str(v)`. `ne` = `!eq`. |
| `in` / `not_in` | `v` dizi; herhangi bir öğeyle `eq`. `not_in` = `!in`. |
| `gt` `gte` `lt` `lte` | İkisi de sayıya çevrilebiliyorsa (sayı ya da `^-?[0-9]+(\.[0-9]+)?$` kalıbına uyan string; boolean **değil**) sayısal karşılaştırma; aksi halde **yanlış** |
| `contains` | `str(p)`, `str(v)`'yi alt dizi olarak içerir (büyük/küçük harf duyarlı) |
| `regex` | `str(v)` kalıbı `str(p)` içinde eşleşir (çapasız arama). Yalnızca ortak alt küme kullanılmalıdır: karakter sınıfları, `^ $ . * + ? | ( )`, `{m,n}`. Geçersiz kalıp → yanlış. |
| `semver_gte` / `semver_lt` | `MAJOR[.MINOR[.PATCH]]` sayısal üçlü karşılaştırması; baştaki `v` ve `-`/`+` sonrası ek atılır; eksik parça 0. Ayrıştırılamazsa **yanlış**. |

`p` yoksa/null ise: `is_set` yanlış, `ne` ve `not_in` **doğru**, diğer tüm operatörler yanlış.

`str(x)`: string → kendisi; `true/false` → `"true"/"false"`; tam sayı değerli sayı → ondalık, üssüz, kesirsiz (`3.0` → `"3"`); diğer sayılar → en kısa gidiş-dönüş ondalık gösterimi.

## 5. SDK davranışı

- `flag(key, default)`: kural seti yoksa `default`. Flag yoksa `default`. Sonuç `null` ise `default`.
- İlk çağrıda **oturum başına bir kez** `flag_exposed` event'i: `props = { "flag": key, "variant": <"true" | varyant | "false">, "ruleset_v": v }`.
- Yenileme: açılışta ve ön plandayken 5 dk'da bir, `If-None-Match` ile. Son iyi kopya diskte/`localStorage`'da tutulur.

## 6. PostHog uyum uçları

`/decide` ve `/flags` (her yöntem, her sorgu parametresi) `200` ve şu gövdeyi döner:

```json
{ "config": { "enable_collect_everything": false }, "toolbarParams": {}, "isAuthenticated": false,
  "supportedCompression": ["gzip", "gzip-js"], "featureFlags": {}, "featureFlagPayloads": {},
  "flags": {}, "errorsWhileComputingFlags": false, "sessionRecording": false,
  "surveys": false, "capturePerformance": false, "autocapture_opt_out": true }
```

PostHog capture uçları (`/batch`, `/i/v0/e`, `/e`, `/capture`, sonda `/` ile de) başarıda `200 {"status": 1}` döner.
