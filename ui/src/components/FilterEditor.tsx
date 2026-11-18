// Editor for the and/or/not expression tree used by query filters and flag
// rule conditions. `FilterEditor` is controlled by the editable model;
// `ExprFilter` wraps it for callers that only care about the JSON.
import { useEffect, useMemo, useRef, useState } from 'preact/hooks'
import { S } from '../i18n'
import {
  COLUMNS, addChild, countConds, fromExpr, needsValue, newCond, newGroup, toExpr, updateNode,
  type CondModel, type FilterModel, type GroupModel, type Op,
} from '../lib/filter'
import type { Expr, Schema } from '../lib/types'
import { Button, useId } from './ui'

export interface Suggest {
  /** property references offered by autocomplete */
  props: string[]
  /** known values per property (e.g. event names for `event`) */
  values?: Record<string, string[]>
}

/** Autocomplete for query filters: fixed columns, `props.*`, `person.*`, event names. */
export const querySuggest = (schema: Schema | null): Suggest => ({
  props: [...COLUMNS, ...(schema?.props ?? []).map((p) => 'props.' + p.key), ...(schema?.person_props ?? []).map((k) => 'person.' + k)],
  values: { event: schema?.events ?? [], platform: ['web', 'ios', 'android'] },
})

/** Autocomplete for flag conditions: flat keys the SDKs know about. */
export const flagSuggest = (schema: Schema | null): Suggest => ({
  props: [
    'platform', 'os', 'os_version', 'app_version', 'device_type', 'device_model', 'locale', 'country', 'user_id', 'anon_id',
    ...(schema?.person_props ?? []),
  ],
  values: { platform: ['web', 'ios', 'android'] },
})

interface EditorProps {
  model: GroupModel
  onChange: (model: GroupModel) => void
  ops: readonly Op[]
  suggest: Suggest
  disabled?: boolean
}

