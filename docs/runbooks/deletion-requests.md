# Silme isteği takibi (KVKK/GDPR)

**Akış.**
1. İstek: UI → Silme istekleri, ya da `POST /api/projects/{pid}/deletions {user_ids, anon_ids, note}`.
2. Control plane `tombstones/p=<pid>/<ts>-<id>.json` yazar.
3. Query servisi ≤ 60 sn içinde yükler → kişi **sorgu sonuçlarından hemen** çıkar.
4. Shipper bundan sonra gelen eşleşen satırları hiç yazmaz.
5. Compactor etkilenen günleri yeniden yazar, replay nesnelerini, kişi özelliklerini ve kimlik bağlarını siler; gün manifest'lerinin `tombstone_ts` değeri ilerler.
6. Control plane, tüm günlerde `tombstone_ts ≥ ts_ms` olunca isteği `done` yapar (ya da 8 gün sonra).
7. `wal/` içindeki ham kopyalar yaşam döngüsüyle (varsayılan 30 gün) silinir. **Kesin silme üst sınırı = WAL saklama süresi.**

**İzleme.** `la_compactor_tombstones_pending`, `la_compactor_tombstoned_rows_total`, `la_compactor_replay_objects_deleted_total`; UI'da isteğin durumu.

**`TombstonesOverdue` (7 gün).**
- Compactor çalışıyor mu, `la_compactor_errors_total` artıyor mu?
- `tombstones/p=<pid>/_applied.state` ilerliyor mu?
- Çok büyük L2 günleri yeniden yazılırken bellek/spool yetiyor mu (`LA_COMPACTOR_SPOOL` diski)?

**Doğrulama (denetim için kanıt).**
```
POST /q/sql  {"from": "<saklama başlangıcı>", "to": "<bugün>",
              "sql": "SELECT count(*) FROM events WHERE user_id = '<id>' OR anon_id = '<anon>'"}   → 0
POST /q/sql  {"sql": "SELECT * FROM persons WHERE person_key = 'u:<id>'", …}                       → boş
```
Depoda: o kişinin `replay/p=<pid>/…/<session>/` nesneleri yok.

**Bilinen sınırlar.**
- WAL saklama süresi dolmadan yapılan bir [rebuild](rebuild.md) tombstone'ları yeniden uygular (tombstone dosyaları 400 gün saklanır); kişi geri gelmez.
- Daha kısa kesin silme gerekiyorsa `LA_WAL_RETENTION_DAYS` düşürülür (rebuild ufku da kısalır).
- Object storage sürümleme açıksa eski sürümler sağlayıcı tarafında yaşam döngüsü kuralıyla ayrıca silinmelidir.
