# Ingestion diski doluyor

**Belirti.** `WalDiskHalfFull`; `la_ingest_ready == 0`; istemciler `503` alıyor.

**Neden olur.** Neredeyse her zaman yükleme durmuştur (bkz. [object storage kesintisi](object-storage-outage.md)); nadiren trafik, disk ve yükleme hızını aşmıştır.

**Doğrula.**
```
curl -s :8080/metrics | grep -E "la_ingest_(wal_local_bytes|wal_local_limit_bytes|wal_upload_lag_seconds|segments_uploaded_total)"
ls -la $LA_WAL_DIR | head          # *.sealed = yüklenmeyi bekleyen, *.wal = açık segment
```

**Müdahale (tercih sırasıyla).**
1. Yüklemeyi düzeltin — tek kalıcı çözüm budur.
2. Trafiği diğer node'lara kaydırın: bu node'un `/readyz`'i zaten düşüktür; yük dengeleyicinin onu çıkardığını doğrulayın. Yeni ingestion node'u ekleyin.
3. Diski büyütün (ağ diski ise çevrimiçi genişletme) ve gerekiyorsa `LA_WAL_MAX_LOCAL_BYTES`'ı yükseltip yuvarlanarak yeniden başlatın.
4. Yükleme eşzamanlılığını artırın: `LA_WAL_UPLOAD_CONCURRENCY`.

**Yapmayın.** `$LA_WAL_DIR` altından dosya silmeyin: her dosya istemciye `200` dönülmüş veridir. Node'u kalıcı olarak kapatacaksanız önce diskini başka bir ingestion sürecine bağlayıp (aynı `LA_NODE_ID` ve `LA_WAL_DIR`) yüklemenin bitmesini bekleyin.

**Sonrası.** `wal_local_bytes` sıfıra yakınsamalı; kanarya temiz olmalı.
