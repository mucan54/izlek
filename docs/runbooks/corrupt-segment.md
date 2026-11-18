# Bozuk segment (karantina)

**Belirti.** `la_shipper_segments_quarantined_total` arttı; `wal/_quarantine/<shardkey>/<segment>.json` kaydı oluştu.

**Sistem ne yaptı.** Açılamayan (zstd), sihri bozuk, eksik ya da aşırı büyük segment **atlandı**, kaydı yazıldı ve hat durmasın diye `_done` işareti verildi. Yarım kuyruk (son girdinin CRC'si/uzunluğu bozuk) karantina değildir: geçerli önek işlenir, `la_shipper_wal_corrupt_tail_total` artar.

**Doğrula.**
1. Karantina kaydını okuyun: `reason`, `plan`, segment anahtarı.
2. Segmenti indirip elle açmayı deneyin (`zstd -d`); ilk 4 bayt `LAW1` mi?
3. Kaynağı bulun: segment adı `<node_id>-<seq>` taşır → o ingestion node'unun logları ve diski (donanım hatası? yarım yükleme?).

**Müdahale.**
- Node'un yerel diskinde aynı segmentin `.sealed` kopyası duruyorsa (yükleme tamamlanmadan önceki hali) onu yeniden yükleyin.
- Nesneyi kurtarabildiyseniz **yeni bir adla** aynı `wal/dt=…/hh=…/` önekine yükleyin (ya da aynı anahtara yazıp `wal/_done/<shardkey>/<segment>.ok` işaretini silin). Shipper onu yeni segment olarak işler; daha önce işlenmiş kayıtlar `uuid` ile compaction/sorguda ayıklanır.
- Kurtarılamıyorsa: kayıp, o segmentteki onaylanmış batch'lerdir. Kapsamı (zaman aralığı, node) not edin; kanarya o aralıkta eksik gösterebilir. Bu bir **veri kaybı olayıdır**, olay kaydı açın.

**Sonrası.** Aynı node'dan tekrarlıyorsa node'u ve diskini servisten çıkarın.