export const FilterEditor = ({ model, onChange, ops, suggest, disabled }: EditorProps) => {
  const listId = useId('fl')
  const valueLists = Object.entries(suggest.values ?? {}).filter(([, v]) => v.length)
  const lastAdded = useRef<number | null>(null)
  const root = useRef<HTMLDivElement>(null)

  // focus the property input of a freshly added condition
  useEffect(() => {
    if (lastAdded.current == null) return
    root.current?.querySelector<HTMLInputElement>(`[data-cond="${lastAdded.current}"] input`)?.focus()
    lastAdded.current = null
  })

  const patch = (id: number, fn: (n: FilterModel) => FilterModel | null) => onChange(updateNode(model, id, fn))

  const cond = (c: CondModel) => (
    <div class="filter-cond" key={c.id} data-cond={c.id}>
      <input
        class="input filter-prop"
        value={c.prop}
        list={listId}
        placeholder={S.filter.prop}
        aria-label={S.filter.prop}
        disabled={disabled}
        onInput={(e) => patch(c.id, (n) => ({ ...(n as CondModel), prop: e.currentTarget.value }))}
      />
      <select
        class="input select filter-op"
        value={c.op}
        aria-label={S.filter.op}
        disabled={disabled}
        onChange={(e) => patch(c.id, (n) => ({ ...(n as CondModel), op: e.currentTarget.value as Op }))}
      >
        {ops.map((op) => (
          <option value={op} key={op}>
            {S.filter.ops[op]}
          </option>
        ))}
      </select>
      {needsValue(c.op) ? (
        <input
          class="input filter-value"
          value={c.value}
          list={suggest.values?.[c.prop.trim()]?.length ? `${listId}-${c.prop.trim()}` : undefined}
          placeholder={c.op == 'in' || c.op == 'not_in' ? S.filter.listPlaceholder : S.filter.value}
          aria-label={S.filter.value}
          disabled={disabled}
          onInput={(e) => patch(c.id, (n) => ({ ...(n as CondModel), value: e.currentTarget.value }))}
        />
      ) : (
        <span class="filter-value" />
      )}
      {!disabled && (
        <button type="button" class="btn btn-ghost btn-sm" aria-label={S.filter.removeCond} title={S.filter.removeCond} onClick={() => patch(c.id, () => null)}>
          ✕
        </button>
      )}
    </div>
  )

  const group = (g: GroupModel, isRoot: boolean) => (
    <div class={`filter-group${isRoot ? ' filter-root' : ''}`} key={g.id}>
      {(g.children.length > 1 || g.negate || !isRoot) && (
        <div class="filter-group-head">
          <select
            class="input select"
            value={g.join}
            aria-label={S.filter.join}
            disabled={disabled}
            onChange={(e) => patch(g.id, (n) => ({ ...(n as GroupModel), join: e.currentTarget.value as 'and' | 'or' }))}
          >
            <option value="and">{S.filter.all}</option>
            <option value="or">{S.filter.any}</option>
          </select>
          <label class="check">
            <input type="checkbox" checked={g.negate} disabled={disabled} onChange={(e) => patch(g.id, (n) => ({ ...(n as GroupModel), negate: e.currentTarget.checked }))} />
            <span>{S.filter.negate}</span>
          </label>
          {!isRoot && !disabled && (
            <button type="button" class="btn btn-ghost btn-sm filter-group-remove" onClick={() => patch(g.id, () => null)}>
              {S.filter.removeGroup}
            </button>
          )}
        </div>
      )}
      {g.children.map((c) => (c.kind == 'cond' ? cond(c) : group(c, false)))}
      {!disabled && (
        <div class="filter-add">
          <Button
            small
            kind="ghost"
            onClick={() => {
              const c = newCond()
              lastAdded.current = c.id
              onChange(addChild(model, g.id, c))
            }}
          >
            + {S.filter.addCond}
          </Button>
          <Button
            small
            kind="ghost"
            onClick={() => {
              const c = newCond()
              lastAdded.current = c.id
              onChange(addChild(model, g.id, newGroup(g.join == 'and' ? 'or' : 'and', [c])))
            }}
          >
            + {S.filter.addGroup}
          </Button>
        </div>
      )}
    </div>
  )

  return (
    <div class="filter" ref={root}>
      <datalist id={listId}>
        {suggest.props.map((p) => (
          <option value={p} key={p} />
        ))}
      </datalist>
      {valueLists.map(([prop, values]) => (
        <datalist id={`${listId}-${prop}`} key={prop}>
          {values.map((v) => (
            <option value={v} key={v} />
          ))}
        </datalist>
      ))}
      {group(model, true)}
    </div>
  )
}

interface ExprProps {
  value: Expr | undefined
  onChange: (value: Expr | undefined) => void
  ops: readonly Op[]
  suggest: Suggest
  /** change to rebuild the editor from `value` (e.g. another insight was loaded) */
  resetKey?: unknown
  disabled?: boolean
}

/** Keeps the editable model (including half-typed rows) and reports the JSON. */
export const ExprFilter = ({ value, onChange, ops, suggest, resetKey, disabled }: ExprProps) => {
  const [model, setModel] = useState(() => fromExpr(value))
  const key = useRef(resetKey)
  useEffect(() => {
    if (key.current === resetKey) return
    key.current = resetKey
    setModel(fromExpr(value))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [resetKey])
  return (
    <FilterEditor
      model={model}
      ops={ops}
      suggest={suggest}
      disabled={disabled}
      onChange={(m) => {
        setModel(m)
        onChange(toExpr(m))
      }}
    />
  )
}

/** A "Filtre (n)" disclosure around an ExprFilter, for per-series and per-step filters. */
export const InlineFilter = (props: ExprProps & { label?: string }) => {
  const count = useMemo(() => (props.value ? countConds(fromExpr(props.value)) : 0), [props.value])
  const [open, setOpen] = useState(count > 0)
  const id = useId('if')
  return (
    <div class="inline-filter">
      <button type="button" class={`btn btn-ghost btn-sm${count ? ' has-filter' : ''}`} aria-expanded={open} aria-controls={id} onClick={() => setOpen(!open)}>
        {props.label ?? S.filter.title}
        {count > 0 && <span class="count">{count}</span>}
        <span aria-hidden="true">{open ? ' ▴' : ' ▾'}</span>
      </button>
      {open && (
        <div id={id} class="inline-filter-body">
          <ExprFilter {...props} />
        </div>
      )}
    </div>
  )
}
