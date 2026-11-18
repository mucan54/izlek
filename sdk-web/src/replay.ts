// Wireframe session replay capture for the web (docs/design.md §13, spec/replay.md).
//
// The page is reduced to rectangles: an initial walk of the DOM produces a
// full snapshot, a MutationObserver (plus input, scroll and resize listeners)
// marks the tree dirty, and at most twice a second the tree is re-measured and
// diffed against the last state that was sent. No pixels, no markup, no
// attribute values leave the page; text is masked at capture time.
//
// Coordinates are viewport ("screen") CSS pixels. Node 1 is the document: a
// scroll node positioned at minus the scroll offset. Window scrolling is sent
// as a scroll frame on node 1, which moves the whole subtree.

import { client as defaultClient, type Client } from './index'
import { murmur3 } from './murmur3'
import {
  BUTTON,
  CLICKABLE,
  CONTAINER,
  FOCUSED,
  HAS_COLOR,
  HAS_TEXT,
  IMAGE,
  INPUT,
  MASKED,
  OTHER,
  SCROLL,
  TEXT,
  chunkHeader,
  encodeFrame,
  type Frame,
  type RNode,
} from './replay-format'
import type { Bytes } from './wire'

export { encodeChunk, encodeFrame } from './replay-format'
export type { Chunk, Frame, RNode } from './replay-format'

export interface ReplayOptions {
  /** Share of sessions to record, 0..1. Default 1. */
  sampleRate?: number
  /** Mask every text, not only form fields. Default false. */
  maskAllText?: boolean
  /** Upper bound of nodes per snapshot. Default 4000. */
  maxNodes?: number
  /** Stop recording after this long without user input. Default 2 min. */
  idleMs?: number
}

export interface Replay {
  /** Start recording (after `init()`). No-op without `CompressionStream`. */
  start(options?: ReplayOptions): void
  /** Upload what is buffered and stop. */
  stop(): void
  /** True while this session is being recorded. */
  active(): boolean
}

const TICK_MS = 500 // at most 2 tree frames per second
const CHUNK_MS = 5000
const CHUNK_BYTES = 65536
const TEXT_MAX = 1000

/** Mask text: one bullet per code point. */
export const maskText = (s: string): string => '•'.repeat(Math.min(Array.from(s).length, TEXT_MAX))

/** Session sampling decision (spec/replay.md §3). */
export const sampled = (sessionId: string, rate: number): boolean => murmur3(sessionId) % 10000 < rate * 10000

const same = (a: RNode, b: RNode) =>
  a.parent == b.parent &&
  a.type == b.type &&
  a.x == b.x &&
  a.y == b.y &&
  a.w == b.w &&
  a.h == b.h &&
  a.flags == b.flags &&
  a.color === b.color &&
  a.text === b.text

/**
 * Delta between the last sent tree (`prev`, in draw order) and a fresh walk.
 *
 * Removing a node removes its subtree on the player, so only the topmost
 * removed nodes are listed, and nodes that survive below a removed ancestor
 * (re-parented) are upserted again.
 */
export const diffTrees = (
  prev: Map<number, RNode>,
  next: RNode[],
): { removed: number[]; upserted: RNode[] } | undefined => {
  const present = new Set<number>()
  for (const n of next) present.add(n.id)
  const gone = new Set<number>()
  const removed: number[] = []
  for (const [id, n] of prev) {
    if (!present.has(id)) {
      if (!gone.has(n.parent)) removed.push(id)
      gone.add(id)
    } else if (gone.has(n.parent)) gone.add(id)
  }
  const upserted = next.filter((n) => {
    const p = prev.get(n.id)
    return !p || gone.has(n.id) || !same(p, n)
  })
  if (removed.length || upserted.length) return { removed, upserted }
}

/** `rgb()/rgba()` computed colour → 0xAARRGGBB; undefined when fully transparent or unparsable. */
export const parseColor = (s: string): number | undefined => {
  const m = /^rgba?\(\s*([\d.]+)[,\s]+([\d.]+)[,\s]+([\d.]+)(?:[,/\s]+([\d.]+))?\s*\)$/.exec(s)
  if (m) {
    const a = m[4] == null ? 255 : Math.round(+m[4] * 255)
    if (a) return ((a << 24) | (Math.round(+m[1]) << 16) | (Math.round(+m[2]) << 8) | Math.round(+m[3])) >>> 0
  }
}

