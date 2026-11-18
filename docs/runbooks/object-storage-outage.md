# Object storage kesintisi

**Belirti.** `la_ingest_wal_upload_lag_seconds` artıyor; `la_ingest_wal_local_bytes` büyüyor; shipper/query loglarında depo hataları; control plane'de `la_control_publish_lag_seconds` artıyor.

**Sistem ne yapar (tasarım gereği).**
- Ingestion **kabul etmeye devam eder**: segmentler yerel diskte birikir, yükleme üstel geri çekilmeyle sonsuza dek denenir. Yerel WAL `la_ingest_wal_local_limit_bytes` sınırına ulaşınca `/readyz` düşer ve istekler `503 + Retry-After` alır; SDK'lar kuyruklarını diskte tutar ve yeniden dener.
- Shipper ve compactor tur atlar, depo dönünce kaldığı plandan devam eder (planlar idempotenttir).
- Query önbellekteki dosyalarla yanıt verebildiği kadarını verir; soğuk okumalar hata döner.
- Control plane API'si çalışır (doğruluk kaynağı SQLite'tır); yayınlar arka planda yeniden denenir.

**Doğrula.**
1. Sağlayıcının durum sayfası / kendi S3 istemcinizle `LIST` ve `PUT` deneyin (ağ mı, kimlik mi, sağlayıcı mı?).
2. Her ingestion node'unda: `la_ingest_wal_local_bytes / la_ingest_wal_local_limit_bytes` oranı ve artış hızı → diskin dolmasına kalan süre.

**Müdahale.**
- Kimlik/anahtar sorunu ise anahtarı düzeltin; süreçleri yeniden başlatmaya gerek yoktur (bir sonraki denemede geçer). Ortam değişkeni değiştiyse yuvarlanarak yeniden başlatın (`SIGTERM` → segment kapanır, yüklenemezse yerelde kalır, açılışta yeniden denenir).
- Kesinti uzun sürecekse ve disk doluyorsa: [Ingestion diski doluyor](ingestion-disk-full.md).
- Node'ları **silmeyin / diskleri atmayın**: onaylanmış ama yüklenmemiş veri yalnızca o disktedir.

**Sonrası.**
- `wal_upload_lag_seconds` 0'a, `ship_lag_seconds` normale dönene kadar izleyin; birikmiş segmentler sırayla işlenir.
- Kanaryanın `missing = 0`, `duplicates = 0` gösterdiğini doğrulayın.
- Kesinti süresince `503` alan istemciler veriyi sonradan gönderir; `late` bayraklı event sayısı artabilir.
