// Date-range picker: relative presets ("last N days", resolved at call time)
// or a custom pair of UTC days.
import { S } from '../i18n'
import { PRESET_DAYS, checkBounds, isDay, resolveBounds } from '../lib/dates'
import type { TimeBounds } from '../lib/types'
import { useId } from './ui'

export const rangeLabel = (b: TimeBounds): string => {
  if (b.range) {
    const m = /^(\d+)([dh])$/.exec(b.range)
    if (m) return m[2] == 'h' ? S.range.lastHours(Number(m[1])) : m[1] == '1' ? S.range.today : S.range.lastDays(Number(m[1]))
    return b.range
  }
  return `${b.from ?? '…'} – ${b.to ?? '…'}`
}

export const DateRange = ({
  value,
  onChange,
  hours,
  disabled,
}: {
  value: TimeBounds
  onChange: (value: TimeBounds) => void
  /** also offer "last 1 / 24 hours" (the live feed) */
  hours?: boolean
  disabled?: boolean
}) => {
  const id = useId('dr')
  const custom = !value.range
  const presets = [...(hours ? ['1h', '24h'] : []), ...PRESET_DAYS.map((d) => d + 'd')]
  if (value.range && !presets.includes(value.range)) presets.push(value.range)
  const bounds = custom ? { from: value.from ?? '', to: value.to ?? '' } : null
  const problem = bounds && isDay(bounds.from) && isDay(bounds.to) ? checkBounds(bounds) : bounds ? 'invalid' : null
  return (
    <div class="daterange">
      <label class="sr-only" for={id}>
        {S.range.label}
      </label>
      <select
        id={id}
        class="input select"
        disabled={disabled}
        value={custom ? 'custom' : value.range}
        onChange={(e) => {
          const v = e.currentTarget.value
          if (v == 'custom') onChange(resolveBounds(value, Date.now()))
          else onChange({ range: v })
        }}
      >
        {presets.map((p) => (
          <option value={p} key={p}>
            {rangeLabel({ range: p })}
          </option>
        ))}
        <option value="custom">{S.range.custom}</option>
      </select>
      {bounds && (
        <>
          <input
            type="date"
            class="input"
            aria-label={S.range.from}
            value={isDay(bounds.from) ? bounds.from : ''}
            max={bounds.to || undefined}
            disabled={disabled}
            onInput={(e) => onChange({ from: e.currentTarget.value, to: bounds.to })}
          />
          <span aria-hidden="true">–</span>
          <input
            type="date"
            class="input"
            aria-label={S.range.to}
            value={isDay(bounds.to) ? bounds.to : ''}
            min={bounds.from || undefined}
            disabled={disabled}
            onInput={(e) => onChange({ from: bounds.from, to: e.currentTarget.value })}
          />
        </>
      )}
      <span class="muted nowrap" title={S.range.utcHint}>
        UTC
      </span>
      {problem && <span class="field-error">{S.range.errors[problem]}</span>}
    </div>
  )
}
