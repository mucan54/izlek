// LAR1 encoder (spec/replay.md) and a scripted demo recording, so the mock
// serves real chunks the player has to decode like production data.

const varint = (out, v) => {
  let n = v
  while (n >= 128) {
    out.push((n % 128) | 128)
    n = Math.floor(n / 128)
  }
  out.push(n)
}
const zigzag = (out, v) => varint(out, v < 0 ? -v * 2 - 1 : v * 2)
const string = (out, s) => {
  const b = new TextEncoder().encode(s)
  varint(out, b.length)
  for (const x of b) out.push(x)
}
const node = (out, n) => {
  varint(out, n.id)
  varint(out, n.parent)
  out.push(n.type)
  zigzag(out, n.x)
  zigzag(out, n.y)
  varint(out, n.w)
  varint(out, n.h)
  out.push(n.flags)
  if (n.flags & 1) out.push(n.color & 255, (n.color >>> 8) & 255, (n.color >>> 16) & 255, (n.color >>> 24) & 255)
  if (n.flags & 2) string(out, n.text)
}

export const encodeFrame = (f) => {
  const body = [f.kind]
  varint(body, f.dt)
  if (f.kind == 0) {
    varint(body, f.w)
    varint(body, f.h)
    varint(body, f.nodes.length)
    for (const n of f.nodes) node(body, n)
  } else if (f.kind == 1) {
    varint(body, f.removed.length)
    for (const id of f.removed) varint(body, id)
    varint(body, f.upserted.length)
    for (const n of f.upserted) node(body, n)
  } else if (f.kind == 2) {
    body.push(f.phase)
    varint(body, f.x)
    varint(body, f.y)
  } else if (f.kind == 3) {
    varint(body, f.node_id)
    zigzag(body, f.dx)
    zigzag(body, f.dy)
  } else if (f.kind == 4) string(body, f.name)
  else if (f.kind == 5) {
    varint(body, f.w)
    varint(body, f.h)
  }
  const out = []
  varint(out, body.length)
  return out.concat(body)
}

/** `chunk`: { session_id: hex32, seq, start_ts, frames } → Uint8Array */
export const encodeChunk = (chunk) => {
  const out = [0x4c, 0x41, 0x52, 0x31]
  for (let i = 0; i < 32; i += 2) out.push(parseInt(chunk.session_id.slice(i, i + 2), 16))
  varint(out, chunk.seq)
  varint(out, chunk.start_ts)
  let bytes = out
  for (const f of chunk.frames) bytes = bytes.concat(encodeFrame(f))
  return Uint8Array.from(bytes)
}

// ---- scripted recording ----

const W = 390
const H = 844
const CHUNK_MS = 5000

// node types / flags
const CONTAINER = 0, TEXT = 1, IMAGE = 2, BUTTON = 3, INPUT = 4, SCROLL = 5, OTHER = 6
const COLOR = 1, HAS_TEXT = 2, MASKED = 4, CLICKABLE = 8, FOCUSED = 16

const WHITE = 0xffffffff
const BLUE = 0xff0a84ff
const DARK = 0xff1d2126
const GREY = 0xff6b7480
const PANEL = 0xfff2f4f6
const GREEN = 0xff1f8a4c

