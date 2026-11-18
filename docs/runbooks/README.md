# Runbook'lar

`docs/design.md` §18.7'deki başlıklar. Her runbook aynı düzeni izler: **belirti → doğrula → müdahale → sonrası**.
Alarm kuralları `deploy/prometheus/alerts.yml` içindedir ve ilgili runbook'a bağlanır.

| Runbook | Tetikleyen alarm |
|---|---|
| [Object storage kesintisi](object-storage-outage.md) | `WalUploadLag`, `Ingest5xxRate`, `PublishLag` |
| [Ingestion diski doluyor](ingestion-disk-full.md) | `WalDiskHalfFull` |
| [Shipper gecikmesi](shipper-lag.md) | `ShipLag`, `ShardWithoutShipper`, `L0Backlog`, `FreshnessSlo` |
| [Manifest çakışma fırtınası](manifest-conflict-storm.md) | `ManifestConflictStorm` |
| [Bozuk segment](corrupt-segment.md) | `SegmentQuarantined` |
| [Hatalı shipper sürümü sonrası yeniden inşa](rebuild.md) | — (elle) |
| [Token sızıntısı](token-leak.md) | `TokensStale` (yan etki), elle |
| [Silme isteği takibi](deletion-requests.md) | `TombstonesOverdue` |
| [Kanarya alarmı](canary-alarm.md) | `CanaryLossOrDuplicate`, `CanaryNotVerifying` |

> Bu runbook'lar yazılı prosedürdür; **henüz tatbik edilmediler** (`docs/design.md` §21 "Operasyon").
> İlk tatbikatta bulunan eksikler buraya işlenmelidir.

Ortak araçlar:

```
# Bir bileşenin metrikleri
curl -s http://<host>:<port>/metrics | grep la_
# Depoyu elle incelemek (file:// ise doğrudan dizin; S3 ise herhangi bir S3 istemcisi)
#   wal/dt=<gün>/hh=<saat>/        yüklenmiş segmentler
#   wal/_plan/<shardkey>/           shipper planları (en yüksek numara = son plan)
#   wal/_done/<shardkey>/           işlenmiş segment işaretleri
#   wal/_quarantine/<shardkey>/     karantina kayıtları
#   data/p=<pid>/d=<gün>/_m/        gün manifest sürümleri
```
