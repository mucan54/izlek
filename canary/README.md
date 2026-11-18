# Kanarya (`la-canary`)

Tasarım: `docs/design.md` §16.2. Sentetik bir proje üzerinden hattın tamamını yoklar: bilinen event'leri LA1 ile `/v1/e`'ye gönderir, bir süre sonra `/q/sql` ile sayar ve **kayıp**, **çift** ve **tazelik** ölçer. Kendi LA1 kodlayıcısını taşır (`canary/la1`, ortak vektörlerle test edilir); ingestion'ın kodunu kullanmaz.

```sh
go build -o bin/la-canary ./canary/cmd/la-canary
LA_CANARY_INGEST_URL=https://in.example.com LA_CANARY_TOKEN=pk_… \
LA_CANARY_QUERY_URL=http://query:8081 LA_CANARY_QUERY_KEY=sk_… bin/la-canary
```

Kanarya için **ayrı bir proje** açın (token + `query` kapsamlı anahtar); ürün verisine karışmaz.

| Değişken | Varsayılan | Anlam |
|---|---|---|
| `LA_CANARY_INGEST_URL`, `LA_CANARY_TOKEN` | — | Ingestion adresi ve yazma token'ı |
| `LA_CANARY_QUERY_URL`, `LA_CANARY_QUERY_KEY` | — | Sorgu servisi ve `query` kapsamlı gizli anahtar |
| `LA_CANARY_INTERVAL` | `1m` | Gönderim aralığı |
| `LA_CANARY_EVENTS_PER_TICK` | `5` | Batch başına event |
| `LA_CANARY_GRACE` | `3m` | Bu süreden genç batch'ler "eksik" sayılmaz |
| `LA_CANARY_RESEND_EVERY` | `10` | N batch'te bir aynı batch'i **bilerek** yeniden gönderir (tekilleştirme sınaması); `0` kapalı |
| `LA_CANARY_STATE` | boş | Durum dosyası (yeniden başlatmada sayaçlar ve koşu kimliği korunur) |
| `LA_CANARY_LISTEN` | `:8099` | `/metrics`, `/healthz`; `off` kapatır |

Kimlikler `(koşu, sıra)` çiftinden deterministik türetilir; böylece doğrulama "şu sıra numaraları tam bir kez var mı" sorusuna iner.

Metrikler: `la_canary_expected`, `la_canary_visible`, `la_canary_missing`, `la_canary_duplicates`, `la_canary_freshness_seconds`, `la_canary_last_verify_timestamp_seconds`, `la_canary_healthy`, `la_canary_sent_total`, `la_canary_send_errors_total`, `la_canary_verify_errors_total`. Alarm kuralları `deploy/prometheus/alerts.yml`, adımlar `docs/runbooks/canary-alarm.md`.

## Tek seferlik kip

```sh
la-canary -once -batches 3 -timeout 3m -poll 2s
```

Gönderir, tüm event'ler tam bir kez görünene kadar bekler; başarıda `0`, eksik/çift ya da süre aşımında `1` ile çıkar. Dağıtım sonrası duman testi ve `e2e/run.mjs` bunu kullanır.
