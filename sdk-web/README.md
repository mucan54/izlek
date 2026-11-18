# Izlek Web SDK

Elle yazılmış, çalışma zamanı bağımlılığı olmayan TypeScript istemcisi: event toplama, kimlik, feature flag ve wireframe tabanlı session replay. Wire formatı `spec/wire.md` (LA1), flag değerlendirmesi `spec/flags.md`, replay formatı `spec/replay.md` (LAR1) ile bire bir uyumludur ve `spec/vectors/` altındaki ortak vektörlerle test edilir.

Üç giriş noktası vardır; çekirdek küçük kalsın diye flags ve replay ayrı yüklenir.

| Modül | İçerik | gzip (min) |
|---|---|---|
| `@izlek/web` | Çekirdek: capture, kimlik, kuyruk, gönderim, otomatik pageview | **3 751 B** (ESM) · 3 713 B (`izlek.min.js`) |
| `@izlek/web/flags` | Kural seti indirme + yerel değerlendirme | +2 057 B · 2 136 B (`izlek-flags.min.js`) |
| `@izlek/web/replay` | Wireframe yakalama + LAR1 kodlayıcı | +4 379 B · 4 290 B (`izlek-replay.min.js`) |
| `@izlek/web/replay-decode` | LAR1 çözücü + `applyFrame` (oynatıcı için) | 1 102 B |

Ölçüm: `npm run size` (esbuild `--minify`, hedef ES2020, gzip seviye 9). Çekirdek için 4 KB bütçesi bu betikte denetlenir; aşılırsa betik hata koduyla çıkar. Üç modül tek pakette 9 033 B.

## Kullanım

```ts
import { init, capture, identify } from '@izlek/web'

init({ token: 'pk_…', host: 'https://in.example.com' })

capture('sepete_eklendi', { urun_id: 42, fiyat: 129.9 })
identify('kullanici-17', { plan: 'pro' })
```

`<script>` ile (sıra önemli; flags ve replay sayfadaki tek çekirdeği kullanır):

```html
<script src="izlek.min.js"></script>
<script src="izlek-flags.min.js"></script>   <!-- isteğe bağlı -->
<script src="izlek-replay.min.js"></script>  <!-- isteğe bağlı -->
<script>
  izlek.init({ token: 'pk_…', host: 'https://in.example.com' })
  izlekFlags.initFlags()
  izlekReplay.startReplay({ sampleRate: 0.1 })
</script>
```

Tıklama takibi yalnızca açıkça işaretlenen öğelerde çalışır (genel autocapture yoktur):

```html
<button data-track="satin_al" data-track-plan="pro">Satın al</button>
<!-- → capture('satin_al', { plan: 'pro' }) -->
```

## API (çekirdek)

| Çağrı | Davranış |
|---|---|
| `init(config)` | Bir kez çağrılır. SSR'da ve bot UA'larında hiçbir şey yapmaz. `init` öncesi çağrılar yok sayılır. |
| `capture(event, props?)` | Event kaydı. Değerler: `null`, boolean, tam sayı (`number`/`bigint` → int64), ondalık (float64), string, dizi/nesne (JSON), `Date` (ISO string). |
| `page(props?)` | Geçerli URL için `pageview`. `autoPageview: false` ise elle çağrılır. |
| `identify(userId, personProps?)` | `user_id`'yi kaydeder, `identify` kaydı yazar. Aynı kimlikle tekrar çağrılırsa yalnızca kişi özellikleri (`set_person`) gider. |
| `setPersonProps(props)` | `set_person` kaydı. |
| `register(props)` / `unregister(key)` | Super props: her event'e eklenir, kalıcıdır. |
| `reset()` | Çıkış: `reset` kaydı eski kimlikle gider, yeni `anon_id` ve yeni oturum başlar. |
| `optOut()` / `optIn()` | `optOut` kuyruğu siler, sonraki çağrıları yok sayar; bayrak kalıcıdır. |
| `flush()` | Kuyruğu gönderir; kuyruk boşalınca ya da gönderim engellenince çözülen `Promise`. |
| `create()` | Bağımsız ikinci bir istemci (ör. ikinci proje ya da test). |

`Config`: `token`, `host` (zorunlu); `flushAt` (20), `flushIntervalMs` (5000), `maxQueueBytes` (200 000), `gzip` (true), `cookieDomain` (ör. `.example.com`), `autoPageview` (true), `autoTrack` (true), `flagsHost` (varsayılan `host`).

Hiçbir public çağrı sayfaya hata fırlatmaz.

### Otomatik davranışlar

- **Context** (batch başlığında bir kez): `platform: "web"`, `url`, `path`, `referrer`, `utm_*`, `locale`, `viewport_w`, `viewport_h`, `ua` (ham). SPA'da sayfa değiştikten sonra aynı batch'e düşen event'ler kendi `url`/`path`/`referrer` değerlerini taşır.
- **`pageview` / `pageleave`**: sayfa açılışında, `history.pushState` / `replaceState` / `popstate` ile yol ya da sorgu değişince, bfcache'ten dönüşte; `pageleave` sayfa kapanırken ve SPA geçişinde.
- **Oturum**: son event'ten 30 dk sonra yeni `session_id`; sayfa yüklemeleri arasında `localStorage` ile sürer.
- **Kimlik sınırı**: bir batch başlığı tek kimlik taşır; `identify`, `reset`, oturum değişimi ve başka sekmedeki kimlik değişimi açık batch'i kapatır.
- **Bot filtresi**: bilinen bot/headless UA'larında SDK hiç başlamaz (çerez de yazmaz).

