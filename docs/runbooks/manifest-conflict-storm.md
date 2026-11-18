# Manifest çakışma fırtınası

**Belirti.** `la_shipper_manifest_conflicts_total` / `la_compactor_manifest_conflicts_total` hızla artıyor; loglarda `too many manifest conflicts`; plan süreleri uzuyor.

**Arka plan.** Manifest sürümleri "yoksa-oluştur" ile yazılır; kaybeden yeniden okur ve tekrar dener. Tek shipper + tek compactor ile çakışma seyrektir. Fırtına şu durumlardan birine işaret eder:

1. **Aynı shard'da iki etkin shipper/compactor** (kira çalışmıyor: saat kayması, `--no-lease` ile başlatılmış ikinci süreç).
2. **Depo "yoksa-oluştur"u doğru uygulamıyor** (çakışma değil, sessiz üzerine yazma olurdu — bu durumda fırtına değil veri tutarsızlığı görülür; kanarya yakalar).
3. Çok sayıda geç gelen veriyle aynı güne yazan shipper ile o günü yeniden yazan compactor'ın yarışı.

**Doğrula.**
- `control/lease/<ad>/` altındaki epoch dosyalarında `holder` sık sık değişiyor mu?
- Node saatleri: kira devri `acquired_at + ttl_ms < şimdi` karşılaştırmasına dayanır; NTP'yi denetleyin.
- Hangi dizin? Hata mesajı çakışan manifest dizinini içerir (`too many manifest conflicts on data/p=…/d=…/_m`).

**Müdahale.**
- Fazla süreci durdurun; `LA_SHIPPER_SHARDS` / `LA_COMPACTOR_SHARDS` atamalarını düzeltin.
- (3) ise compactor'ı o gün için geçici yavaşlatın: `LA_COMPACTOR_INTERVAL_SECS`'i artırın.
- Çakışmalar doğruluğu bozmaz (kaybeden her zaman yeniden dener ya da vazgeçer); yalnızca boşa iştir. Acele ile manifest dosyası **silmeyin**.

**Sonrası.** Çakışma oranı normale dönmeli; kanarya temiz kalmalı.
