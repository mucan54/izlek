// Canvas renderer for a replay scene: wireframe rectangles by node type, text
// (masked text as bullets), the current touch gesture and a screen banner.
import { BUTTON, FOCUSED, HAS_COLOR, HAS_TEXT, IMAGE, INPUT, MASKED, SCROLL, TEXT, CONTAINER, type RNode } from './lar1'
import type { PlayerState } from './player'

export interface DrawOptions {
  /** CSS pixels available for the device screen */
  width: number
  height: number
  dpr: number
  /** label prefix for the screen banner, e.g. "Ekran" */
  screenLabel: string
  font: string
}

// The device screen is drawn like paper in both UI themes: a recording is
// content, not chrome, and captured colours assume a light backdrop.
const PAPER = '#ffffff'
const INK = '#1d2126'
const MUTED = '#7b838c'
const LINE = '#c9ced4'
const FILL = '#eef0f2'
const ACCENT = '#256abf'

const css = (argb: number): string => {
  const a = ((argb >>> 24) & 255) / 255
  return `rgba(${(argb >>> 16) & 255},${(argb >>> 8) & 255},${argb & 255},${Math.round(a * 1000) / 1000})`
}

/** Black or white, whichever reads better on the given fill. */
const inkOn = (argb: number): string => {
  const r = (argb >>> 16) & 255
  const g = (argb >>> 8) & 255
  const b = argb & 255
  return (r * 299 + g * 587 + b * 114) / 1000 > 150 ? INK : '#ffffff'
}

const rounded = (ctx: CanvasRenderingContext2D, x: number, y: number, w: number, h: number, r: number): void => {
  const k = Math.max(0, Math.min(r, w / 2, h / 2))
  ctx.beginPath()
  ctx.moveTo(x + k, y)
  ctx.arcTo(x + w, y, x + w, y + h, k)
  ctx.arcTo(x + w, y + h, x, y + h, k)
  ctx.arcTo(x, y + h, x, y, k)
  ctx.arcTo(x, y, x + w, y, k)
  ctx.closePath()
}

/** Text clipped to `max` px with an ellipsis. */
const fit = (ctx: CanvasRenderingContext2D, text: string, max: number): string => {
  if (ctx.measureText(text).width <= max) return text
  let lo = 0
  let hi = text.length
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1
    if (ctx.measureText(text.slice(0, mid) + '…').width <= max) lo = mid
    else hi = mid - 1
  }
  return lo > 0 ? text.slice(0, lo) + '…' : ''
}

const label = (n: RNode): string => {
  if (!(n.flags & HAS_TEXT) || !n.text) return ''
  // capture already replaced masked text; make sure nothing readable slips through
  return n.flags & MASKED ? '•'.repeat([...n.text].length) : n.text.replace(/\s+/g, ' ')
}

const drawNode = (ctx: CanvasRenderingContext2D, n: RNode, font: string): void => {
  const { x, y, w, h } = n
  if (w <= 0 || h <= 0) return
  const color = n.flags & HAS_COLOR && n.color !== undefined ? n.color : undefined
  const text = label(n)
  const size = Math.max(9, Math.min(h <= 30 ? h * 0.6 : 15, 16))
  ctx.font = `${n.type == BUTTON ? 600 : 400} ${size}px ${font}`
  ctx.textBaseline = 'middle'
  ctx.lineWidth = 1

  switch (n.type) {
    case TEXT: {
      if (text) {
        ctx.fillStyle = n.flags & MASKED ? MUTED : color !== undefined ? css(color) : INK
        ctx.textAlign = 'left'
        // one line per ~1.35 × font size; long paragraphs become stacked lines
        const lines = Math.max(1, Math.min(6, Math.floor(h / (size * 1.35))))
        if (lines == 1) ctx.fillText(fit(ctx, text, w), x, y + h / 2)
        else {
          const words = text.split(' ')
          let line = ''
          let row = 0
          const flush = (s: string, last: boolean) => ctx.fillText(last ? fit(ctx, s, w) : s, x, y + size * 0.7 + row++ * size * 1.35)
          for (let i = 0; i < words.length && row < lines; i++) {
            const next = line ? line + ' ' + words[i] : words[i]
            if (ctx.measureText(next).width > w && line) {
              if (row == lines - 1) {
                flush(line + ' ' + words.slice(i).join(' '), true)
                line = ''
                break
              }
              flush(line, false)
              line = words[i]
            } else line = next
          }
          if (line && row < lines) flush(line, true)
        }
      } else {
        // text without content: a grey bar, like a skeleton line
        ctx.fillStyle = FILL
        rounded(ctx, x, y + h * 0.25, w, h * 0.5, 3)
        ctx.fill()
      }
      break
    }
    case IMAGE: {
      ctx.fillStyle = FILL
      ctx.strokeStyle = LINE
      ctx.fillRect(x, y, w, h)
      ctx.strokeRect(x + 0.5, y + 0.5, w - 1, h - 1)
      ctx.beginPath()
      ctx.moveTo(x, y)
      ctx.lineTo(x + w, y + h)
      ctx.moveTo(x + w, y)
      ctx.lineTo(x, y + h)
      ctx.stroke()
      break
    }
    case BUTTON: {
      const fill = color ?? 0xffe3e7eb
      ctx.fillStyle = css(fill)
      rounded(ctx, x, y, w, h, 6)
      ctx.fill()
      if (color === undefined) {
        ctx.strokeStyle = LINE
        ctx.stroke()
      }
      if (text) {
        ctx.fillStyle = inkOn(fill)
        ctx.textAlign = 'center'
        ctx.fillText(fit(ctx, text, w - 12), x + w / 2, y + h / 2)
      }
      break
    }
    case INPUT: {
      ctx.fillStyle = color !== undefined ? css(color) : PAPER
      rounded(ctx, x, y, w, h, 4)
      ctx.fill()
      ctx.strokeStyle = n.flags & FOCUSED ? ACCENT : LINE
      ctx.lineWidth = n.flags & FOCUSED ? 2 : 1
      rounded(ctx, x + 0.5, y + 0.5, w - 1, h - 1, 4)
      ctx.stroke()
      if (text) {
        ctx.fillStyle = n.flags & MASKED ? MUTED : INK
        ctx.textAlign = 'left'
        ctx.fillText(fit(ctx, text, w - 16), x + 8, y + h / 2)
      }
      break
    }
    case SCROLL:
      // the scrolled content itself: usually far larger than the viewport, so only tint it
      if (color !== undefined) {
        ctx.fillStyle = css(color)
        ctx.fillRect(x, y, w, h)
      }
      break
    default: {
      if (color !== undefined) {
        ctx.fillStyle = css(color)
        ctx.fillRect(x, y, w, h)
      } else if (n.type != CONTAINER) {
        ctx.fillStyle = FILL
        ctx.fillRect(x, y, w, h)
      }
      if (color === undefined || n.type != CONTAINER) {
        ctx.strokeStyle = LINE
        ctx.strokeRect(x + 0.5, y + 0.5, w - 1, h - 1)
      }
      if (text) {
        ctx.fillStyle = color !== undefined ? inkOn(color) : INK
        ctx.textAlign = 'left'
        ctx.fillText(fit(ctx, text, w - 8), x + 4, y + h / 2)
      }
      if (n.flags & FOCUSED) {
        ctx.strokeStyle = ACCENT
        ctx.lineWidth = 2
        ctx.strokeRect(x + 1, y + 1, w - 2, h - 2)
      }
    }
  }
}

