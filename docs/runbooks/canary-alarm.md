# Kanarya alarmı

Kanarya (`la-canary`) özel bir projeye sıra numaralı event gönderir ve sorgu API'sinden geri okur. **Her doğrulamada tüm koşuyu baştan sayar**; bu yüzden sonradan (ör. hatalı compaction ile) kaybolan veri de yakalanır.

## `la_canary_missing > 0` — onaylanmış event sorguda yok
En ciddi alarmdır: "onaylanmış event kaybı = 0" SLO'su ihlal edilmiş olabilir.

1. **Gecikme mi, kayıp mı?** `la_shipper_ship_lag_seconds` yüksekse önce [shipper gecikmesi](shipper-lag.md). Kanarya, `LA_CANARY_GRACE` (varsayılan 3 dk) dolmadan eksik saymaz; grace'i aşan gecikme alarm üretir ama kayıp değildir — gecikme kapanınca kendiliğinden düzelir.
2. Kanarya logundaki `missing_seqs=[…]` listesini alın. Sıra numarası → gönderim zamanı (dakikada `LA_CANARY_EVENTS_PER_TICK` event).
3. O zaman aralığı için zinciri geriye doğru izleyin:
   - `wal/dt=…/hh=…/` altında o dakikanın segmenti var mı? Yoksa → ingestion node'u (yüklenmemiş yerel segment? node/disk kaybı? → §6.6 dayanıklılık penceresi).
   - Segment var, `wal/_done/<shardkey>/` işareti var mı? Yoksa → shipper işlememiş. Varsa `wal/_quarantine/` kaydı var mı? → [bozuk segment](corrupt-segment.md).
   - İşlenmişse: `la_shipper_records_rejected_total{reason}` (kanarya projesi `unknown_project` ile mi atıldı? `control/projects.json` yayınlandı mı?).
   - Gün manifest'inde dosya var mı, sorgu neden görmüyor? (tombstone? yanlış proje anahtarı?)
4. Kaynak bir yazılım hatasıysa ve WAL duruyorsa: [rebuild](rebuild.md).

## `la_canary_duplicates > 0` — aynı sıra numarası birden çok
1. `la_ingest_dup_batches_total` artıyor mu? Kanarya her 10 batch'ten birini bilerek iki kez gönderir (`LA_CANARY_RESEND_EVERY`); bunlar görünmemelidir.
2. Çiftler compaction'dan **sonra** da duruyor mu? (L0'lar arası çiftler sorgu birleştirmesinde ayıklanır; ayıklanmıyorsa sorgu katmanında hata var.)
3. Aynı `uuid` iki farklı `ts` ile mi yazılmış (saat düzeltmeli yeniden gönderim)? Bu durumda sorgu çift sayar, compaction tekilleştirir — bilinen sınır; kanaryanın saatini (NTP) denetleyin.

## `CanaryNotVerifying` — doğrulama yapılamıyor
Kayıp kanıtı **değildir**; kör kaldık demektir. `la_canary_verify_errors_total`, kanarya logu (query erişimi, `sk_` anahtarı geçerli mi, `401`?), query servisi sağlığı.

## Koşuyu sıfırlamak
Kök neden giderildikten sonra kalıcı bir eksik, alarmı sonsuza dek açık tutar. Olay kaydı kapatıldıktan sonra `LA_CANARY_STATE` dosyasını silip kanaryayı yeniden başlatın (yeni koşu kimliği).
