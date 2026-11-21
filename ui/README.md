# İzlek UI

Control plane'in `/` altında sunduğu tek sayfalık yönetim arayüzü. TypeScript + Preact + esbuild; grafik kütüphanesi ve UI kiti yok (grafikler elle yazılmış SVG, oturum kaydı oynatıcısı canvas). Çalışma anında dış ağ isteği yapmaz (CDN, font vb. yok); CSP `default-src 'self'` altında çalışır.

Arayüz yalnızca `spec/control.md` API'sini (ve onun `/api/projects/{pid}/q/*` sorgu vekilini) çağırır.

## Çalıştırma

```sh
npm install

npm run dev          # esbuild --watch + mock control plane (demo verisiyle) → http://localhost:8090
                     # giriş: demo@izlek.local / izlek-demo-123   (izleyici: izleyici@izlek.local)
MOCK_SEED=0 npm run dev   # boş veritabanı: ilk kurulum ekranıyla açılır
```

Gerçek bir control plane'e karşı:

```sh
npm run build                      # dist/ üretir (statik dosyalar)
# control plane'i LA_UI_DIR=/…/ui/dist ile başlatın; dist/'i kendisi sunar (spec/control.md §10)

# ya da control plane'i yeniden başlatmadan, yerelde:
LA_CONTROL_URL=http://127.0.0.1:8090 npm run dev   # → http://localhost:8089, /api/ gerçek servise iletilir
```

`dist/` içeriği: `index.html` + `assets/app-<hash>.js` + `assets/app-<hash>.css`. Sunucunun bilinmeyen yolları `index.html`'e düşürmesi gerekir (`/api/` hariç).

## Testler

```sh
npm test             # typecheck + build + birim testleri + uçtan uca test
npm run typecheck
npm run test:unit    # node:test; testler esbuild ile build/unit/ altına derlenir
npm run test:e2e     # Playwright + Chromium, süreç içi mock sunucuya karşı (dist/ gerekir)
E2E_SHOTS=/tmp/izlek-shots npm run test:e2e   # ana ekranların açık/koyu ekran görüntülerini de yazar
```

