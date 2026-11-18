// Hand-rolled SVG time chart (line or grouped bars) with axes, a legend,
// a hover/keyboard crosshair and a tooltip. Colours come from CSS classes
// (.s1 … .s8) so both themes are handled in the stylesheet.
import { useMemo, useState } from 'preact/hooks'
import { S } from '../i18n'
import { band, nearestIndex, niceAxis, pickLabels, scaleLinear } from '../lib/chart'
import { fmtAxis, fmtNum } from '../lib/format'
import { useWidth } from '../lib/hooks'

export interface ChartSeries {
  key: string
  label: string
  values: (number | null)[]
}

/** Categorical slots; a ninth series is never given a generated colour. */
export const MAX_SERIES = 8

interface Props {
  type: 'line' | 'bar'
  /** short x labels, one per bucket */
  labels: string[]
  /** long labels for the tooltip */
  titles?: string[]
  series: ChartSeries[]
  height?: number
  /** all values are whole numbers (counts): no fractional ticks */
  integer?: boolean
  ariaLabel: string
}

export const TimeChart = ({ type, labels, titles, series, height = 260, integer, ariaLabel }: Props) => {
  const [ref, width] = useWidth<HTMLDivElement>(720)
  const [hidden, setHidden] = useState<Set<string>>(new Set())
  const [hover, setHover] = useState<number | null>(null)

  // colour follows the series' slot, not its rank among the visible ones
  const slots = useMemo(() => series.slice(0, MAX_SERIES).map((s, i) => ({ ...s, slot: i + 1 })), [series])
  const shown = slots.filter((s) => !hidden.has(s.key))
  const n = labels.length

  const all = shown.flatMap((s) => s.values).filter((v): v is number => v != null)
  const axis = niceAxis(Math.min(0, ...all), Math.max(0, ...all), height > 200 ? 5 : 3, { integer })
  const tickText = fmtAxis(axis.ticks)
  const left = Math.max(28, Math.max(...tickText.map((t) => t.length)) * 7 + 12)
  const right = 12
  const top = 10
  const bottom = 26
  const x0 = left
  const x1 = Math.max(left + 40, width - right)
  const y = scaleLinear(axis.min, axis.max, height - bottom, top)
  const bands = band(n, x0, x1)
  const xLabels = pickLabels(n, Math.max(2, Math.floor((x1 - x0) / 76)))

  const path = (values: (number | null)[]): string => {
    let d = ''
    let pen = false
    values.forEach((v, i) => {
      if (v == null) {
        pen = false
        return
      }
      d += `${pen ? 'L' : 'M'}${bands.center(i).toFixed(1)},${y(v).toFixed(1)}`
      pen = true
    })
    return d
  }

  const groupWidth = Math.min(bands.step * 0.8, shown.length * 26)
  const barWidth = shown.length ? Math.max(1, Math.min(24, groupWidth / shown.length - 2)) : 0
  const bar = (i: number, k: number, v: number): string => {
    const cx = bands.center(i) - groupWidth / 2 + (groupWidth / shown.length) * (k + 0.5)
    const xa = cx - barWidth / 2
    const base = y(0)
    const tip = y(v)
    const r = Math.min(4, barWidth / 2, Math.abs(base - tip))
    const dir = tip <= base ? 1 : -1 // rounded data end, square at the baseline
    return `M${xa},${base}V${tip + r * dir}Q${xa},${tip} ${xa + r},${tip}H${xa + barWidth - r}Q${xa + barWidth},${tip} ${xa + barWidth},${tip + r * dir}V${base}Z`
  }

  const move = (clientX: number, target: Element) => {
    const box = target.getBoundingClientRect()
    setHover(nearestIndex(clientX - box.left, n, x0, x1))
  }

  const tipLeft = hover == null ? 0 : bands.center(hover)
  const flip = tipLeft > width * 0.6

  return (
    <div class="chart" ref={ref}>
      {slots.length > 1 && (
        <ul class="legend" aria-label={S.chart.legend}>
          {slots.map((s) => (
            <li key={s.key}>
              <button
                type="button"
                class={`legend-item${hidden.has(s.key) ? ' legend-off' : ''}`}
                aria-pressed={!hidden.has(s.key)}
                title={S.chart.toggle}
                onClick={() => {
                  const next = new Set(hidden)
                  if (next.has(s.key)) next.delete(s.key)
                  else next.add(s.key)
                  setHidden(next)
                }}
              >
                <span class={`swatch s${s.slot} ${type == 'line' ? 'swatch-line' : ''}`} aria-hidden="true" />
                {s.label}
              </button>
            </li>
          ))}
        </ul>
      )}
      <div class="chart-plot">
        <svg
          width={width}
          height={height}
          viewBox={`0 0 ${width} ${height}`}
          role="img"
          aria-label={ariaLabel}
          tabIndex={0}
          onMouseMove={(e) => move(e.clientX, e.currentTarget)}
          onMouseLeave={() => setHover(null)}
          onTouchStart={(e) => move(e.touches[0].clientX, e.currentTarget)}
          onTouchMove={(e) => move(e.touches[0].clientX, e.currentTarget)}
          onBlur={() => setHover(null)}
          onKeyDown={(e) => {
            if (e.key == 'ArrowRight' || e.key == 'ArrowLeft') {
              e.preventDefault()
              setHover((h) => Math.max(0, Math.min(n - 1, (h ?? (e.key == 'ArrowRight' ? -1 : n)) + (e.key == 'ArrowRight' ? 1 : -1))))
            } else if (e.key == 'Escape') setHover(null)
          }}
        >
          {axis.ticks.map((t, i) => (
            <g key={t}>
              <line class={t == 0 ? 'axis' : 'grid'} x1={x0} x2={x1} y1={Math.round(y(t)) + 0.5} y2={Math.round(y(t)) + 0.5} />
              <text class="tick" x={x0 - 8} y={y(t)} text-anchor="end" dominant-baseline="middle">
                {tickText[i]}
              </text>
            </g>
          ))}
          {xLabels.map((i) => (
            <text class="tick" key={i} x={bands.center(i)} y={height - 8} text-anchor="middle">
              {labels[i]}
            </text>
          ))}
          {hover != null && type == 'line' && <line class="crosshair" x1={tipLeft} x2={tipLeft} y1={top} y2={height - bottom} />}
          {hover != null && type == 'bar' && (
            <rect class="hover-band" x={bands.center(hover) - bands.step / 2} y={top} width={bands.step} height={height - bottom - top} />
          )}
          {type == 'line'
            ? shown.map((s) => (
                <g key={s.key} class={`s${s.slot}`}>
                  <path class="line" d={path(s.values)} />
                  {/* isolated points have no segment to show them; few buckets get dots throughout */}
                  {s.values.map((v, i) =>
                    v != null && (n <= 31 || (s.values[i - 1] == null && s.values[i + 1] == null)) ? (
                      <circle class="dot" key={i} cx={bands.center(i)} cy={y(v)} r={n <= 31 ? 2.5 : 2} />
                    ) : null,
                  )}
                  {hover != null && s.values[hover] != null && <circle class="dot dot-hover" cx={bands.center(hover)} cy={y(s.values[hover] as number)} r={4.5} />}
                </g>
              ))
            : shown.map((s, k) => (
                <g key={s.key} class={`s${s.slot}`}>
                  {s.values.map((v, i) => (v != null && v != 0 ? <path class="bar" key={i} d={bar(i, k, v)} /> : null))}
                </g>
              ))}
        </svg>
        {hover != null && shown.length > 0 && (
          <div class={`tooltip${flip ? ' tooltip-left' : ''}`} style={{ left: flip ? undefined : tipLeft + 12 + 'px', right: flip ? width - tipLeft + 12 + 'px' : undefined }} role="status">
            <p class="tooltip-title">{titles?.[hover] ?? labels[hover]}</p>
            <table>
              <tbody>
                {shown.map((s) => (
                  <tr key={s.key}>
                    <th>
                      <span class={`swatch s${s.slot}`} aria-hidden="true" />
                      {s.label}
                    </th>
                    <td>{fmtNum(s.values[hover])}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {series.length > MAX_SERIES && <p class="muted small">{S.chart.truncated(MAX_SERIES, series.length)}</p>}
    </div>
  )
}
