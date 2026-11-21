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
| `GET /api/me/connected-apps` · `DELETE /api/me/connected-apps/{id}` | Kullanıcının OAuth ile bağladığı uygulamalar ve iptali (§13.4) |
| `GET/POST /api/me/tokens` · `DELETE /api/me/tokens/{id}` | Kişisel erişim token'ları (§14) |

- Parola: argon2id (`m=64MiB, t=3, p=1` ya da daha güçlü), en az 10 karakter.
- Oturum çerezi: `la_session`, `HttpOnly`, `SameSite=Lax`, `Secure` (TLS arkasında), 256 bit rastgele; sunucuda yalnızca SHA-256'sı saklanır; 30 gün kayan süre.
- CSRF: çerezle gelen tüm durum değiştiren isteklerde `X-LA-CSRF` başlığı zorunludur; değer `GET /api/me` yanıtındaki `csrf` alanıdır (oturuma bağlı).
- Giriş denemeleri IP + e-posta başına sınırlanır (5 / dk).
- API anahtarı ile erişim: `Authorization: Bearer sk_…` (`admin` kapsamlı) yalnızca kendi projesinin `/api/projects/{pid}/…` uçlarına; çerez/CSRF gerekmez.
- OIDC: v1'de yok (belgelenmiş eksik); kullanıcı tablosu `oidc_subject` sütunu taşır.
- MCP ucu (`/mcp`) aynı `sk_` anahtarını ya da control plane'in kendi verdiği OAuth erişim token'ını kabul eder (§13).

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
LA_MCP_ENABLED=true                        # /mcp, /oauth/*, /.well-known/oauth-* (§13)
LA_PUBLIC_URL=https://izlek.example.com    # control plane'in genel kökeni; OAuth için zorunlu
LA_PUBLIC_INGEST_URL=https://in.example.com  # get_sdk_setup parçacıklarındaki ingestion adresi
LA_MCP_ALLOWED_ORIGINS=                    # /mcp'yi tarayıcıdan çağırabilecek ek kökenler (virgüllü)
LA_OAUTH_CLIENT_REGISTRATION=cimd,dcr      # OAuth istemcisi tanıtma yolları
```

## 12. Netleştirmeler (UI ile backend arasında bağlayıcı)

- `GET /api/me` yanıtı `csrf` alanını da taşır. `X-LA-CSRF`, oturum çereziyle gelen **her** GET-dışı istekte zorunludur (`/q/*` vekil POST'ları, `evaluate` ve logout dahil); `login` ve `setup` muaftır.
- Liste uçları çıplak JSON dizisi döner. Silme uçları boş gövdeyle `200`/`204` dönebilir.
- `401` yalnızca "oturum yok/bitti" demektir. Yanlış mevcut parola (`/api/me/password`, `/api/me/totp/disable`) → `403`; hatalı TOTP kodu (`/api/me/totp/enable`) → `400`.
- Rol tablosunda geçmeyen okumalar: üye listesi = org'un her üyesi; denetim kaydı = `admin`+; token listesi = `viewer`; anahtar listesi = `admin`; silme isteği listesi = `admin`.
- `POST /api/projects` yalnızca `Project` döner (otomatik token `/tokens`'tan okunur). Kapatılmış projeler `GET /api/projects`'te görünmez.
- Denetim kaydı sayfalaması: `before` = alınan son satırın `id`'si (hariç).
- Flag `PUT` gövdesi `{key, name, description, enabled, rules, default}`'tir; `salt` ve `version` istemciden alınmaz. `default: false` ≡ `null`.
- Insight `query` alanı **opak** JSON olarak saklanır (`range`, `ui` gibi sorgu servisinin bilmediği alanlar içerebilir); control plane içini doğrulamaz. Dashboard öğeleri 12 sütunlu ızgarada tam sayı `x, y, w, h` taşır.
- Kimlikler (kullanıcı, org, insight, dashboard, anahtar) istemci için opaktır.

## 13. MCP sunucusu ve OAuth yetkilendirme sunucusu

Kullanım kılavuzu (İngilizce): `docs/mcp.md`. Bu bölüm sözleşmedir. `LA_MCP_ENABLED=false` ise bu bölümdeki hiçbir uç yoktur (404).

### 13.1 `POST /mcp`

Model Context Protocol, Streamable HTTP taşıması. Her istek tek bir JSON-RPC iletisidir; her yanıt tek bir `application/json` nesnesidir. Sunucu olay akışı açmaz, oturum tutmaz (`Mcp-Session-Id` vermez); `GET`/`DELETE` → `405`.

| Revizyon | Davranış |
|---|---|
| `2026-07-28` | Durumsuz. İstek `_meta["io.modelcontextprotocol/protocolVersion"]` ve `…/clientCapabilities` taşır; `MCP-Protocol-Version` başlığı gövdeyle aynı olmalı, `Mcp-Method` (ve `tools/call` için `Mcp-Name`) gövdeyi yansıtmalıdır; uyuşmazlık `400` + `-32020`. `server/discover` vardır. Sonuçlar `resultType: "complete"` ve `_meta` içinde `serverInfo` taşır; `tools/list` ayrıca `ttlMs`, `cacheScope: "private"`. Bilinmeyen yöntem `404` + `-32601`, geçersiz parametre `400` + `-32602`. |
| `2025-11-25`, `2025-06-18`, `2025-03-26` | `initialize` el sıkışması: istenen revizyon destekleniyorsa aynısı, değilse `2025-11-25` döner. Sonraki isteklerde `MCP-Protocol-Version` (yoksa `2025-03-26` sayılır). `ping`, `notifications/*` (`202`). JSON-RPC hataları `200` ile döner. Toplu (batch) istek kabul edilmez. |

Desteklenmeyen revizyon: `400` + `-32022`, `data: {supported, requested}`. Yöntemler: `tools/list`, `tools/call` (ve yukarıdakiler). Araç hataları (geçersiz argüman, yetki, bulunamadı, sorgu reddi) `isError: true` taşıyan **sonuçtur**; bilinmeyen araç ve bozuk istek JSON-RPC hatasıdır (`-32602`).

`Origin` başlığı: yoksa kabul; varsa yalnızca `LA_PUBLIC_URL` ve `LA_MCP_ALLOWED_ORIGINS` (başkası `403`). İzinli kökenlere CORS başlıkları verilir; köken asla körlemesine yansıtılmaz.

### 13.2 MCP kimlik doğrulama

`Authorization: Bearer …` zorunludur; çerez oturumu **kabul edilmez**.

| Kimlik | Kim olarak | Kapsam |
|---|---|---|
| `sk_…` (`admin` kapsamlı) | Anahtar; kendi projesinde `admin` rolü | Yalnızca o proje. `create_project` yoktur. `admin` kapsamı yoksa `403` |
| OAuth erişim token'ı (`oat_…`) | Onaylayan kullanıcı; her org'daki **güncel** rolü | Grant'in kapsamlarıyla sınırlı: `izlek:read`, `izlek:admin` (`admin`, `read`'i içerir) |
| Kişisel erişim token'ı (`pat_…`, §14) | Token'ın sahibi kullanıcı; her org'daki **güncel** rolü | Token'ın kapsamlarıyla sınırlı; OAuth grant'iyle aynı araç görünürlüğü. Kaynak (`resource`) denetimi yoktur; `LA_PUBLIC_URL` gerekmez |

- Kimlik yok/geçersiz: `401` + `WWW-Authenticate: Bearer [error="invalid_token", …] scope="izlek:read izlek:admin", resource_metadata="<LA_PUBLIC_URL>/.well-known/oauth-protected-resource/mcp"` (`LA_PUBLIC_URL` yoksa `Bearer realm="izlek"`). Bu yanıt gövde okunmadan verilir.
- Token'ın kapsamı araca yetmiyorsa: `403` + `WWW-Authenticate: Bearer error="insufficient_scope", scope="…", resource_metadata="…"`. `tools/list` zaten yalnızca kapsamın izin verdiği araçları listeler.
- Erişim token'ı yalnızca `<LA_PUBLIC_URL>/mcp` kaynağı için verilmişse geçerlidir (RFC 8707).
- Rol denetimi araçların çağırdığı API işleyicilerindedir (§1 tablosu): kapsam rolü aşamaz; başka org'un projesi "bulunamadı"dır.
- Sınırlar: anahtar/grant/kişisel token başına dakikada 300 araç çağrısı; sorgu yanıtı en çok 256 KB.
- Hesap düzeyindeki kimliklerde (OAuth, `pat_`) çağıran birden çok proje görebilir: `project_id` verilmezse araç "project_id is required" hatası döner; sunucu yönergeleri modele önce `list_projects` çağırmasını söyler.

### 13.3 Araçlar

Her araç, ilgili `/api/…` işleyicisini çağıranın kimliğiyle çalıştırır (aynı doğrulama, aynı rol denetimi, aynı denetim kaydı). `project_id`, çağıran tek proje görebiliyorsa verilmeyebilir. Sonuç: `structuredContent` (nesne) + aynı JSON'u taşıyan metin bloğu.

| Araç | Kapsam | En düşük rol | Karşılığı |
|---|---|---|---|
| `list_projects` | read | viewer | `GET /api/projects` + kullanıcının org'ları |
| `create_project` | admin | admin | `POST /api/projects` (yalnızca OAuth) |
| `get_project` | read | viewer | `GET /api/projects/{pid}` |
| `update_project_settings` | admin | admin | `PATCH /api/projects/{pid}` |
| `list_write_tokens` | read | viewer | `GET …/tokens` |
| `create_write_token` | admin | admin | `POST …/tokens` |
| `update_write_token` | admin | admin | `PATCH …/tokens/{token}` |
| `list_secret_keys` | read | admin | `GET …/keys` |
| `create_secret_key` | admin | admin | `POST …/keys` (sır yalnızca bu sonuçta) |
| `get_sdk_setup` | read | viewer | Token ve `LA_PUBLIC_INGEST_URL` ile doldurulmuş kurulum parçacıkları (`web`, `web_module`, `android`, `ios`, `jvm`, `posthog_js`, `posthog_node`, `http`); adres yoksa `https://YOUR-INGEST-HOST.invalid` ve `ingest_host_is_placeholder: true` |
| `verify_installation` | read | viewer | Son N dakikada (1–1440, varsayılan 30) event ve platform başına sayılar, son event zamanı; `/q/sql` üzerinden |
| `list_flags` · `get_flag` · `evaluate_flag` | read | viewer | §5 |
| `create_flag` | admin | admin | `POST …/flags` |
| `update_flag` | admin | admin | Mevcut tanımı okur, verilen alanları değiştirir, `PUT …/flags/{key}`; `expected_version` uyuşmazsa reddedilir |
| `query_trends` · `query_funnel` · `query_retention` · `query_events` · `run_sql` · `get_schema` | read | viewer | §8 sorgu vekili; `project_id` dışındaki argümanlar gövde olarak iletilir |

Araç olarak **sunulmayanlar** (yalnızca UI/API): proje kapatma, token/anahtar/flag silme, üye yönetimi, silme istekleri, insight ve dashboard'lar.

Araç açıklamaları (`annotations`): `readOnlyHint` (okuma araçlarında `true`), `destructiveHint` (`update_*` araçlarında `true`), `idempotentHint`, `openWorldHint: false`.

### 13.4 OAuth 2.1 yetkilendirme sunucusu

Yalnızca `LA_PUBLIC_URL` ayarlıysa açıktır (değilse `/oauth/*` → `404 oauth_not_configured`). `issuer` = `LA_PUBLIC_URL`; tek kaynak `<LA_PUBLIC_URL>/mcp`. Yalnızca yetkilendirme kodu + PKCE (`S256`); tüm istemciler herkese açık istemcidir (istemci sırrı yoktur).

| Uç | |
|---|---|
| `GET /.well-known/oauth-protected-resource[/mcp]` | RFC 9728: `{resource, authorization_servers: [issuer], scopes_supported, bearer_methods_supported: ["header"]}` |
| `GET /.well-known/oauth-authorization-server` | RFC 8414. `code_challenge_methods_supported: ["S256"]`, `token_endpoint_auth_methods_supported: ["none"]`, `authorization_response_iss_parameter_supported: true`, `client_id_metadata_document_supported`, `registration_endpoint` (DCR açıksa) |
| `POST /oauth/register` | RFC 7591 (JSON). `{redirect_uris, client_name?, …}` → `201 {client_id: "oc_…", token_endpoint_auth_method: "none", …}`. Açık kayıttır; IP başına 10 dakikada 30. Hatalar: `invalid_redirect_uri`, `invalid_client_metadata` |
| `GET/POST /oauth/authorize` | Giriş ve onay sayfaları (sunucu tarafında üretilir; İngilizce/Türkçe: `ui_locales` parametresi → `la_locale` çerezi → `Accept-Language`, varsayılan İngilizce). Parametreler: `response_type=code`, `client_id`, `redirect_uri`, `code_challenge`, `code_challenge_method=S256`, `state?`, `scope?`, `resource?` |
| `POST /oauth/token` | Form gövdesi. `grant_type=authorization_code` (`code`, `client_id`, `code_verifier`, `redirect_uri?`, `resource?`) ve `grant_type=refresh_token` (`refresh_token`, `client_id`, `scope?`, `resource?`) → `{access_token, token_type: "Bearer", expires_in, refresh_token, scope}`. Hatalar RFC 6749 §5.2 (`invalid_grant`, `invalid_client`, `invalid_request`, `invalid_target`, `invalid_scope`, `unsupported_grant_type`) |
| `POST /oauth/revoke` | RFC 7009. Yenileme token'ı → tüm grant sona erer; erişim token'ı → yalnızca o token. Her zaman `200` |
| `GET /api/me/connected-apps` | Oturumdaki kullanıcının geçerli grant'leri: `[{id, client_id, client_name, client_kind: "dcr"\|"cimd", client_uri, redirect_hosts, scopes, created_at, last_used_at, expires_at}]` |
| `DELETE /api/me/connected-apps/{id}` | Grant'i iptal eder (`204`); başkasının grant'i `404` |

Kurallar:

- **İstemci**: `oc_…` (kayıtlı) ya da bir Client ID Metadata Document'ın `https` adresi. Belge yalnızca **oturum açmış** kullanıcı için, doğrudan (vekilsiz), yalnızca genel adreslere (bağlantı anında denetlenir), yönlendirme izlemeden, 5 sn / 64 KB sınırıyla indirilir; `client_id` alanı adresle aynı olmalıdır; 1–60 dk önbelleklenir.
- **Yönlendirme adresi**: `https` ya da loopback üzerinde `http`; kayıtlı adresle **birebir** eşleşmelidir (yalnızca loopback adreslerinde port yok sayılır, RFC 8252 §7.3). İstemci ve adresi doğrulanana kadar hatalar sayfada gösterilir, yönlendirme yapılmaz; sonrasında hatalar `error`, `state`, `iss` ile istemciye döner. Her yetkilendirme yanıtı `iss` taşır (RFC 9207).
- **Kaynak** (RFC 8707): `resource` verilmişse `<LA_PUBLIC_URL>/mcp` (ya da kökenin kendisi) olmalıdır, değilse `invalid_target`; verilmemişse aynı kaynak varsayılır.
- **Kapsam**: bilinmeyen kapsamlar yok sayılır; hiç kapsam istenmemişse `izlek:read`. Kullanıcı onay sayfasında `izlek:admin`'i çıkarabilir.
- **Onay**: `la_session` oturumu gerekir; yoksa aynı sayfada giriş formu gösterilir (`POST /api/auth/login` ile aynı denetim, hız sınırı ve denetim kaydı). Formlar CSRF'e (oturuma bağlı token; giriş formunda çereze bağlı token; `Origin`/`Sec-Fetch-Site` denetimi) ve çerçevelemeye (`frame-ancestors 'none'`) karşı korunur.
- **Süreler**: kod 2 dk ve tek kullanımlık; erişim token'ı 1 saat; yenileme token'ı her kullanımda döner (kullanılmayan 30 günde biter); grant en çok 180 gün. Kullanılmış bir kodun ya da yenileme token'ının yeniden sunulması grant'i **iptal eder**.
- Kod ve token'lar yalnızca SHA-256 olarak saklanır (`oauth_codes`, `oauth_tokens`). Biçimler: `oac_`/`oat_`/`ort_` + 40 karakter base62.
- Parola değişimi ve TOTP'nin açılması kullanıcının tüm grant'lerini iptal eder.
- Hız sınırları (IP başına): token + iptal uçları dakikada 300; başarısız token isteği dakikada 30'dan sonra `429` (geçerli istekler etkilenmez).

### 13.5 Denetim kaydı

| `action` | `actor` | `target` | `detail` |
|---|---|---|---|
| `oauth.authorize` | kullanıcı | `client_id` | `{client_name, client_kind, scopes, redirect_host}` |
| `oauth.token` | kullanıcı | `client_id` | `{client_name, client_kind, scopes, grant, grant_type: "authorization_code"\|"refresh_token"}` |
| `oauth.revoke` | kullanıcı | `client_id` | `{…, grant, reason: "user"\|"client"\|"code_replay"\|"refresh_token_reuse"}` |

Araçların yaptığı her değişiklik, UI'dan yapılan aynı değişiklikle aynı kaydı üretir (`project.create`, `token.update`, `flag.update`, …; aktör kullanıcı ya da anahtar) ve `detail.via = {mcp_tool, oauth_client?, oauth_client_id?, oauth_grant?}` alanını taşır. Okuma araçları kayıt üretmez. Kod, token ve anahtar değerleri hiçbir kayda girmez.

## 14. Kişisel erişim token'ları (`pat_`)

Hesap düzeyinde, sabit bir kimlik: başlık gönderebilen ama tarayıcı akışı yürütemeyen istemciler (Claude Code, betikler) için. Token **kullanıcı olarak** davranır: kullanıcının üye olduğu her org'da, her çağrı anındaki rolüyle; kapsamları (`izlek:read`, `izlek:admin`; §13.2 ile aynı anlam) bunu daraltır, asla genişletmez.

Biçim: `pat_` + 32 karakter base62 (`sk_` ile aynı üreteç). Yalnızca SHA-256'sı saklanır (`personal_tokens`, migration 4); değer yalnızca oluşturma yanıtında görünür.

`PersonalToken`: `{ id: "<string>", name, prefix: "pat_…abcd", scopes: ["izlek:read"] | ["izlek:read","izlek:admin"], created_at, last_used_at: ms|null, expires_at: ms|null }`

| Yol | |
|---|---|
| `GET /api/me/tokens` | Çıplak dizi, yeni → eski. Süresi dolmuş token'lar listelenmez |
| `POST /api/me/tokens` | `{name (1–80), scopes, expires_in_days: 30\|90\|365\|null}` → `201` `PersonalToken` + **bir kez** `token: "pat_…"`. `["izlek:admin"]` tek başına verilirse `izlek:read` eklenir. Bilinmeyen kapsam, boş kapsam, geçersiz süre ya da ad → `400 bad_request`. Kullanıcı başına en çok 25 geçerli token: `409 too_many_tokens` |
| `DELETE /api/me/tokens/{id}` | `204`; başkasının (ya da olmayan) kimlik → `404` |

- Bu üç uç **yalnızca oturum çerezi + `X-LA-CSRF`** ile çalışır. `Authorization: Bearer` taşıyan istek (`pat_`, `sk_`, OAuth) reddedilir (`403`/`401`): hiçbir token başka token üretemez, listeleyemez, iptal edemez.
- Parola değişimi ve TOTP'nin açılması kullanıcının **tüm** kişisel token'larını siler (`user.password` / `user.totp_enable` kaydında `personal_tokens_revoked`).
- İptal edilen token silinir; süresi dolanlar saatlik temizlikte silinir. İptal edilmiş, süresi dolmuş ya da bilinmeyen token → `401` (`/mcp`'de §13.2'deki `WWW-Authenticate` ile).
- `last_used_at` token başına en çok dakikada bir güncellenir.

### 14.1 `/mcp` üzerinde

`Authorization: Bearer pat_…` — aynı kapsamlara sahip bir OAuth grant'i gibi davranır (araç listesi, `create_project`, `project_id` kuralı, kimlik başına hız sınırı). `LA_PUBLIC_URL` gerekmez.

### 14.2 `/api/…` üzerinde

Çerez ve CSRF başlığı gerekmez. İzin verilen uçlar:

| Uç | |
|---|---|
| `GET /api/me` | (`csrf` boş döner) |
| `GET /api/projects`, `POST /api/projects` | |
| `/api/projects/{pid}/…` altındaki her şey | aşağıdaki iki istisna dışında; rol denetimi §1 tablosundaki gibidir, başka org'un projesi `404` |

**Kapalı** (`403 forbidden`, kapsam ne olursa olsun): `POST /api/setup`, `POST /api/auth/login`, `POST /api/auth/logout`, `POST /api/me/password`, `POST /api/me/totp/*`, `/api/me/tokens…`, `/api/me/connected-apps…`, `/api/orgs/{org}/members…`, `GET /api/orgs/{org}/audit`, `DELETE /api/orgs/{org}`, `DELETE /api/projects/{pid}` (proje kapatma) ve `POST /api/projects/{pid}/deletions` (silme isteği).

Kapsam: `izlek:admin` yoksa durum değiştiren istekler `403 insufficient_scope` döner. Salt okuma sayılan GET-dışı istekler: `POST …/q/{endpoint}` ve `POST …/flags/{key}/evaluate`.

### 14.3 Denetim kaydı

| `action` | `actor` | `target` | `detail` |
|---|---|---|---|
| `pat.create` | kullanıcı | token `id` | `{name, scopes, expires_at?}` |
| `pat.revoke` | kullanıcı | token `id` | `{name, reason: "user"}` |

Token'la yapılan her değişiklik kullanıcının kaydıdır ve `detail.via = {personal_token: <ad>, personal_token_id: "<id>", mcp_tool?}` taşır. Token değeri ve özeti hiçbir kayda girmez.