### Gönderim ve kalıcılık

- Kapanan her batch bellekte tutulur ve `localStorage`'a `la_<token>_<batch_id>` anahtarıyla yazılır; `200` gelince silinir. Sonraki sayfa açılışında kalanlar **aynı `batch_id`** ile yeniden gönderilir (`sent_at` yenilenir). 7 günden eski batch'ler atılır; toplam `maxQueueBytes`'ı aşarsa en eskiler atılır ve sayı sonraki batch'in `dropped_count` context alanında bildirilir.
- Durum kodları (`docs/design.md` §3.5): `2xx` sil · `400` at · `401/403` at + 1 saat dur (sayfa yüklemeleri arasında da) · `413` ikiye böl · `429`/`503` `Retry-After` kadar bekle · diğer her şey ve ağ hatası: 1 sn → 5 dk üstel backoff (%50–100 jitter).
- Sayfa gizlenince/kapanınca kuyruk `fetch(..., { keepalive: true })` ile, başlıksız ve `?t=<token>` ile gönderilir (preflight gerektirmez); `keepalive` yoksa `sendBeacon`. Teslim **en az bir kez**dir: yanıt gelmeden sayfa ölürse aynı batch sonraki açılışta tekrar gider, sunucu `batch_id` ile ayıklar.
- 1 KB üstü gövdeler `CompressionStream` varsa gzip'lenir.

Sunucu/CDN tarafında gerekenler: CORS'ta `Authorization`, `Content-Type`, `Content-Encoding` (ve flags için `If-None-Match`) başlıklarına izin; `Access-Control-Expose-Headers: Retry-After, ETag`. Bunlar yoksa SDK çalışır ama `Retry-After` yerine backoff kullanır ve flag dosyasını koşulsuz indirir.

## Feature flags

```ts
import { initFlags, flag, onFlagsChange } from '@izlek/web/flags'

await initFlags()                      // init() sonrasında
if (flag('new_checkout') === 'b') { … }
flag('beta_banner', false)             // kapalı/bilinmiyor → verilen varsayılan
```

- `<flagsHost>/flags/<token>.json` indirilir; son iyi kopya `localStorage`'dadır ve açılışta ağ beklenmeden kullanılır. Sayfa görünürken 5 dk'da bir `If-None-Match` ile yenilenir.
- Değerlendirme tamamen yereldir: context → super props → kişi özellikleri (`identify` / `setPersonProps` ile bu tarayıcıda verilenler) + `user_id`, `anon_id`.
- `flag()` oturum başına bir kez `flag_exposed` yazar (`flag`, `variant`, `ruleset_v`); değer oturum içinde değişirse (ör. `identify` sonrası) yeniden yazar. `allFlags()` exposure yazmaz.
- Diğer dışa aktarımlar: `evalFlag`, `matchCond`, `murmur3`, `reloadFlags`, `createFlags(client)`.

## Session replay

```ts
import { startReplay, stopReplay } from '@izlek/web/replay'

startReplay({ sampleRate: 0.1, maskAllText: false })
```

Ekran görüntüsü değil wireframe kaydedilir: ilk DOM taraması tam anlık görüntü üretir; `MutationObserver` ve giriş/kaydırma/boyut dinleyicileri ağacı "kirli" işaretler; saniyede en çok iki kez ağaç yeniden ölçülüp fark (`delta`) yazılır. Parçalar 5 sn ya da 64 KB'de kapanır, gzip'lenip `/v1/r`'ye yüklenir; her parça tam anlık görüntüyle başlar. Kullanıcı girdisi yokken (varsayılan 2 dk) ve sekme gizliyken kayıt yapılmaz.

Maskeleme yakalama anında yapılır:

| Öğe | Varsayılan |
|---|---|
| `input`, `textarea`, `select`, `contenteditable` içeriği | `•` ile maskeli (aynı uzunlukta) |
| `type=password` | Her zaman maskeli |
| Görseller (`img`, `svg`, `canvas`, `video`…) | Yalnızca yer tutucu dikdörtgen; `alt`, `src` kaydedilmez |
| Diğer metinler | Açık; `maskAllText: true` ile kapatılır |
| `data-la-mask` | Alt ağaçtaki tüm metni maskeler |
| `data-la-unmask` | Alt ağaçta maskeyi kaldırır (parola hariç) |

Öznitelik değerleri, HTML ve tuş vuruşları hiç kaydedilmez. Oturum örneklemesi `murmur3_32(session_id) mod 10000 < sampleRate × 10000` ile yapılır; oran uygulama tarafından verilir.

