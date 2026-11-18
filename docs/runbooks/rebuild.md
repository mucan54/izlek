# Hatalı shipper sürümü sonrası yeniden inşa (`rebuild`)

**Ne zaman.** `data/` altındaki dosyalar yanlış üretildiyse (hatalı eşleme/şema, bozuk compaction) ya da kaybolduysa; kaynak `wal/` hâlâ duruyorsa (varsayılan 30 gün).

**Ön koşullar.**
- Düzeltilmiş `la-shipper` sürümü dağıtılmış olmalı.
- Aralık WAL saklama ufkunun içinde olmalı (`--force` ufku yok sayar; eksik segmentlerin günleri eksik üretilir).
- Rebuild sırasında shard haritasını değiştirmeyin.

**Yürütme.**
```
la-shipper rebuild --from 2026-11-10 --to 2026-11-18            # tüm projeler
la-shipper rebuild --from 2026-11-10 --to 2026-11-18 --project 1017
la-shipper rebuild … --with-side-tables                          # identity/ ve persons/ da yeniden yazılsın
```
`--from/--to` **event günleridir** (düzeltilmiş `ts`'nin UTC günü).

Süreç boyunca:
- Ingestion, sorgu ve compaction çalışmaya devam eder.
- **Shipping bekler** (`la_shipper_rebuild_fenced = 1`): rebuild her shardkey'e bir çit planı yazar; tazelik bozulur, veri kaybolmaz.
- Her `(proje, gün)` tek bir manifest sürümüyle takas edilir; sorgular ya eski ya yeni dosya kümesini görür, karışığını değil.

**Yarıda kalırsa.** Aynı komutu yeniden çalıştırın (kaldığı yerden sürer). Vazgeçmek için `la-shipper rebuild --abort` — yalnızca çiti kaldırır, takas edilmiş günler yeni haliyle kalır.

**Doğrula.**
- Komut çıktısındaki gün başına satır sayıları; WAL'da satırı olmayan günler "dokunulmadı" diye raporlanır.
- Kanarya projesinde `missing = 0`, `duplicates = 0`.
- Etkilenen bir dashboard'u rebuild öncesi/sonrası karşılaştırın.

**Sonrası.** Eski dosyalar 24 saatlik GC beklemesinden sonra silinir; bu süre boyunca depolama ≈ 2× olur. Rebuild süresini ve hızını kaydedin (kapasite planı girdisi).

> `data/` yeniden inşa tatbikatı henüz yapılmadı (`docs/design.md` §21).
