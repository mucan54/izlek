# Token sızıntısı / token döndürme

İki tür anahtar vardır ve riskleri farklıdır.

## `pk_` yazma token'ı
Tasarım gereği **herkese açıktır** (uygulamaya gömülür). "Sızıntı" bir olay değildir; kötüye kullanım (sahte event basma) olaydır.

**Belirti.** Beklenmeyen kaynaktan trafik, `la_ingest_rate_limited_total` artışı, saçma event adları.

**Müdahale.**
1. Web token'ı ise `allowed_origins` listesini doldurun (Proje ayarları → Token'lar). Tarayıcı dışı istemciler `Origin` göndermeyerek bunu aşabilir; yine de gelişigüzel kötüye kullanımı keser.
2. `rate_limit`'i düşürün.
3. Gerekirse yeni token oluşturup uygulamayı güncelleyin, eski token'ı **devre dışı** bırakın (silmeden önce): ingestion `403` döner, SDK'lar batch'i atar ve 1 saat susar. Eski uygulama sürümleri veri gönderemez — bilerek verilen karardır.
4. Sahte veriyi ayıklamak için: ayırt edici bir özellik varsa silme isteği (`anon_ids`) ya da etkilenen günler için filtreli yeniden inşa gerekir; genel bir "event sil" aracı v1'de yoktur.

Değişiklik `control/tokens.json` ile yayılır; ingestion 30 sn'de bir yoklar. `TokensStale` alarmı (10 dk'dır yenilenemiyor) varsa değişikliğiniz **uygulanmamıştır**: önce [object storage](object-storage-outage.md) ve control plane yayın durumuna (`la_control_publish_lag_seconds`) bakın.

## `sk_` gizli anahtar
Okuma (`query`) ve/veya yönetim (`admin`) yetkisi verir; sızıntısı **güvenlik olayıdır**.

1. Anahtarı hemen silin (Proje ayarları → Gizli anahtarlar ya da `DELETE /api/projects/{pid}/keys/{id}`). `control/keys.json` yeniden yayınlanır; query servisi ≤ 60 sn içinde yeni listeyi yükler.
2. Yeni anahtar üretin, tüketicileri güncelleyin.
3. Denetim kaydında (`/api/orgs/{org}/audit`) anahtarın `key:<id>` aktörüyle yaptığı işlemleri, query servisi loglarında o projedeki sorguları inceleyin.
4. `admin` kapsamlıysa: token/flag/proje ayarlarında yetkisiz değişiklik var mı? Flag sürüm geçmişi ve denetim kaydı ile karşılaştırın.

Depoda yalnızca `sha256(sk_)` tutulur; `control/keys.json` sızsa bile anahtarlar geri elde edilemez.
