# Control plane API v1

Kaynak: `docs/design.md` §14. Control plane; kullanıcı, proje, token, flag, kayıtlı sorgu ve silme isteklerini yönetir, değişiklikleri object storage'a **anlık görüntü** olarak yayınlar (`manifest.md` §8, `flags.md` §1) ve UI için sorgu servisine vekillik eder. Veri düzlemi control plane'e bağlanmaz.

Tüm uçlar JSON konuşur. Hata gövdesi: `{"error": {"code": "…", "message": "…"}}`. Zaman damgaları unix ms.

## 1. Kimlik doğrulama

| Yol | Açıklama |
|---|---|
| `GET /api/setup` | `{ "needs_setup": bool }` — hiç kullanıcı yoksa `true` |
| `POST /api/setup` | Yalnızca kullanıcı yokken: `{email, password, org_name}` → ilk `owner` + org; oturum açar |
| `POST /api/auth/login` | `{email, password, totp?}` → oturum çerezi. Hatalı: `401`. TOTP gerekli ama yok: `401` + `code: "totp_required"` |
| `POST /api/auth/logout` | Oturumu siler |
| `GET /api/me` | `{ user: {id, email, totp_enabled}, orgs: [{id, name, role}] }` |
| `POST /api/me/password` | `{current, new}` |
| `POST /api/me/totp/begin` → `{secret, otpauth_url}` · `POST /api/me/totp/enable` `{code}` · `POST /api/me/totp/disable` `{password}` | TOTP (RFC 6238, SHA-1, 30 sn, 6 hane) |

- Parola: argon2id (`m=64MiB, t=3, p=1` ya da daha güçlü), en az 10 karakter.
- Oturum çerezi: `la_session`, `HttpOnly`, `SameSite=Lax`, `Secure` (TLS arkasında), 256 bit rastgele; sunucuda yalnızca SHA-256'sı saklanır; 30 gün kayan süre.
- CSRF: çerezle gelen tüm durum değiştiren isteklerde `X-LA-CSRF` başlığı zorunludur; değer `GET /api/me` yanıtındaki `csrf` alanıdır (oturuma bağlı).
- Giriş denemeleri IP + e-posta başına sınırlanır (5 / dk).
- API anahtarı ile erişim: `Authorization: Bearer sk_…` (`admin` kapsamlı) yalnızca kendi projesinin `/api/projects/{pid}/…` uçlarına; çerez/CSRF gerekmez.
- OIDC: v1'de yok (belgelenmiş eksik); kullanıcı tablosu `oidc_subject` sütunu taşır.

Roller (org düzeyinde): `owner` > `admin` > `member` > `viewer`.

| İşlem | En düşük rol |
|---|---|
| Okuma (projeler, flag'ler, insight'lar, sorgu vekili) | `viewer` |
| Insight/dashboard yazma | `member` |
| Flag yazma, token/anahtar yönetimi, proje ayarları, silme isteği | `admin` |
| Üye yönetimi, org silme | `owner` |

Yetkisiz: `403`. Başka org'un kaynağı: `404` (varlığı sızdırılmaz).

## 2. Org ve üyeler

