# Session replay formatı v1 (`LAR1`)

Kaynak: `docs/design.md` §13. Vektörler: `spec/vectors/replay.json`. İlkel tipler `wire.md` §1 ile aynıdır.

## 1. Parça (chunk)

```
Chunk := "LAR1" session_id:bytes16 seq:varint start_ts:varint Frame*
Frame := varint(len) kind:u8 dt_ms:varint payload
```

- `seq`: oturum içinde 0'dan artan parça sırası.
- `start_ts`: parçanın ilk karesinin zamanı (unix ms, cihaz saati).
- `dt_ms`: karenin, parçadaki **önceki kareye** göre gecikmesi (ilk kare için `start_ts`'ye göre).
- `len` = `kind + dt_ms + payload` bayt sayısı. Bilinmeyen `kind` atlanır.

## 2. Kareler

| kind | Ad | payload |
|---|---|---|
| 0 | `full_snapshot` | `w:varint h:varint count:varint Node{count}` |
| 1 | `delta` | `removed:varint id{removed}` `upserted:varint Node{upserted}` |
| 2 | `touch` | `phase:u8 x:varint y:varint` — phase: 0 down · 1 move · 2 up |
| 3 | `scroll` | `node_id:varint dx:zigzag dy:zigzag` |
| 4 | `screen_change` | `name:string` |
| 5 | `viewport` | `w:varint h:varint` |

```
Node := id:varint parent:varint type:u8 x:zigzag y:zigzag w:varint h:varint
        flags:u8 [color:u32le] [text:string]
```

- `id ≥ 1`; `parent = 0` kök demektir. Koordinatlar ekran pikseli (dp/CSS px), köke göre **mutlak**.
- `flags`: bit0 `has_color` · bit1 `has_text` · bit2 `masked` · bit3 `clickable` · bit4 `focused`.
- `color`: `0xAARRGGBB` (yalnızca bit0 kuruluysa vardır). `text`: yalnızca bit1 kuruluysa vardır.
- `type`: 0 container · 1 text · 2 image · 3 button · 4 input · 5 scroll · 6 other.
- Düğümler çizim sırasıyla (ebeveyn çocuklardan önce) yazılır.
- `delta`: `upserted` içindeki düğüm mevcutsa **tümüyle** değiştirilir; `removed` bir düğümü tüm alt ağacıyla kaldırır.

Maskeleme (`docs/design.md` §13.4) **yakalama anında** uygulanır: maskeli metin, aynı uzunlukta `•` (U+2022) karakterleriyle değiştirilir ve `masked` biti kurulur; `type = 2` (image) düğümleri asla içerik taşımaz.

## 3. Örnekleme ve bölme (SDK)

- En çok 2 kare/sn; ağaç değişmediyse kare yazılmaz.
- `full_snapshot`: oturum başında, her `screen_change` sonrasında, her parçanın başında ve en geç 60 sn'de bir.
  (Her parça `full_snapshot` ile başladığından parçalar bağımsız oynatılabilir.)
- Parça 5 sn ya da 64 KB (sıkıştırılmamış) dolunca kapanır.
- Oturum örneklemesi: `murmur3_32(session_id hex32) mod 10000 < sample_rate × 10000`.

## 4. Yükleme

`POST /v1/r?s=<session_hex32>&n=<seq>&ts=<start_ts>&d=<duration_ms>&a=<anon_id>[&u=<user_id>][&t=pk_…]`

- Gövde: sıkıştırılmış `Chunk`. `Content-Encoding: gzip` ya da `zstd` **zorunludur** (gövde açılmadan saklanır).
- Gövde ≤ 1 MB. Token `replay: true` değilse `403`.
- Sunucu gövdeyi akış halinde `replay/p=<pid>/d=<alış günü UTC>/<session_hex32>/<seq:06>.bin.{zst|gz}` anahtarına yazar, ardından WAL'a `format=3` indeks kaydı ekler (`wal.md` §2.3) ve fsync sonrası `200` döner.
- Aynı `(s, n)` tekrar gönderilirse nesnenin üzerine yazılır (içerik aynıdır); indeks event'i deterministik `uuid` ile tekilleşir.

## 5. Oynatma

Oturumun parçaları `/q/events` ile listelenir (`event = "session_replay_chunk"`, `session_id` filtresi, `props.seq` sırası); her parçanın `props.key` nesnesi sorgu servisinin `GET /q/replay?key=…` ucu üzerinden (proje öneki denetlenerek) indirilir.