/** Records frames against a live node list and cuts chunks every 5 s, each starting with a full snapshot. */
class Recorder {
  constructor(sessionId, startTs) {
    this.sessionId = sessionId
    this.startTs = startTs
    this.nodes = new Map()
    this.screen = ''
    this.chunks = []
    this.cur = null
    this.t = 0 // ms since session start
    this.last = 0
  }
  open(t) {
    this.cur = { session_id: this.sessionId, seq: this.chunks.length, start_ts: this.startTs + t, frames: [], end: t }
    this.last = t
    if (this.screen) this.cur.frames.push({ kind: 4, dt: 0, name: this.screen })
    this.cur.frames.push({ kind: 5, dt: 0, w: W, h: H })
    if (this.nodes.size) this.cur.frames.push({ kind: 0, dt: 0, w: W, h: H, nodes: [...this.nodes.values()].map((n) => ({ ...n })) })
  }
  close() {
    if (!this.cur) return
    const { end, ...chunk } = this.cur
    this.chunks.push({ ...chunk, duration_ms: Math.max(0, end - (chunk.start_ts - this.startTs)) })
    this.cur = null
  }
  /** Moves the clock; opens a new chunk when the current one is 5 s old. */
  at(t) {
    this.t = t
    if (this.cur && t - (this.cur.start_ts - this.startTs) >= CHUNK_MS) this.close()
    if (!this.cur) this.open(t)
    return this
  }
  emit(frame) {
    this.cur.frames.push({ ...frame, dt: this.t - this.last })
    this.last = this.t
    this.cur.end = this.t
  }
  subtree(root) {
    const ids = new Set([root])
    for (let size = 0; size != ids.size; ) {
      size = ids.size
      for (const n of this.nodes.values()) if (ids.has(n.parent)) ids.add(n.id)
    }
    return ids
  }
  show(name, nodes) {
    this.screen = name
    this.nodes = new Map(nodes.map((n) => [n.id, n]))
    this.emit({ kind: 4, name })
    this.emit({ kind: 0, w: W, h: H, nodes: nodes.map((n) => ({ ...n })) })
  }
  delta(removed, upserted) {
    for (const id of removed) for (const x of this.subtree(id)) this.nodes.delete(x)
    for (const n of upserted) this.nodes.set(n.id, n)
    this.emit({ kind: 1, removed, upserted: upserted.map((n) => ({ ...n })) })
  }
  scroll(id, dy) {
    for (const x of this.subtree(id)) {
      const n = this.nodes.get(x)
      n.y += dy
    }
    this.emit({ kind: 3, node_id: id, dx: 0, dy })
  }
  touch(phase, x, y) {
    this.emit({ kind: 2, phase, x, y })
  }
  tap(t, x, y) {
    this.at(t).touch(0, x, y)
    this.at(t + 70).touch(2, x, y)
  }
  finish() {
    this.close()
    return this.chunks
  }
}

const n = (id, parent, type, x, y, w, h, extra = {}) => {
  const node = { id, parent, type, x, y, w, h, flags: 0 }
  if (extra.color !== undefined) {
    node.flags |= COLOR
    node.color = extra.color
  }
  if (extra.text !== undefined) {
    node.flags |= HAS_TEXT
    node.text = extra.masked ? '•'.repeat([...extra.text].length) : extra.text
  }
  if (extra.masked) node.flags |= MASKED
  if (extra.clickable) node.flags |= CLICKABLE
  if (extra.focused) node.flags |= FOCUSED
  return node
}

const PRODUCTS = [
  ['Seramik kupa', '249,90 ₺'],
  ['Keten önlük', '389,00 ₺'],
  ['Bambu kesme tahtası', '459,50 ₺'],
  ['Cam saklama kabı', '179,90 ₺'],
  ['Pamuklu havlu seti', '529,00 ₺'],
  ['Dökme demir tava', '1.249,00 ₺'],
  ['Ahşap kaşık seti', '199,00 ₺'],
  ['Çelik termos', '649,90 ₺'],
]

/**
 * A ~34 s shopping session: login → product list (scrolling) → idle →
 * product detail → cart → order placed. Returns the chunks (logical frames
 * plus `duration_ms`) and the moments worth an analytics event.
 */
