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

- Birim testleri: API istemcisi (CSRF, 401, hata eşleme), göreli tarih aralıkları, filtre ifadesi modeli ↔ JSON, flag formu doğrulaması, LAR1 çözücü (`spec/vectors/replay.json`), oynatıcı durum makinesi, grafik ölçek/işaret yardımcıları, sorgu gövdesi üretimi, dashboard yerleşimi, rol tablosu. Ayrıca mock'un flag değerlendirmesi `spec/vectors/flags.json` ve `murmur3.json` ile, LAR1 kodlayıcısı `replay.json` ile sınanır.
- Uçtan uca: kurulum → giriş → proje → token ve gizli anahtar → trend/huni/tutundurma/SQL insight → dashboard → flag (varyantlar, önizleme, geçmiş) → event akışı → kişi → oturum kaydı oynatma → silme isteği → üyeler/denetim kaydı → parola/TOTP → oturum düşmesi → izleyici rolü → tablet genişliği → klavye → güvenlik başlıkları.
- Playwright paket bağımlılığı değildir; ortamda kurulu olması beklenir (`require('playwright')`).

## Yapı

```
src/
  main.tsx, app.tsx        açılış, oturum, yönlendirme, kabuk (menü, proje seçici)
  i18n/tr.ts               TÜM arayüz metinleri (başka dil = aynı şekilde ikinci bir modül, i18n/index.ts'e kaydedilir)
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
  components/              ui.tsx (düğme, alan, modal, sekme…), FilterEditor, DateRange, charts (SVG), results
  screens/                 Auth, Projects, Events, Insights, Dashboards, Persons, Replay, Flags, Settings, Deletions, Members
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