- Birim testleri: API istemcisi (CSRF, 401, hata eşleme), göreli tarih aralıkları, filtre ifadesi modeli ↔ JSON, flag formu doğrulaması, LAR1 çözücü (`spec/vectors/replay.json`), oynatıcı durum makinesi, grafik ölçek/işaret yardımcıları, sorgu gövdesi üretimi, dashboard yerleşimi, rol tablosu, dil modülleri (`tr` ve `en` aynı anahtar/tür yapısında mı, fonksiyonlar aynı argümanlarla metin döndürüyor mu, İngilizce çoğullar, dil seçimi ve `la_locale`), MCP yardımcıları (OAuth üstverisinin yorumlanması, adres, Claude Code komutları, kapsam özeti; araç listesi ve komutlar `docs/mcp.md` ile karşılaştırılır). Ayrıca mock'un flag değerlendirmesi `spec/vectors/flags.json` ve `murmur3.json` ile, LAR1 kodlayıcısı `replay.json` ile sınanır.
- Uçtan uca: kurulum → giriş → proje → token ve gizli anahtar → trend/huni/tutundurma/SQL insight → dashboard → flag (varyantlar, önizleme, geçmiş) → event akışı → kişi → oturum kaydı oynatma → silme isteği → üyeler/denetim kaydı → parola/TOTP → dil değiştirme (İngilizce'ye geçiş, yeniden yüklemede kalıcılık, Türkçe'ye dönüş) → yapay zekâ asistanları sayfası (adres ve kopyalama, OAuth açık/kapalı, MCP kapalı) → bağlı uygulamalar (listeleme, onaylı iptal, boş durum, 404) → erişim token’ları (oluşturma, bir kez gösterme ve kopyalama, 25 sınırı, onaylı iptal, 404'te gizlenme) → oturum düşmesi → izleyici rolü → tablet genişliği → klavye → güvenlik başlıkları.
- Uçtan uca senaryo Türkçe metinleri doğrular. Arayüz dilini tarayıcıdan seçtiği ve Playwright'ın varsayılan dili `en-US` olduğu için test bağlamı açıkça `locale: 'tr-TR'` ile açılır; dil adımı arayüzden İngilizce'ye geçip geri döner.
- Playwright paket bağımlılığı değildir; ortamda kurulu olması beklenir (`require('playwright')`).

## Diller

Arayüz Türkçe (`tr`) ve İngilizce (`en`) gelir. İki dil de aynı JS paketindedir; dil değiştirmek ağ isteği yapmaz (İngilizce pakete yaklaşık 7,7 KB gzip ekler).

- **Seçim** açılışta, ilk çizimden önce yapılır (`main.tsx` → `detectLocale()`): `localStorage['la_locale']` değeri `tr` ya da `en` ise o dil; değilse tarayıcı dili (`navigator.language`) `tr` ile başlıyorsa Türkçe, diğer her durumda İngilizce. Depolamaya erişilemiyorsa (gizli mod vb.) tarayıcı diline bakılır.
- **Değiştirme**: giriş/kurulum ekranının altındaki ve Hesap sayfasındaki (“Tercihler” kartı) “Dil” / “Language” seçim kutusu. Seçim `la_locale` anahtarına yazılır ve sayfa yeniden yüklenmeden uygulanır: `App`, ekranları dil koduyla anahtarlanmış bir `Fragment` içinde çizer, dil değişince ekranlar yeni dilde baştan kurulur. Oturum ve API istemcisi korunur; ekranların kendi durumu (ör. yarım kalmış bir form) sıfırlanır. Tercih tarayıcıya özeldir, sunucuda saklanmaz.
- Sayı ve tarih biçimleri de dile uyar (`INTL`: `tr-TR` / `en-US`). Yüzde işaretinin yeri (`%25` ↔ `25%`), kısaltma ekleri ve süre birimleri dil modülünde `fmt` altındadır.
- Metinlere her zaman çizim anında `S.…` ile erişin. `S` canlı bir bağdır (`setLocale` yeniden atar); modül düzeyinde `const X = S.foo` gibi bir sabit ilk dilde kalır. Gerekiyorsa fonksiyon yapın (`const x = () => S.foo`).
- Çevrilmeyenler: API'nin döndürdüğü hata iletileri (`error.message`, sunucudan geldiği gibi gösterilir), SDK örneklerindeki event adları ve kimlikler (örnek veri; yalnızca örneklerdeki açıklama satırları dile uyar), marka adı.

Yeni bir dil eklemek için:

1. `src/i18n/<kod>.ts` yazın: `export const xx: Strings = { … }`. `tr.ts` ile aynı anahtarlar ve aynı fonksiyon imzaları gerekir; eksik ya da fazla anahtar `npm run typecheck`'te hata verir. Sayı alan fonksiyonlarda dilin çoğul kurallarını uygulayın.
2. `src/i18n/index.ts` içindeki `LOCALES` tablosuna kaydedin: `xx: { strings: xx, intl: 'xx-XX', name: '<dilin kendi dilindeki adı>' }`. Seçim kutusu ve `la_locale` denetimi bu tablodan beslenir.
3. Tarayıcı diline göre kendiliğinden seçilmesini istiyorsanız kuralı `pickLocale`'e ekleyin (şu an: `tr*` → Türkçe, diğerleri → İngilizce).
4. `test/unit/i18n.test.ts` içindeki yapı karşılaştırmasına yeni modülü ekleyin.

## Yapay zekâ asistanları (MCP) ve bağlı uygulamalar

Control plane'in uzak MCP sunucusu (`docs/mcp.md`) için arayüzde iki yer vardır. Metinlerin tamamı dil modüllerinde (`mcp`, `apps`) durur; adımlar ve uyarılar `docs/mcp.md`'den alınmıştır ("doğrulanmadı" notları dahil).

- **Proje ayarları → “Yapay zekâ asistanları” sekmesi** (`/p/<pid>/settings/mcp`, her rol okuyabilir): MCP adresi ve kopyalama düğmesi, Claude Code / claude.ai–Claude Desktop / ChatGPT için bağlantı adımları, asistanın yapabildikleri (araçlar `docs/mcp.md`'deki gibi üç grupta; değişiklik yapanlar ✎ ile işaretli) ve yapamadıkları, güvenlik notları.
  - OAuth'un açık olup olmadığı `GET /.well-known/oauth-authorization-server` ile anlaşılır (`lib/mcp.ts`): `issuer` içeren JSON → OAuth açık; `404` → yalnızca gizli anahtarlar (`LA_PUBLIC_URL` ayarlanmamış); `200` ama JSON değil (yol SPA kabuğuna düşmüş) → MCP kapalı (`LA_MCP_ENABLED=false`) ya da sunucu eski. Yalnızca durum koduna bakılmaz.
  - Gösterilen adres `location.origin + '/mcp'`'dir. Sunucunun `issuer`'ı (yani `LA_PUBLIC_URL`) sayfanın açıldığı adresten farklıysa onun `/mcp`'si gösterilir ve nedeni yazılır: OAuth belirteçleri tam olarak `<LA_PUBLIC_URL>/mcp` için geçerlidir.
  - Komutlarda gizli anahtar yerine her zaman `sk_…` yer tutucusu yazılır; “Gizli anahtar oluştur” bağlantısı, o sekmeyi görebilen rollere (yönetici) gösterilir.
- **Hesap → “Bağlı uygulamalar” kartı**: `GET /api/me/connected-apps` (çıplak dizi) ile OAuth izinleri listelenir; kapsamlar düz sözle gösterilir (`izlek:read` → salt okunur, `izlek:admin` → okuma ve değişiklik). “Erişimi iptal et” onay penceresinden sonra `DELETE /api/me/connected-apps/{id}` çağırır. Liste ucu `404` dönerse hata yerine tek satırlık “etkin değil” notu çıkar. Parola değişince ya da iki adımlı doğrulama açılınca (sunucu tüm izinleri iptal eder) liste yeniden okunur.
- **Hesap → “Erişim token’ları” kartı** (`screens/AccessTokens.tsx`, “Bağlı uygulamalar”ın hemen üstünde): hesap genelinde kişisel erişim token’ları (`pat_…`). `GET /api/me/tokens` (çıplak dizi), `POST /api/me/tokens` `{ name, scopes, expires_in_days: 30|90|365|null }` (token yanıtta yalnızca bir kez gelir ve yalnızca oluşturma penceresinde gösterilir), `DELETE /api/me/tokens/{id}`. Liste ucu `404` dönerse (eski sunucu) kart hiç çizilmez. `too_many_tokens` hatası arayüz dilinde gösterilir. Parola değişince ya da iki adımlı doğrulama açılınca liste yeniden okunur. Yapay zekâ asistanları sekmesinde Claude Code için ilk seçenek bu token’dır (`mcp-cmd-pat`); proje gizli anahtarı “yalnızca bu proje” olarak ikinci sıradadır.

Mock (`mock/server.mjs`) erişim token’ı uçlarını da sunar (25 sınırı, kullanıcıya özel, parola/TOTP değişiminde tümü iptal; `mock.hooks.pats = false` → `404`). Ayrıca bu iki ucu ve üstveri belgesini sunar; `MOCK_SEED=1` ile demo kullanıcısına iki uygulama (Claude: okuma ve değişiklik, ChatGPT: salt okunur) bağlanır. Testler için: `mock.hooks.oauth = false` (üstveri `404`), `mock.hooks.mcp = false` (üstveri yolu SPA kabuğuna düşer, bağlı uygulama uçları `404`), `mock.connectDemoApps(userId)` / `mock.connectApp(userId, {…})`. Vekil kipinde (`LA_CONTROL_URL`) `/api/` ile birlikte `/.well-known/`, `/oauth/` ve `/mcp` de gerçek servise iletilir.

Dışarıdan Playwright ile seçiciler: `[data-testid="mcp-url"]` (adres alanı), `mcp-url-copy`, `mcp-status` (`data-state`: `checking | oauth | keys | off | unknown`), `mcp-cmd-key`, `mcp-cmd-oauth`, `mcp-client-code | -claude | -chatgpt`, `mcp-tools`; `[data-testid="connected-apps"]` (`data-count`, satırlar `tr[data-app-id]`), `connected-apps-off`; `[data-testid="access-tokens"]` (`data-count`, satırlar `tr[data-token-id]`), `pat-secret` (bir kez gösterilen token), `pat-copy`, `mcp-cmd-pat`.

## Yapı

```
src/
  main.tsx, app.tsx        açılış, oturum, yönlendirme, kabuk (menü, proje seçici)
  i18n/
    tr.ts, en.ts           TÜM arayüz metinleri; dil başına aynı yapıda bir modül
    index.ts               dil seçimi: S (geçerli metinler), INTL, setLocale, detectLocale, chooseLocale (bkz. Diller)
  lib/
    api.ts                 fetch sarmalayıcı: çerez oturumu, X-LA-CSRF, 401 → giriş, hata gövdesi → ApiError
    types.ts               control + query API tipleri
    dates.ts               göreli aralık ("7d") → from/to
    filter.ts              and/or/not ifade ağacı: düzenleme modeli ↔ JSON
    flagform.ts            flag formu: model, doğrulama, yüzde ↔ baz puan
    insight.ts             kayıtlı sorgu: varsayılanlar, doğrulama, vekile giden gövde
    layout.ts              dashboard ızgarası (12 sütun) yerleşimi
    chart.ts, format.ts    eksen/ölçek yardımcıları; sayı, tarih, süre biçimleri
    roles.ts, router.tsx, hooks.ts, session.ts, snippets.ts
    mcp.ts                 MCP sayfası için saf yardımcılar: OAuth üstverisi → durum, adres, komutlar, araç listesi, kapsam özeti
  components/              ui.tsx (düğme, alan, modal, sekme…), FilterEditor, DateRange, charts (SVG), results
  screens/                 Auth, Projects, Events, Insights, Dashboards, Persons, Replay, Flags, Settings, Deletions, Members,
                           Mcp (yapay zekâ asistanları sekmesi + bağlı uygulamalar kartı), AccessTokens (erişim token’ları kartı)
  replay/
    lar1.ts                LAR1 çözücü: ../sdk-web/src/replay-decode.ts yeniden dışa aktarılır (tek kaynak)
    fetch.ts               parça indirme + gzip/zstd açma
    player.ts              zaman çizgisi ve durum makinesi (DOM'suz, test edilebilir)
    draw.ts                canvas çizimi
  styles.css               tek stil dosyası; açık/koyu tema prefers-color-scheme ile
mock/
  server.mjs               bağımlılıksız mock control plane + statik sunucu + (LA_CONTROL_URL ile) vekil
  data.mjs                 deterministik demo verisi ve spec/query.md'nin bellek içi uygulaması
  expr.mjs, lar1.mjs, totp.mjs   koşul eşleme + flag değerlendirme, LAR1 kodlayıcı + örnek oturum, TOTP
test/unit, test/e2e
```

## Sözleşme yorumları

Belirsiz kalan noktalarda yapılan varsayımlar (gerçek servisle bütünleştirirken denetlenmeli):

- Liste uçları çıplak dizi döner (`{"items": […]}` gibi tek anahtarlı sarmalayıcı da kabul edilir).
- CSRF belirteci `GET /api/me` yanıtındaki `csrf` alanıdır; giriş/kurulumdan sonra `/api/me` yeniden okunur. Sorgu vekiline giden `POST`'lar da başlığı taşır.
- `"7d"` = bugün dahil son 7 UTC günü (`from = bugün − 6`, `to = bugün`). Event akışındaki `1h`/`24h` aralıkları RFC 3339 anları olarak gider.
- Kayıtlı sorguda grafik türü `query.ui = {"chart": "bar"}` olarak saklanır (yalnızca sütun grafikte); vekile gönderilmeden önce çıkarılır.
- `is_set` koşulu `"value": true` ile yazılır (değer yok sayılır).
- Dashboard `x, y, w, h`: 12 sütunlu ızgara; `h` satır birimi (UI 2/3/4 kullanır). UI kutuları sıralı liste olarak tutar, `x/y`'yi soldan sağa paketleyerek üretir.
- Denetim kaydı sayfalaması: `before` = son satırın `id`'si.
- Parola/TOTP doğrulama uçlarında (`/api/me/password`, `/api/me/totp/*`) dönen `401`, oturum düşmesi sayılmaz.
- `/q/replay` gövdesi `Content-Encoding` ile de (tarayıcı açar) ham olarak da (UI açar) gelebilir; içerik sihirli baytlara bakılarak ayırt edilir.
- Düğüm `color` alanı: `type = text` için yazı rengi, diğerlerinde dolgu rengi (web SDK'nın ürettiği gibi). `scroll` karesi düğümü alt ağacıyla birlikte kaydırır (SDK'nın `applyFrame`'i).

## Bilinen eksikler

- TOTP kurulumunda QR kodu yok; gizli anahtar ve `otpauth://` bağlantısı metin olarak gösterilir.
- Kaydedilmemiş değişiklikle sayfadan ayrılırken uyarı yok.
- Dashboard'da sürükle-bırak yok; sıralama ve boyut düğme/seçim kutusuyla yapılır.
- SQL düzenleyici düz `textarea`'dır (sözdizimi renklendirme/otomatik tamamlama yok).
- Zaman kovaları ve gün sınırları UTC'dir (v1 sözleşmesi); tablolardaki zaman damgaları tarayıcının yerel saatiyle gösterilir.
- `scan_limit_bytes` ve `max_concurrent_queries` yalnızca gösterilir, düzenlenmez.
- Grafikte en çok 8 seri çizilir; fazlası yalnızca tabloda görünür.
- Oynatıcı tel kafes çizer: yazı tipi/satır kırılımı cihazdakiyle birebir değildir; kaydırılabilir kapsayıcılar için kırpma uygulanmaz (yalnızca ekran sınırına kırpılır).
- SDK örnekleri `docs/design.md` §4.2 ve `sdk-web/README.md`'ye göre yazıldı; Android/iOS paket koordinatları ve Swift çağrı biçimi yayımlanan SDK ile doğrulanmalı.
- Mock'un `/q/sql` ucu gerçek bir SQL motoru değildir; UI'nin ve demonun kullandığı birkaç sorgu biçimini tanır.