const BANNER_MS = 1500
const TOUCH_FADE_MS = 450

/** Scale that fits a `w × h` device into the available box. */
export const fitScale = (w: number, h: number, boxW: number, boxH: number): number =>
  w > 0 && h > 0 ? Math.min(boxW / w, boxH / h) : 1

/**
 * Draws the state. The canvas must already be sized to
 * `scene.w × scene.h × scale × dpr` device pixels by the caller.
 */
export const draw = (ctx: CanvasRenderingContext2D, state: PlayerState, scale: number, options: DrawOptions): void => {
  const { scene } = state
  const w = scene.w || 1
  const h = scene.h || 1
  ctx.setTransform(1, 0, 0, 1, 0, 0)
  ctx.clearRect(0, 0, ctx.canvas.width, ctx.canvas.height)
  const k = scale * options.dpr
  ctx.setTransform(k, 0, 0, k, 0, 0)
  ctx.fillStyle = PAPER
  ctx.fillRect(0, 0, w, h)

  ctx.save()
  ctx.beginPath()
  ctx.rect(0, 0, w, h)
  ctx.clip()
  for (const n of scene.nodes.values()) {
    // off-screen nodes are common (scrolled content); skip them early
    if (n.x >= w || n.y >= h || n.x + n.w <= 0 || n.y + n.h <= 0) continue
    drawNode(ctx, n, options.font)
  }

  // touch: trail of the gesture plus a ring that fades after the finger lifts
  const g = state.gesture
  if (g.length) {
    const last = g[g.length - 1]
    const age = state.time - last.t
    const alpha = last.phase == 2 ? Math.max(0, 1 - age / TOUCH_FADE_MS) : 1
    if (alpha > 0) {
      ctx.globalAlpha = alpha
      if (g.length > 1) {
        ctx.strokeStyle = ACCENT
        ctx.lineWidth = 3
        ctx.lineJoin = 'round'
        ctx.lineCap = 'round'
        ctx.beginPath()
        ctx.moveTo(g[0].x, g[0].y)
        for (const p of g) ctx.lineTo(p.x, p.y)
        ctx.stroke()
      }
      const r = last.phase == 2 ? 14 + (age / TOUCH_FADE_MS) * 10 : 14
      ctx.fillStyle = 'rgba(37,106,191,0.25)'
      ctx.strokeStyle = ACCENT
      ctx.lineWidth = 2
      ctx.beginPath()
      ctx.arc(last.x, last.y, r, 0, Math.PI * 2)
      ctx.fill()
      ctx.stroke()
      ctx.globalAlpha = 1
    }
  }

  // screen-change banner
  if (scene.screen && state.time - state.screenAt < BANNER_MS) {
    const text = options.screenLabel + ': ' + scene.screen
    ctx.font = `600 13px ${options.font}`
    ctx.textBaseline = 'middle'
    ctx.textAlign = 'center'
    const tw = Math.min(w - 24, ctx.measureText(text).width + 24)
    ctx.globalAlpha = Math.min(1, (BANNER_MS - (state.time - state.screenAt)) / 300)
    ctx.fillStyle = 'rgba(29,33,38,0.88)'
    rounded(ctx, (w - tw) / 2, 10, tw, 26, 13)
    ctx.fill()
    ctx.fillStyle = '#ffffff'
    ctx.fillText(fit(ctx, text, tw - 16), w / 2, 23)
    ctx.globalAlpha = 1
  }
  ctx.restore()
}