export const demoRecording = (sessionId, startTs) => {
  const r = new Recorder(sessionId, startTs)

  // -- login --
  const emailBox = (text, focused) => n(4, 1, INPUT, 24, 236, 342, 48, { text, masked: true, focused })
  const passBox = (text, focused) => n(6, 1, INPUT, 24, 324, 342, 48, { text, masked: true, focused })
  r.at(0).show('Giriş', [
    n(1, 0, CONTAINER, 0, 0, W, H, { color: WHITE }),
    n(2, 1, TEXT, 24, 120, 300, 34, { text: 'Tekrar hoş geldin', color: DARK }),
    n(3, 1, TEXT, 24, 210, 200, 18, { text: 'E-posta', color: GREY }),
    emailBox('', false),
    n(5, 1, TEXT, 24, 298, 200, 18, { text: 'Parola', color: GREY }),
    passBox('', false),
    n(7, 1, BUTTON, 24, 404, 342, 50, { text: 'Giriş yap', color: BLUE, clickable: true }),
    n(8, 1, TEXT, 110, 474, 170, 18, { text: 'Parolamı unuttum', color: BLUE, clickable: true }),
  ])
  r.tap(600, 180, 260)
  r.at(700).delta([], [emailBox('', true)])
  for (let i = 1; i <= 4; i++) r.at(700 + i * 420).delta([], [emailBox('ayse@ornek.com'.slice(0, i * 4), true)])
  r.tap(2700, 180, 348)
  r.at(2800).delta([], [emailBox('ayse@ornek.com', false), passBox('', true)])
  for (let i = 1; i <= 3; i++) r.at(2800 + i * 450).delta([], [passBox('x'.repeat(i * 3), true)])
  r.tap(4500, 195, 428)

  // -- product list (chunk boundary at 5 s falls here) --
  const list = [
    n(1, 0, CONTAINER, 0, 0, W, H, { color: WHITE }),
    n(20, 1, SCROLL, 0, 150, W, 8 * 132 + 20),
  ]
  PRODUCTS.forEach(([title, price], i) => {
    const y = 160 + i * 132
    const id = 30 + i * 4
    list.push(n(id, 20, OTHER, 16, y, 358, 120, { color: PANEL, clickable: true }))
    list.push(n(id + 1, id, IMAGE, 28, y + 12, 96, 96))
    list.push(n(id + 2, id, TEXT, 140, y + 26, 220, 22, { text: title, color: DARK }))
    list.push(n(id + 3, id, TEXT, 140, y + 60, 160, 20, { text: price, color: GREY }))
  })
  // header drawn last so it stays on top of the scrolled list
  list.push(n(10, 1, CONTAINER, 0, 0, W, 150, { color: WHITE }))
  list.push(n(11, 10, TEXT, 16, 56, 240, 30, { text: 'Merhaba Ayşe', color: DARK }))
  list.push(n(12, 10, INPUT, 16, 98, 358, 40, { text: 'Ürün ara' }))
  r.at(5100).show('Ana sayfa', list)

  // two flick gestures, each a handful of move + scroll frames
  const flick = (t0, total) => {
    r.at(t0).touch(0, 200, 620)
    for (let i = 1; i <= 6; i++) {
      r.at(t0 + i * 90).touch(1, 200, 620 - i * 40)
      r.scroll(20, Math.round(total / 6))
    }
    r.at(t0 + 640).touch(2, 200, 380)
  }
  flick(6400, -240)
  flick(8300, -264)
  flick(10600, -180)
  // back up a little, then open "Bambu kesme tahtası" (third card)
  r.at(12100).touch(0, 200, 300)
  for (let i = 1; i <= 4; i++) {
    r.at(12100 + i * 90).touch(1, 200, 300 + i * 45)
    r.scroll(20, 60)
  }
  r.at(12560).touch(2, 200, 480)
  const third = r.nodes.get(38)
  r.tap(13600, 190, Math.max(170, Math.min(H - 40, third.y + 60)))

  // -- product detail, after a long idle stretch (reading) --
  const cartButton = (text, color) => n(64, 1, BUTTON, 24, 700, 342, 54, { text, color, clickable: true })
  r.at(13900).show('Ürün', [
    n(1, 0, CONTAINER, 0, 0, W, H, { color: WHITE }),
    n(60, 1, IMAGE, 0, 0, W, 360),
    n(61, 1, TEXT, 24, 388, 320, 30, { text: 'Bambu kesme tahtası', color: DARK }),
    n(62, 1, TEXT, 24, 428, 200, 24, { text: '459,50 ₺', color: DARK }),
    n(63, 1, TEXT, 24, 472, 342, 120, {
      text: 'Tek parça bambudan, su oluklu kesme tahtası. Elde yıkanır, ayda bir yağlanması önerilir.',
      color: GREY,
    }),
    cartButton('Sepete ekle', BLUE),
  ])
  r.tap(24200, 195, 726)
  r.at(24400).delta([], [
    cartButton('Sepete eklendi ✓', GREEN),
    n(70, 1, OTHER, 24, 60, 342, 56, { color: DARK }),
    n(71, 70, TEXT, 40, 78, 240, 20, { text: 'Sepetinde 1 ürün var', color: WHITE }),
    n(72, 70, BUTTON, 290, 70, 64, 36, { text: 'Git', color: BLUE, clickable: true }),
  ])
  r.tap(26300, 322, 88)
  // the toast (70) goes away with its children when the cart opens
  r.at(26400).delta([70], [])

  // -- cart --
  const payButton = (text, color) => n(88, 1, BUTTON, 24, 700, 342, 54, { text, color, clickable: true })
  r.at(26600).show('Sepet', [
    n(1, 0, CONTAINER, 0, 0, W, H, { color: WHITE }),
    n(80, 1, TEXT, 24, 64, 300, 32, { text: 'Sepetim', color: DARK }),
    n(81, 1, OTHER, 16, 120, 358, 96, { color: PANEL }),
    n(82, 81, IMAGE, 28, 132, 72, 72),
    n(83, 81, TEXT, 116, 140, 240, 22, { text: 'Bambu kesme tahtası', color: DARK }),
    n(84, 81, TEXT, 116, 172, 160, 20, { text: '1 adet · 459,50 ₺', color: GREY }),
    n(85, 1, TEXT, 24, 250, 160, 20, { text: 'Kart numarası', color: GREY }),
    n(86, 1, INPUT, 24, 276, 342, 48, { text: '4242424242424242', masked: true }),
    n(87, 1, TEXT, 24, 640, 342, 28, { text: 'Toplam: 459,50 ₺', color: DARK }),
    payButton('Satın al', BLUE),
  ])
  r.tap(30800, 195, 726)
  r.at(31000).delta([], [payButton('İşleniyor…', GREY)])
  r.at(32600).delta([85, 86], [
    payButton('Alışverişe dön', BLUE),
    n(90, 1, OTHER, 24, 250, 342, 120, { color: 0xffe6f4ec }),
    n(91, 90, TEXT, 44, 276, 300, 26, { text: 'Siparişin alındı', color: GREEN }),
    n(92, 90, TEXT, 44, 314, 300, 20, { text: 'Sipariş no: 48213', color: GREY }),
  ])
  r.at(33800).delta([], [n(87, 1, TEXT, 24, 640, 342, 28, { text: 'Teşekkürler!', color: DARK })])

  return {
    chunks: r.finish(),
    // [offset ms, event, screen, props]
    moments: [
      [0, 'app_opened', 'Giriş', {}],
      [4600, 'login', 'Giriş', { method: 'password' }],
      [5100, 'screen', 'Ana sayfa', {}],
      [13700, 'product_viewed', 'Ürün', { product_id: 'p-103', category: 'mutfak', price: 459.5 }],
      [24300, 'add_to_cart', 'Ürün', { product_id: 'p-103', price: 459.5 }],
      [26600, 'checkout_started', 'Sepet', { items: 1 }],
      [32600, 'purchase', 'Sepet', { amount: 459.5, currency: 'TRY', items: 1 }],
    ],
  }
}
