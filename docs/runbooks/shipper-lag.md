# Shipper gecikmesi / compaction birikimi

**Belirti.** `la_shipper_ship_lag_seconds > 300`, `la_shipper_segments_pending` artıyor, `la_canary_freshness_seconds > 90`, ya da `la_shipper_lease_held == 0`.

**Doğrula.**
1. Kira kimde? `control/lease/shipper-<shardkey>/` altındaki en yüksek epoch dosyası (`holder`, `acquired_at`). Sahibi ölü ise yedek en geç 90 sn sonra devralır; yedek yoksa süreci başlatın.
2. Shipper logu: `plan executed` satırları geliyor mu, `seconds` ve `records_out` ne? Hata döngüsü var mı (`la_shipper_errors_total`)?
3. `la_shipper_rebuild_fenced == 1` ise süren bir rebuild vardır; shipping bilerek bekliyordur ([rebuild](rebuild.md)).
4. Bellek: shipper RSS'i plan boyutuyla orantılıdır (≈ 1,5 KB/event). OOM ile ölüyorsa `LA_SHIPPER_BATCH_MB`'yi düşürün.

**Müdahale.**
- Tek shard yetişemiyorsa (CPU %100, plan süresi ≈ batch aralığı): shard sayısını artırın — `control/shards.json`'a daha yüksek `version` ve **ileri** bir `from_ms` ile yeni harita ekleyin; geçiş penceresinde (48 saat) eski ve yeni shardkey'leri aynı süreçte çalıştırın (`LA_SHIPPER_SHARDS=*`).
- Geçici sıçramada beklemek yeterlidir: planlar sırayla işlenir, veri kaybı olmaz; yalnızca tazelik bozulur.
- `L0Backlog`: compactor çalışıyor mu (`la_compactor_lease_held`), `la_compactor_errors_total` artıyor mu? Kaynağını artırın ya da `LA_COMPACTOR_L0_FILES`'ı düşürün. L0 birikimi doğruluğu bozmaz, sorguları yavaşlatır.

**Sonrası.** `ship_lag_seconds` < 60 ve kanarya tazeliği < 90 sn olana kadar izleyin.