| Yol | |
|---|---|
| `GET /api/orgs/{org}/members` | `[{user_id, email, role, created_at}]` |
| `POST /api/orgs/{org}/members` | `{email, role, password}` — kullanıcı yoksa oluşturur (ilk parola yönetici tarafından verilir; e-posta gönderimi v1'de yok) |
| `PATCH /api/orgs/{org}/members/{user_id}` | `{role}` — son `owner` düşürülemez |
| `DELETE /api/orgs/{org}/members/{user_id}` | |
| `GET /api/orgs/{org}/audit?limit=&before=` | `[{id, ts, actor, action, target, detail}]` — yeni → eski |

## 3. Projeler

`Project`: `{ id, org_id, name, retention_days, replay_retention_days, drop_keys: [string], scan_limit_bytes, max_concurrent_queries, replay_sample_rate, created_at }`

| Yol | |
|---|---|
| `GET /api/projects` | Kullanıcının tüm org'larındaki projeler |
| `POST /api/projects` | `{org_id, name}` → `Project` (+ otomatik bir `pk_` token) |
| `GET/PATCH/DELETE /api/projects/{pid}` | `PATCH` yukarıdaki ayar alanlarını alır. `DELETE` projeyi **kapatır**: token'lar `disabled` olur, veri saklama süresi 0'a çekilir (compactor siler) |

`id` 1'den artan `u32`'dir ve asla yeniden kullanılmaz.

## 4. Yazma token'ları (`pk_`) ve gizli anahtarlar (`sk_`)

`Token`: `{ token: "pk_…", name, status: "active"|"disabled", allowed_origins: [string], rate_limit, replay: bool, created_at }`

| Yol | |
|---|---|
| `GET/POST /api/projects/{pid}/tokens` | `POST {name, allowed_origins?, rate_limit?, replay?}` |
| `PATCH/DELETE /api/projects/{pid}/tokens/{token}` | `PATCH {name?, status?, allowed_origins?, rate_limit?, replay?}`. `DELETE`: token yayından kalkar (ingestion `401` döner) ve `flags/<token>.json` silinir |

`Key`: `{ id, name, scopes: ["query"|"admin"], prefix: "sk_…abcd", created_at, last_used_at }`

| Yol | |
|---|---|
| `GET/POST /api/projects/{pid}/keys` | `POST {name, scopes}` → `Key` + **bir kez** `secret: "sk_…"` |
| `DELETE /api/projects/{pid}/keys/{id}` | |

Biçim: `pk_` / `sk_` + 32 karakter base62 (≥ 190 bit). `sk_` yalnızca SHA-256 olarak saklanır.

## 5. Feature flag'ler

`Flag`: `{ key, name, description, enabled, salt, rules: [Rule], default, version, updated_at, updated_by }` — `rules`/`default` biçimi `flags.md` §1.

| Yol | |
|---|---|
| `GET/POST /api/projects/{pid}/flags` | `POST {key, name?, description?, enabled?, rules?, default?}`; `salt` sunucuda üretilir ve **değişmez** (dağılım kararlılığı) |
| `GET/PUT/DELETE /api/projects/{pid}/flags/{key}` | `PUT` tüm tanımı değiştirir; her yazım `flag_versions`'a eklenir |
| `GET /api/projects/{pid}/flags/{key}/versions` | `[{version, ts, actor, flag}]` |
| `POST /api/projects/{pid}/flags/{key}/evaluate` | `{props, user_id, anon_id}` → `{value}` — sunucu tarafı deneme (UI önizlemesi); `flags.md` §3 ile aynı sonuç (`spec/vectors/flags.json`'ı geçer) |

Doğrulama: `key` `^[a-z0-9][a-z0-9_-]{0,63}$`; `rollout` 0–10000; varyant ağırlıkları toplamı 10000; operatörler `flags.md` §4 listesinden; `regex` derlenebilir olmalı.

Her flag değişikliğinde projenin **her** aktif token'ı için `flags/<token>.json` yeniden yazılır (`v` = proje flag kümesi sürümü, her yayında +1).

## 6. Insight ve dashboard

`Insight`: `{ id, name, kind: "trends"|"funnel"|"retention"|"sql", query: <spec/query.md istek gövdesi>, created_by, updated_at }`. Göreli tarih için `query.from/to` yerine `query.range: "7d"|"30d"|"90d"|…` saklanabilir; vekil çağrıdan önce UI somut tarihe çevirir.

`Dashboard`: `{ id, name, items: [{insight_id, x, y, w, h}], updated_at }`

| Yol | |
|---|---|
| `GET/POST /api/projects/{pid}/insights` · `GET/PUT/DELETE /api/projects/{pid}/insights/{id}` | |
| `GET/POST /api/projects/{pid}/dashboards` · `GET/PUT/DELETE /api/projects/{pid}/dashboards/{id}` | |

## 7. Silme istekleri (KVKK/GDPR)

`Deletion`: `{ id: "del_…", user_ids: [string], anon_ids: [string], status: "pending"|"done", created_at, created_by, note }`

| Yol | |
|---|---|
| `GET/POST /api/projects/{pid}/deletions` | `POST {user_ids?, anon_ids?, note?}` → `tombstones/p=<pid>/<ts>-<id>.json` yazar (`person_keys = ["u:"+id…]`) |

`status`, tombstone'un `ts_ms`'i projenin tüm gün manifest'lerinde `tombstone_ts ≥ ts_ms` olduğunda (ya da 8 gün sonra) `done` olur; control plane bunu periyodik olarak denetler.

## 8. Sorgu vekili

`POST /api/projects/{pid}/q/{trends|funnel|retention|events|sql|schema}` ve `GET /api/projects/{pid}/q/replay?key=…` — gövde/yanıt `spec/query.md` ile aynı. Control plane rolü denetler, sonra `LA_QUERY_URL`'e `X-LA-Internal` + `X-LA-Project` başlıklarıyla iletir. İstemciden gelen `X-LA-*` başlıkları **atılır**.

## 9. Yayınlama

| Dosya | Tetikleyici |
|---|---|
| `control/tokens.json` | token/proje değişikliği |
| `control/keys.json` | anahtar değişikliği |
| `control/projects.json` | proje ayarı değişikliği |
| `flags/<token>.json` | flag ya da token değişikliği |
| `tombstones/…` | silme isteği |

- Her yayın, veritabanındaki **tüm** durumu yeniden üretir (artımlı değil); `version` alanı tekdüze artar.
- Değişiklikten sonra ≤ 2 sn içinde yayınlanır; ayrıca 5 dk'da bir tam yeniden yayın (elle silinen dosyaya karşı).
- Yayın başarısızsa API isteği **başarılı** kalır (veritabanı doğruluk kaynağıdır), yayın arka planda yeniden denenir ve `la_control_publish_lag_seconds` artar.

## 10. Diğer

- `GET /healthz`, `/readyz`, `/metrics` (`la_control_` önekli).
- `LA_UI_DIR` ayarlıysa `/` altında statik SPA sunulur (bilinmeyen yollar `index.html`'e düşer; `/api/` hariç).
- Güvenlik başlıkları: `Content-Security-Policy` (yalnızca kendi kökeni), `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, TLS arkasında `Strict-Transport-Security`.
- Tüm yönetim işlemleri `audit_log`'a yazılır: `{ts, actor (user id | key id), action ("project.create", "flag.update", …), target, detail (JSON; sır içermez)}`.

## 11. Yapılandırma

```
LA_CONTROL_LISTEN=:8090
LA_CONTROL_DB=/var/lib/la/control.db      # SQLite (WAL modu)
LA_QUERY_URL=http://127.0.0.1:8081
LA_QUERY_INTERNAL_SECRET=…                 # query servisiyle paylaşılan sır
LA_UI_DIR=/usr/share/la/ui
LA_COOKIE_SECURE=true
LA_STORE_URL / LA_S3_*                     # manifest.md §0
```
