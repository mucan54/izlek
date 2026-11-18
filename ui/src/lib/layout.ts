// Dashboard layout. Tiles live on a 12-column grid; the UI keeps them as an
// ordered list and derives `x`/`y` by packing rows left to right, so
// "reorder" and "resize" are simple list operations.
import type { DashboardItem } from './types'

export const COLS = 12
export const WIDTHS = [4, 6, 8, 12] as const
export const HEIGHTS = [2, 3, 4] as const

/** Reading order: top to bottom, then left to right. */
export const ordered = (items: DashboardItem[]): DashboardItem[] => [...items].sort((a, b) => a.y - b.y || a.x - b.x)

const clampW = (w: number) => Math.max(1, Math.min(COLS, Math.round(w) || 6))
const clampH = (h: number) => Math.max(1, Math.min(8, Math.round(h) || 3))

/** Assigns `x`/`y` to items in list order; a tile that does not fit starts a new row. */
export const pack = (items: DashboardItem[]): DashboardItem[] => {
  let x = 0
  let y = 0
  let rowH = 0
  return items.map((it) => {
    const w = clampW(it.w)
    const h = clampH(it.h)
    if (x + w > COLS) {
      x = 0
      y += rowH
      rowH = 0
    }
    const placed = { insight_id: it.insight_id, x, y, w, h }
    x += w
    rowH = Math.max(rowH, h)
    return placed
  })
}

export const moveItem = (items: DashboardItem[], index: number, delta: number): DashboardItem[] => {
  const to = index + delta
  if (to < 0 || to >= items.length) return items
  const next = [...items]
  const [it] = next.splice(index, 1)
  next.splice(to, 0, it)
  return pack(next)
}

export const resizeItem = (items: DashboardItem[], index: number, size: { w?: number; h?: number }): DashboardItem[] =>
  pack(items.map((it, i) => (i == index ? { ...it, ...size } : it)))

export const addItem = (items: DashboardItem[], insightId: string | number, w = 6, h = 3): DashboardItem[] =>
  pack([...items, { insight_id: insightId, x: 0, y: 0, w, h }])

export const removeItem = (items: DashboardItem[], index: number): DashboardItem[] => pack(items.filter((_, i) => i != index))