Oynatıcı için: `decodeChunk(bytes, withTime?)`, `newScene()`, `applyFrame(scene, frame)` (`@izlek/web/replay-decode`).

## Kişisel veri notu (KVKK/GDPR)

SDK'nın kendiliğinden gönderdiği ya da sakladığı alanlardan kişisel veri sayılması gerekenler:

| Alan | Nerede | Not |
|---|---|---|
| `anon_id` | Batch başlığı; `la_<token>` çerezi (1 yıl) ve `localStorage` | Çevrimiçi tanımlayıcı (takma adlı veri). |
| `user_id` | Batch başlığı, `identify` kaydı, `localStorage` | Doğrudan tanımlayıcı. E-posta/telefon yerine iç kimlik kullanın. |
| Kişi özellikleri | `identify` / `setPersonProps` kayıtları; flags modülü yüklüyse `localStorage` (`la_<token>_p`) | İçerik uygulamaya bağlıdır; çoğu zaman kişisel veridir. |
| `session_id` | Batch başlığı, replay parçaları | Oturum tanımlayıcısı. |
| `ua` | Context | Ham User-Agent; parmak izi bileşeni. |
| `url`, `path`, `referrer`, `utm_*` | Context / event | Sorgu parametrelerinde e-posta, token vb. olabilir; SDK URL'leri **temizlemez**. |
| `locale`, `viewport_w/h` | Context | Tek başına düşük riskli; parmak izi bileşeni. |
| Event ve super props | Kayıtlar | Uygulamanın gönderdiği her şey. |
| Replay metinleri | `/v1/r` | Form alanları maskelidir; ekrandaki diğer metinler varsayılan olarak **açıktır** ve kişisel veri içerebilir (`maskAllText`, `data-la-mask`). |

IP adresi SDK tarafından gönderilmez; sunucu saklamaz, yalnızca ülke kodu türetir. Proje ayarındaki "yazımda at" listesi (shipper) belirli anahtarları kalıcı depoya yazmadan düşürür. Rıza gerekiyorsa `init()` rıza alındıktan sonra çağrılmalıdır: `init` öncesinde çerez ya da `localStorage` yazılmaz. `optOut()` sonrası kimlik durur ama hiçbir şey gönderilmez.

`localStorage` anahtarları: `la_<token>` (durum), `la_<token>_<batch_id>` (gönderilmemiş batch), `la_<token>_f` (kural seti), `la_<token>_p` (kişi özellikleri), `la_<token>_x` (exposure'lar), `la_<token>_r` (replay parça sırası).

## Geliştirme ve test

```
npm install
npm run build         # dist/: ESM modülleri, *.min.js, .d.ts
npm run typecheck
npm run test:unit     # node:test — vektörler + sahte tarayıcı/taşıma/saat (önce build)
npm run test:browser  # Chromium (makinede kurulu Playwright) + yerel toplayıcı (önce build)
npm run size          # gzip boyutları, çekirdek için 4 KB bütçesi
npm test              # hepsi
```

- `test/vectors.test.mjs`: `spec/vectors/{wire,murmur3,flags,replay}.json` doğrudan depo kökünden okunur.
- `test/core.test.mjs`: her durum kodu, 413 bölme, backoff takvimi, kalıcılık ve aynı `batch_id` ile yeniden gönderim, kimlik sınırları, oturum, opt-out, `localStorage` sınırı, sayfa kapanışı, SPA, `data-track`.
- `test/browser/smoke.mjs`: paketlenmiş `*.min.js` dosyaları gerçek tarayıcıda, farklı origin'deki toplayıcıya karşı (CORS/preflight gerçek).

## Bilinen eksikler

- Yalnızca Chromium'da tarayıcı testi yapıldı; Firefox/WebKit ve `sendBeacon` yolu gerçek tarayıcıda doğrulanmadı.
- ES2020 + `BigInt` gerekir; daha eski tarayıcılar için derleme yok.
- `os`, `device_type` gibi ayrılmış sütunlar doldurulmaz (yalnızca ham `ua` gider); `timezone`, `screen_w/h` yok.
- `utm_*` yalnızca geçerli URL'den okunur, oturum boyunca taşınmaz.
- Yalnızca `#hash` değişen yönlendirmeler otomatik `pageview` üretmez (`page()` çağırın).
- Oturum ve super props origin başınadır; alt alan adları arasında yalnızca `anon_id` (çerezle) paylaşılır.
- Başka sekmenin gönderim sırasındaki batch'i yeni sekme tarafından da gönderilebilir (aynı `batch_id`; sunucu ayıklar).
- Replay yüklemesi en iyi çabadır (2 tekrar, kalıcı kuyruk yok); sayfa kapanışındaki son parça gzip asenkron olduğu için kaybolabilir. iframe içi, kapalı shadow DOM, canvas içeriği, sözde öğeler ve CSS animasyonları yakalanmaz; iç içe kaydırma alanlarında kırpma temsil edilmez. `CompressionStream` yoksa replay başlamaz.
- Flags: `regex` operatörü JS `RegExp` ile çalışır (spec'teki ortak alt küme için aynıdır); tam sayı olmayan sayıların `str()` biçimi JS'inkidir.