const SKIP = /^(SCRIPT|STYLE|HEAD|META|LINK|NOSCRIPT|TEMPLATE|TITLE|BASE|BR|WBR)$/
const MEDIA = /^(IMG|SVG|CANVAS|VIDEO|PICTURE|OBJECT|EMBED)$/
const FIELD = /^(INPUT|TEXTAREA|SELECT)$/
const BUTTON_INPUT = /^(button|submit|reset)$/
const TEXTLESS_INPUT = /^(checkbox|radio|range|color|file|image)$/

export const createReplay = (client: Client): Replay => {
  const core = client._
  let opts: ReplayOptions = {}
  let running = false
  let off = false // the server refused replay uploads (401/403)
  let K = ''
  let endpoint = ''

  // session
  let sid = ''
  let recording = false // current session is sampled in
  let seq = 0

  // DOM → node ids
  let ids = new WeakMap<Node, number>()
  let nextId = 1

  // last state sent, in draw order; coordinates kept in sync with scroll frames
  let tree = new Map<number, RNode>()
  let sx = 0
  let sy = 0
  let vw = 0
  let vh = 0

  // open chunk
  let buf: Bytes = []
  let chunkStart = 0
  let lastT = 0
  let lastTreeT = 0

  let dirty = true
  let screen: string | undefined
  let lastInput = 0
  let lastScroll = 0
  let lastMove = 0
  let pointerDown = false
  let observer: MutationObserver | undefined
  let timer: ReturnType<typeof setInterval> | undefined
  const cleanup: Array<() => void> = []

  const now = () => Date.now()

  const idOf = (n: Node) => {
    let id = ids.get(n)
    if (!id) ids.set(n, (id = ++nextId))
    return id
  }

  /** Measure the whole document into wireframe nodes (parents before children). */
  const walk = (): RNode[] => {
    const max = opts.maxNodes || 4000
    const out: RNode[] = []
    const doc = document.documentElement
    const body = document.body
    const range = document.createRange()
    const active = document.activeElement
    // round document coordinates, not viewport ones, so that scrolling by a
    // fractional offset never looks like a layout change
    const X = (v: number) => Math.round(v + scrollX) - sx
    const Y = (v: number) => Math.round(v + scrollY) - sy

    out.push({
      id: 1,
      parent: 0,
      type: SCROLL,
      x: -sx,
      y: -sy,
      w: Math.max(doc.scrollWidth, innerWidth),
      h: Math.max(doc.scrollHeight, innerHeight),
      flags: HAS_COLOR,
      color:
        parseColor(getComputedStyle(doc).backgroundColor) ??
        (body && parseColor(getComputedStyle(body).backgroundColor)) ??
        0xffffffff,
    })

    /** mask: 0 inherit defaults · 1 mask everything · 2 unmask */
    const visit = (el: Element, parent: number, mask: number) => {
      if (out.length >= max) return
      const tag = el.tagName.toUpperCase()
      if (SKIP.test(tag)) return
      if (el.hasAttribute('data-la-mask')) mask = 1
      else if (el.hasAttribute('data-la-unmask')) mask = 2
      const cs = getComputedStyle(el)
      if (cs.display == 'none' || cs.opacity == '0') return
      const r = el.getBoundingClientRect()
      const e = el as HTMLInputElement

      let type = CONTAINER
      let flags = 0
      let text: string | undefined
      let leaf = false
      let emit = false

      if (MEDIA.test(tag)) {
        // images are placeholders: never any content
        type = IMAGE
        leaf = emit = true
      } else if (FIELD.test(tag)) {
        const kind = (e.type || '').toLowerCase()
        if (kind == 'hidden') return
        leaf = emit = true
        if (BUTTON_INPUT.test(kind)) {
          type = BUTTON
          flags = CLICKABLE
          text = e.value
          if (mask == 1) flags |= MASKED
        } else {
          type = INPUT
          if (!TEXTLESS_INPUT.test(kind)) {
            const value = tag == 'SELECT' ? (el as any as HTMLSelectElement).selectedOptions[0]?.text || '' : e.value
            if (value) {
              text = value
              // field contents are masked unless explicitly unmasked; passwords always
              if (mask != 2 || kind == 'password') flags |= MASKED
            } else if (e.placeholder) {
              text = e.placeholder
              if (mask == 1) flags |= MASKED
            }
          }
        }
      } else if (tag == 'BUTTON' || el.getAttribute('role') == 'button') {
        type = BUTTON
        flags = CLICKABLE
        emit = true
        if (!el.children.length) {
          // a plain text button is one node carrying its label
          text = (el.textContent || '').replace(/\s+/g, ' ').trim()
          leaf = true
          if (mask == 1) flags |= MASKED
        }
      } else if (tag == 'IFRAME' || tag == 'HR' || tag == 'AUDIO') {
        type = OTHER
        leaf = emit = true
      } else {
        if (el.hasAttribute('contenteditable') && (el as HTMLElement).isContentEditable) {
          // rich text editors are text fields
          type = INPUT
          emit = true
          if (mask != 2) mask = 1
        } else if (
          /auto|scroll/.test(cs.overflowX + cs.overflowY) &&
          (el.scrollHeight > el.clientHeight || el.scrollWidth > el.clientWidth)
        ) {
          type = SCROLL
          emit = true
        }
        if ((tag == 'A' && el.hasAttribute('href')) || el.hasAttribute('onclick')) {
          flags = CLICKABLE
          emit = true
        }
      }

      const color = type == IMAGE ? undefined : parseColor(cs.backgroundColor)
      // plain containers only matter when they paint something
      if (color != null || cs.borderTopWidth != '0px' || cs.boxShadow != 'none') emit = true
      if (cs.visibility != 'visible' || !(r.width || r.height)) emit = false

      if (emit) {
        if (text != null) {
          text = flags & MASKED ? maskText(text) : text.slice(0, TEXT_MAX)
          flags |= HAS_TEXT
        }
        if (color != null) flags |= HAS_COLOR
        if (el == active) flags |= FOCUSED
        const n: RNode = {
          id: idOf(el),
          parent,
          type,
          x: X(r.left),
          y: Y(r.top),
          w: Math.round(r.width),
          h: Math.round(r.height),
          flags,
        }
        if (color != null) n.color = color
        if (text != null) n.text = text
        out.push(n)
        parent = n.id
      }
      if (leaf) return

      const textColor = parseColor(cs.color)
      const kids = (root: ParentNode) => {
        for (const child of root.childNodes) {
          if (child.nodeType == 1) visit(child as Element, parent, mask)
          else if (child.nodeType == 3 && cs.visibility == 'visible' && out.length < max) {
            const s = (child.nodeValue || '').replace(/\s+/g, ' ').trim()
            if (s) {
              range.selectNodeContents(child)
              const tr = range.getBoundingClientRect()
              if (tr.width && tr.height) {
                const n: RNode = {
                  id: idOf(child),
                  parent,
                  type: TEXT,
                  x: X(tr.left),
                  y: Y(tr.top),
                  w: Math.round(tr.width),
                  h: Math.round(tr.height),
                  flags: HAS_TEXT | (mask == 1 ? MASKED : 0),
                  text: mask == 1 ? maskText(s) : s.slice(0, TEXT_MAX),
                }
                if (textColor != null) {
                  n.flags |= HAS_COLOR
                  n.color = textColor
                }
                out.push(n)
              }
            }
          }
        }
      }
      if (el.shadowRoot) kids(el.shadowRoot)
      kids(el)
    }

    if (body) visit(body, 1, opts.maskAllText ? 1 : 0)
    return out
  }

  /** Append a frame to the open chunk, stamping `dt`. */
  const add = (t: number, f: Frame) => {
    f.dt = Math.max(t - lastT, 0)
    lastT = Math.max(t, lastT)
    encodeFrame(buf, f)
  }

  const snapshot = (t: number) => {
    const x = Math.round(scrollX)
    const y = Math.round(scrollY)
    if (tree.size && t - lastTreeT < TICK_MS) {
      // Measured less than half a second ago: a chunk that has to start right now
      // repeats that state (moved by any scrolling since) instead of walking the
      // DOM again, which keeps measurements at two per second at most.
      for (const n of tree.values()) {
        n.x += sx - x
        n.y += sy - y
      }
      sx = x
      sy = y
      dirty = true
    } else {
      sx = x
      sy = y
      tree = new Map()
      for (const n of walk()) tree.set(n.id, n)
      dirty = false
      lastTreeT = t
    }
    vw = innerWidth
    vh = innerHeight
    add(t, { kind: 0, dt: 0, w: vw, h: vh, nodes: Array.from(tree.values()) })
  }

  /**
   * Make sure a chunk is open, rolling over a full one first. Returns true if a
   * new chunk was started: it begins with a full snapshot of the current page
   * (so every chunk can be played alone), and the caller has no tree work left.
   */
  const open = (t: number): boolean => {
    if (buf.length >= CHUNK_BYTES || t - chunkStart >= CHUNK_MS) close()
    if (buf.length) return false
    chunkStart = lastT = t
    if (screen != null) {
      add(t, { kind: 4, dt: 0, name: screen })
      screen = undefined
    }
    snapshot(t)
    return true
  }

  const upload = async (query: string, bytes: Uint8Array<ArrayBuffer>, keepalive: boolean) => {
    try {
      const c = core.c()!
      const body = await core.z(bytes)
      for (let attempt = 0; ; attempt++) {
        let status = 0
        try {
          status = (
            await fetch(endpoint + query, {
              method: 'POST',
              body,
              headers: {
                authorization: 'Bearer ' + c.token,
                'content-type': 'application/octet-stream',
                'content-encoding': 'gzip',
              },
              // lets the last chunk outlive the page when it fits the keepalive budget
              keepalive: keepalive && body.byteLength < 6e4,
            })
          ).status
        } catch {}
        if (status == 401 || status == 403) off = true
        // retry (twice) only what can succeed later: network errors, 429, 5xx
        if ((status && status != 429 && status < 500) || attempt > 1 || off) return
        await new Promise((r) => setTimeout(r, 2000 * 4 ** attempt))
      }
    } catch {}
  }

  /** Close the open chunk and upload it. `last`: the page is going away. */
  const close = (last?: boolean) => {
    if (!buf.length) return
    const s = core.s()
    // the sequence continues across page loads (and tabs) of the same session
    let stored: any
    try {
      stored = JSON.parse(localStorage.getItem(K)!)
    } catch {}
    if (stored && stored.s == sid && stored.n > seq) seq = stored.n
    const head = chunkHeader(sid, seq, chunkStart)
    const query =
      '?s=' +
      sid +
      '&n=' +
      seq +
      '&ts=' +
      chunkStart +
      '&d=' +
      (lastT - chunkStart) +
      '&a=' +
      encodeURIComponent(s.a) +
      (s.u ? '&u=' + encodeURIComponent(s.u) : '')
    seq++
    try {
      localStorage.setItem(K, JSON.stringify({ s: sid, n: seq }))
    } catch {}
    const bytes = new Uint8Array(head.concat(buf))
    buf = []
    upload(query, bytes, !!last)
  }

  /** Follow the core session; decide sampling when it changes. */
  const follow = (): boolean => {
    const s = core.s()
    if (off || s.o) {
      buf = []
      return false
    }
    const id = core.i()
    if (id != sid) {
      close() // the old session's tail, under the old id
      sid = id
      seq = 0
      recording = sampled(id, opts.sampleRate ?? 1)
      screen = location.pathname
      dirty = true
    }
    return recording
  }

  /** Window scroll → scroll frame on the document node; keeps `tree` in step. */
  const syncScroll = (t: number) => {
    const x = Math.round(scrollX)
    const y = Math.round(scrollY)
    const dx = sx - x
    const dy = sy - y
    if (dx || dy) {
      lastScroll = t
      if (!open(t)) {
        sx = x
        sy = y
        for (const n of tree.values()) {
          n.x += dx
          n.y += dy
        }
        add(t, { kind: 3, dt: 0, node_id: 1, dx, dy })
      }
      // fixed and sticky elements did not move with the document
      dirty = true
    }
  }

  const tick = () => {
    const t = now()
    // without user input nothing is recorded: tickers and carousels must not keep a recording alive
    if (document.visibilityState == 'hidden' || t - lastInput > (opts.idleMs || 12e4)) return close(true)
    if (!follow()) return
    syncScroll(t)
    if (buf.length && (innerWidth != vw || innerHeight != vh)) {
      vw = innerWidth
      vh = innerHeight
      add(t, { kind: 5, dt: 0, w: vw, h: vh })
      dirty = true
    }
    if (screen != null && buf.length) {
      // screen change inside a chunk: name it, then a full snapshot
      add(t, { kind: 4, dt: 0, name: screen })
      screen = undefined
      snapshot(t)
    } else if (dirty && t - lastTreeT >= TICK_MS && !open(t)) {
      const nodes = walk()
      const d = diffTrees(tree, nodes)
      dirty = false
      lastTreeT = t
      if (d) {
        tree = new Map()
        for (const n of nodes) tree.set(n.id, n)
        add(t, { kind: 1, dt: 0, removed: d.removed, upserted: d.upserted })
      }
    }
    if (buf.length >= CHUNK_BYTES || (buf.length && t - chunkStart >= CHUNK_MS)) close()
  }

  const input = () => {
    lastInput = now()
  }

  const pointer = (phase: number) => (e: PointerEvent) => {
    const t = (lastInput = now())
    if (phase == 0) pointerDown = true
    else if (!pointerDown) return
    if (phase == 2) pointerDown = false
    // moves are sampled; down and up always go through
    if (phase == 1 && t - lastMove < 50) return
    lastMove = t
    if (!follow()) return
    open(t)
    add(t, { kind: 2, dt: 0, phase, x: Math.max(Math.round(e.clientX), 0), y: Math.max(Math.round(e.clientY), 0) })
  }

  return {
    start(options = {}) {
      try {
        const c = core.c()
        if (running || !c || typeof CompressionStream == 'undefined') return
        running = true
        opts = options
        K = 'la_' + c.token + '_r'
        endpoint = c.host.replace(/\/+$/, '') + '/v1/r'
        lastInput = now()

        const on = (target: EventTarget, type: string, f: (e: any) => void) => {
          const g = (e: Event) => {
            try {
              f(e)
            } catch {}
          }
          const o = { capture: true, passive: true }
          target.addEventListener(type, g, o)
          cleanup.push(() => target.removeEventListener(type, g, o))
        }
        const mark = () => {
          dirty = true
        }
        on(document, 'pointerdown', pointer(0))
        on(document, 'pointermove', pointer(1))
        on(document, 'pointerup', pointer(2))
        on(document, 'pointercancel', pointer(2))
        on(document, 'keydown', input)
        on(document, 'wheel', input)
        on(document, 'scroll', (e) => {
          const t = (lastInput = now())
          dirty = true
          // window scrolling is cheap to follow closely; element scrolling shows up in the next delta
          if (e.target == document && t - lastScroll >= 100 && follow()) syncScroll(t)
        })
        for (const type of ['input', 'change', 'focusin', 'focusout', 'load', 'transitionend', 'animationend'])
          on(document, type, mark)
        on(window, 'resize', mark)
        on(document, 'visibilitychange', () => {
          if (document.visibilityState == 'hidden') close(true)
          else lastInput = now()
        })
        on(window, 'pagehide', () => close(true))

        observer = new MutationObserver(mark)
        observer.observe(document.documentElement, {
          subtree: true,
          childList: true,
          attributes: true,
          characterData: true,
        })
        // SPA navigation: the core records a pageview, the replay names the new screen
        core.h.push((kind, name, props) => {
          if (running && !kind && name == 'pageview' && sid) {
            screen = (props.path as string) || location.pathname
            dirty = true
          }
        })
        timer = setInterval(() => {
          try {
            tick()
          } catch {}
        }, TICK_MS / 2)
      } catch {}
    },
    stop() {
      try {
        running = false
        clearInterval(timer)
        observer && observer.disconnect()
        for (const f of cleanup.splice(0)) f()
        close()
      } catch {}
    },
    active: () => running && recording && !off,
  }
}

/** Replay bound to the default client. */
export const replay: Replay = /* @__PURE__ */ createReplay(defaultClient)

export const startReplay = (options?: ReplayOptions): void => replay.start(options)
export const stopReplay = (): void => replay.stop()
