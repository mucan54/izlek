// Feature flags: list with enable/disable, the rule editor, version history
// and the server-side "evaluate for a user" preview.
import { useEffect, useState } from 'preact/hooks'
import { S } from '../i18n'
import { asList } from '../lib/api'
import { FLAG_OPS, summarize } from '../lib/filter'
import { TOTAL_BP, bpToPct, emptyFlagForm, evenWeights, flagToForm, formToFlag, newRule, validateFlagForm, weightTotal, type FlagError, type FlagForm, type RuleForm } from '../lib/flagform'
import { fmtAgo, fmtDateTime } from '../lib/format'
import { useAction, useAsync } from '../lib/hooks'
import { can } from '../lib/roles'
import { Link, navigate } from '../lib/router'
import { useActorName, useApp, useProject } from '../lib/session'
import type { Flag, FlagVersion, Rule } from '../lib/types'
import { FilterEditor, flagSuggest } from '../components/FilterEditor'
import { AsyncView, Badge, Button, Card, ConfirmButton, Empty, ErrorNote, Field, Input, PageHead, Select, Spinner, Switch, Tabs, toast } from '../components/ui'

const words = () => ({ and: S.filter.andWord, or: S.filter.orWord, not: S.filter.notWord, ops: S.filter.opSymbols as Record<string, string> })

/** One-line description of a rule for the list and the history. */
export const ruleSummary = (r: Rule): string => {
  const when = r.when ? summarize(r.when, words()) : S.flags.everyone
  const rollout = '%' + bpToPct(r.rollout ?? TOTAL_BP)
  const variants = r.variants?.length ? ' → ' + r.variants.map((v) => `${v.key} %${bpToPct(v.w)}`).join(', ') : ''
  return `${when} · ${rollout}${variants}`
}

const defaultText = (d: Flag['default']): string => (typeof d == 'string' ? `"${d}"` : d === true ? 'true' : 'null')

const flagBody = (f: Flag, patch: Partial<Flag> = {}) => ({
  key: f.key,
  name: f.name,
  description: f.description,
  enabled: f.enabled,
  rules: f.rules,
  default: f.default,
  ...patch,
})

export const FlagList = () => {
  const project = useProject()
  const app = useApp()
  const canWrite = can(project.role, 'admin')
  const list = useAsync(async (signal) => asList<Flag>(await app.api.get(`${project.base}/flags`, { signal })), [project.base])
  const [busyKey, setBusyKey] = useState<string | null>(null)
  const toggle = async (f: Flag, enabled: boolean) => {
    setBusyKey(f.key)
    try {
      const updated = await app.api.put<Flag>(`${project.base}/flags/${encodeURIComponent(f.key)}`, flagBody(f, { enabled }))
      list.set((list.data ?? []).map((x) => (x.key == f.key ? (updated ?? { ...f, enabled }) : x)))
      toast(enabled ? S.flags.enabledToast(f.key) : S.flags.disabledToast(f.key))
    } catch (e) {
      toast((e as Error).message, 'error')
    }
    setBusyKey(null)
  }
  return (
    <>
      <PageHead title={S.flags.title} sub={S.flags.sub}>
        {canWrite && (
          <Link class="btn btn-primary" href={project.to('flags/~new')}>
            {S.flags.create}
          </Link>
        )}
      </PageHead>
      <AsyncView state={list} empty={<Empty title={S.flags.empty}>{canWrite ? S.flags.emptyHint : S.flags.emptyViewer}</Empty>}>
        {(flags) => (
          <Card>
            <div class="table-wrap">
              <table class="table">
                <thead>
                  <tr>
                    <th scope="col">{S.flags.state}</th>
                    <th scope="col">{S.flags.key}</th>
                    <th scope="col">{S.flags.rules}</th>
                    <th scope="col">{S.flags.default}</th>
                    <th scope="col" class="num">
                      {S.flags.version}
                    </th>
                    <th scope="col">{S.common.updated}</th>
                  </tr>
                </thead>
                <tbody>
                  {[...flags]
                    .sort((a, b) => a.key.localeCompare(b.key))
                    .map((f) => (
                      <tr key={f.key}>
                        <td class="nowrap">
                          {canWrite ? (
                            <Switch checked={f.enabled} disabled={busyKey == f.key} onChange={(v) => void toggle(f, v)} label={f.enabled ? S.common.on : S.common.off} />
                          ) : (
                            <Badge kind={f.enabled ? 'ok' : 'neutral'}>{f.enabled ? S.common.on : S.common.off}</Badge>
                          )}
                        </td>
                        <td>
                          <Link href={project.to('flags/' + encodeURIComponent(f.key))} class="mono">
                            {f.key}
                          </Link>
                          {f.name && <div class="muted">{f.name}</div>}
                        </td>
                        <td class="rules-cell">
                          {f.rules.length == 0 ? (
                            <span class="muted">{S.flags.noRules}</span>
                          ) : (
                            <ol class="rule-lines">
                              {f.rules.map((r, i) => (
                                <li key={i}>{ruleSummary(r)}</li>
                              ))}
                            </ol>
                          )}
                        </td>
                        <td class="mono">{defaultText(f.default)}</td>
                        <td class="num">v{f.version}</td>
                        <td class="nowrap">{fmtAgo(f.updated_at)}</td>
                      </tr>
                    ))}
                </tbody>
              </table>
            </div>
          </Card>
        )}
      </AsyncView>
    </>
  )
}

const errorText = (e: FlagError): string => (e.code == 'weights_sum' ? S.flags.errors.weights_sum(bpToPct(e.total ?? 0)) : S.flags.errors[e.code])

const RuleEditor = ({
  rule,
  index,
  count,
  errors,
  disabled,
  onChange,
  onMove,
  onRemove,
}: {
  rule: RuleForm
  index: number
  count: number
  errors: FlagError[]
  disabled: boolean
  onChange: (rule: RuleForm) => void
  onMove: (delta: number) => void
  onRemove: () => void
}) => {
  const { schema } = useProject()
  const at = `rules.${index}.`
  const err = (part: string) => errors.filter((e) => e.field == at + part).map(errorText)
  const total = weightTotal(rule.variants)
  const setVariant = (i: number, patch: Partial<RuleForm['variants'][number]>) => onChange({ ...rule, variants: rule.variants.map((v, k) => (k == i ? { ...v, ...patch } : v)) })
  const spread = (variants: RuleForm['variants']) => {
    const w = evenWeights(variants.length)
    return variants.map((v, i) => ({ ...v, pct: bpToPct(w[i]) }))
  }
  return (
    <li class="rule">
      <header class="rule-head">
        <h3>{S.flags.ruleN(index + 1)}</h3>
        <span class="muted">{index == 0 ? S.flags.firstMatch : S.flags.ifNoneAbove}</span>
        <span class="spacer" />
        {!disabled && (
          <>
            <button type="button" class="btn btn-ghost btn-sm" disabled={index == 0} aria-label={S.flags.moveUp(index + 1)} onClick={() => onMove(-1)}>
              ↑
            </button>
            <button type="button" class="btn btn-ghost btn-sm" disabled={index == count - 1} aria-label={S.flags.moveDown(index + 1)} onClick={() => onMove(1)}>
              ↓
            </button>
            <button type="button" class="btn btn-ghost btn-sm" aria-label={S.flags.removeRule(index + 1)} onClick={onRemove}>
              ✕
            </button>
          </>
        )}
      </header>
      <fieldset class="form-section">
        <legend>{S.flags.when}</legend>
        <p class="field-hint">{S.flags.whenHint}</p>
        <FilterEditor model={rule.when} onChange={(when) => onChange({ ...rule, when })} ops={FLAG_OPS} suggest={flagSuggest(schema)} disabled={disabled} />
        {err('when').map((m) => (
          <p class="field-error" key={m}>
            {m}
          </p>
        ))}
      </fieldset>
      <div class="form-grid form-grid-2">
        <Field label={S.flags.rollout} hint={S.flags.rolloutHint} error={err('rollout')[0]}>
          {(id, d) => (
            <div class="row">
              <Input id={id} class="input-num" value={rule.rolloutPct} onChange={(rolloutPct) => onChange({ ...rule, rolloutPct })} inputMode="decimal" aria-describedby={d} disabled={disabled} />
              <span>%</span>
            </div>
          )}
        </Field>
        <Field label={S.flags.stableOn} hint={S.flags.stableOnHint}>
          {(id, d) => (
            <Select
              id={id}
              aria-describedby={d}
              disabled={disabled}
              value={rule.stableOnAnon ? 'anon_id' : 'user'}
              onChange={(v) => onChange({ ...rule, stableOnAnon: v == 'anon_id' })}
              options={[
                ['user', S.flags.stableUser],
                ['anon_id', S.flags.stableAnon],
              ]}
            />
          )}
        </Field>
      </div>
      <fieldset class="form-section">
        <legend>{S.flags.variants}</legend>
        {rule.variants.length == 0 ? (
          <p class="field-hint">{S.flags.noVariants}</p>
        ) : (
          <>
            <ul class="variants">
              {rule.variants.map((v, i) => (
                <li key={i}>
                  <input class="input mono" value={v.key} placeholder={S.flags.variantKey} aria-label={S.flags.variantKeyN(i + 1)} disabled={disabled} onInput={(e) => setVariant(i, { key: e.currentTarget.value })} />
                  <input class="input input-num" value={v.pct} inputMode="decimal" aria-label={S.flags.variantWeightN(i + 1)} disabled={disabled} onInput={(e) => setVariant(i, { pct: e.currentTarget.value })} />
                  <span>%</span>
                  {!disabled && (
                    <button type="button" class="btn btn-ghost btn-sm" aria-label={S.flags.removeVariant(i + 1)} onClick={() => onChange({ ...rule, variants: rule.variants.filter((_, k) => k != i) })}>
                      ✕
                    </button>
                  )}
                </li>
              ))}
            </ul>
            <p class={`weights-total${total == TOTAL_BP ? '' : ' field-error'}`} role="status">
              {S.flags.weightsTotal(bpToPct(total))} {total == TOTAL_BP ? '✓' : S.flags.mustBe100}
            </p>
          </>
        )}
        {/* the sum itself is reported by the running total above */}
        {errors
          .filter((e) => e.field == at + 'variants' && e.code != 'weights_sum')
          .map(errorText)
          .map((m) => (
            <p class="field-error" key={m}>
              {m}
            </p>
          ))}
        {!disabled && (
          <div class="row">
            <Button
              small
              kind="ghost"
              onClick={() => {
                const names = ['control', 'test', 'c', 'd', 'e', 'f']
                const used = new Set(rule.variants.map((v) => v.key))
                const key = names.find((n) => !used.has(n)) ?? `v${rule.variants.length + 1}`
                // adding to an even split keeps it even; a hand-tuned split is left alone
                const wasEven = rule.variants.length == 0 || evenWeights(rule.variants.length).every((w, i) => bpToPct(w) == rule.variants[i].pct.trim())
                const next = [...rule.variants, { key, pct: '0' }]
                onChange({ ...rule, variants: wasEven ? spread(next) : next })
              }}
            >
              + {S.flags.addVariant}
            </Button>
            {rule.variants.length > 1 && (
              <Button small kind="ghost" onClick={() => onChange({ ...rule, variants: spread(rule.variants) })}>
                {S.flags.splitEvenly}
              </Button>
            )}
          </div>
        )}
      </fieldset>
    </li>
  )
}

const Preview = ({ flagKey, dirty }: { flagKey: string; dirty: boolean }) => {
  const project = useProject()
  const app = useApp()
  const [userId, setUserId] = useState('')
  const [anonId, setAnonId] = useState('')
  const [props, setProps] = useState('{\n  "platform": "ios",\n  "app_version": "3.2.0"\n}')
  const [result, setResult] = useState<{ value: unknown } | null>(null)
  let parsed: Record<string, unknown> | null = null
  let parseError = false
  try {
    const v = props.trim() ? (JSON.parse(props) as unknown) : {}
    if (v && typeof v == 'object' && !Array.isArray(v)) parsed = v as Record<string, unknown>
    else parseError = true
  } catch {
    parseError = true
  }
  const run = useAction(async () => {
    setResult(null)
    setResult(await app.api.post(`${project.base}/flags/${encodeURIComponent(flagKey)}/evaluate`, { props: parsed ?? {}, user_id: userId.trim(), anon_id: anonId.trim() }))
  })
  const value = result?.value
  return (
    <Card title={S.flags.previewTitle}>
      <form
        class="stack form-narrow"
        onSubmit={(e) => {
          e.preventDefault()
          if (!parseError) void run.run()
        }}
      >
        <p class="muted">{S.flags.previewIntro}</p>
        {dirty && (
          <p class="note note-warn" role="status">
            {S.flags.previewDirty}
          </p>
        )}
        <div class="form-grid form-grid-2">
          <Field label={S.persons.userId} hint={S.flags.previewUserHint}>
            {(id, d) => <Input id={id} name="user_id" value={userId} onChange={setUserId} aria-describedby={d} />}
          </Field>
          <Field label={S.persons.anonId}>{(id) => <Input id={id} name="anon_id" value={anonId} onChange={setAnonId} />}</Field>
        </div>
        <Field label={S.flags.previewProps} error={parseError ? S.flags.previewPropsError : null} hint={S.flags.previewPropsHint}>
          {(id, d) => <textarea id={id} class="input mono" rows={5} spellcheck={false} value={props} aria-describedby={d} aria-invalid={parseError} onInput={(e) => setProps(e.currentTarget.value)} />}
        </Field>
        <ErrorNote error={run.error} />
        <div class="row">
          <Button kind="primary" type="submit" busy={run.busy} disabled={parseError || (!userId.trim() && !anonId.trim())}>
            {S.flags.evaluate}
          </Button>
          {result && (
            <output class="eval-result" data-testid="eval-result">
              {S.flags.result}: <strong class="mono">{value === null || value === false || value === undefined ? 'null' : value === true ? 'true' : `"${String(value)}"`}</strong>{' '}
              <span class="muted">({value === null || value === false || value === undefined ? S.flags.resultOff : value === true ? S.flags.resultOn : S.flags.resultVariant})</span>
            </output>
          )}
        </div>
      </form>
    </Card>
  )
}

const History = ({ flagKey, stamp, onRestore, canWrite }: { flagKey: string; stamp: number; onRestore: (f: Flag) => void; canWrite: boolean }) => {
  const project = useProject()
  const app = useApp()
  const actorName = useActorName(project.project.org_id)
  const versions = useAsync(
    async (signal) => asList<FlagVersion>(await app.api.get(`${project.base}/flags/${encodeURIComponent(flagKey)}/versions`, { signal })).sort((a, b) => b.version - a.version),
    [project.base, flagKey, stamp],
  )
  return (
    <Card title={S.flags.historyTitle}>
      <AsyncView state={versions} empty={<p class="muted">{S.flags.historyEmpty}</p>}>
        {(list) => (
          <div class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th scope="col" class="num">
                    {S.flags.version}
                  </th>
                  <th scope="col">{S.common.date}</th>
                  <th scope="col">{S.flags.actor}</th>
                  <th scope="col">{S.flags.state}</th>
                  <th scope="col">{S.flags.rules}</th>
                  <th scope="col">
                    <span class="sr-only">{S.common.actions}</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {list.map((v, i) => (
                  <tr key={v.version}>
                    <td class="num">
                      v{v.version} {i == 0 && <Badge kind="accent">{S.flags.current}</Badge>}
                    </td>
                    <td class="nowrap">{fmtDateTime(v.ts)}</td>
                    <td>{actorName(v.actor)}</td>
                    <td>
                      <Badge kind={v.flag.enabled ? 'ok' : 'neutral'}>{v.flag.enabled ? S.common.on : S.common.off}</Badge>
                    </td>
                    <td class="rules-cell">
                      <ol class="rule-lines">
                        {(v.flag.rules ?? []).map((r, k) => (
                          <li key={k}>{ruleSummary(r)}</li>
                        ))}
                      </ol>
                      <span class="muted">
                        {S.flags.default}: <span class="mono">{defaultText(v.flag.default)}</span>
                      </span>
                    </td>
                    <td class="cell-action">
                      {canWrite && i > 0 && (
                        <Button small onClick={() => onRestore(v.flag)}>
                          {S.flags.restore}
                        </Button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </AsyncView>
    </Card>
  )
}

type Tab = 'definition' | 'preview' | 'history'

export const FlagEditor = ({ flagKey }: { flagKey?: string }) => {
  const project = useProject()
  const app = useApp()
  const isNew = !flagKey
  const canWrite = can(project.role, 'admin')
  const url = flagKey ? `${project.base}/flags/${encodeURIComponent(flagKey)}` : ''
  const loaded = useAsync<Flag | null>(async (signal) => (flagKey ? app.api.get<Flag>(url, { signal }) : null), [url])
  const [form, setForm] = useState<FlagForm>(emptyFlagForm)
  const [saved, setSaved] = useState('')
  const [flag, setFlag] = useState<Flag | null>(null)
  const [showErrors, setShowErrors] = useState(false)
  const [tab, setTab] = useState<Tab>('definition')

  const adopt = (f: Flag) => {
    const next = flagToForm(f)
    setFlag(f)
    setForm(next)
    setSaved(JSON.stringify(formToFlag(next)))
  }
  useEffect(() => {
    if (loaded.data) adopt(loaded.data)
  }, [loaded.data])

  const errors = validateFlagForm(form)
  const dirty = isNew || JSON.stringify(formToFlag(form)) != saved
  const top = (field: string) => (showErrors ? (errors.filter((e) => e.field == field).map(errorText)[0] ?? null) : null)

  const save = useAction(async () => {
    setShowErrors(true)
    if (errors.length) throw new Error(S.flags.fixErrors(errors.length))
    const body = formToFlag(form)
    if (isNew) {
      const created = await app.api.post<Flag>(`${project.base}/flags`, body)
      toast(S.flags.created)
      navigate(project.to('flags/' + encodeURIComponent(created.key)), true)
    } else {
      adopt(await app.api.put<Flag>(url, body))
      setShowErrors(false)
      toast(S.flags.savedToast)
    }
  })

  if (!isNew && loaded.error) return <ErrorNote error={loaded.error} onRetry={loaded.reload} />
  if (!isNew && !flag) return <Spinner />
  if (isNew && !canWrite) return <Empty title={S.flags.emptyViewer} />

  const setRule = (i: number, rule: RuleForm) => setForm({ ...form, rules: form.rules.map((r, k) => (k == i ? rule : r)) })
  const moveRule = (i: number, d: number) => {
    const rules = [...form.rules]
    const [r] = rules.splice(i, 1)
    rules.splice(i + d, 0, r)
    setForm({ ...form, rules })
  }

  return (
    <>
      <PageHead
        title={
          <span class="title-edit">
            <Link href={project.to('flags')} class="crumb">
              {S.flags.title}
            </Link>
            <span aria-hidden="true" class="crumb-sep">
              /
            </span>
            <span class="mono">{isNew ? S.flags.newFlag : flagKey}</span>
            {flag && <Badge kind={flag.enabled ? 'ok' : 'neutral'}>{flag.enabled ? S.common.on : S.common.off}</Badge>}
            {flag && <span class="muted small">v{flag.version}</span>}
          </span>
        }
      >
        {canWrite ? (
          <>
            {!isNew && dirty && <span class="muted">{S.insights.unsaved}</span>}
            <Button kind="primary" busy={save.busy} disabled={!dirty} onClick={() => void save.run()}>
              {isNew ? S.common.create : S.common.save}
            </Button>
            {!isNew && (
              <ConfirmButton
                label={S.common.delete}
                title={S.flags.deleteTitle}
                message={S.flags.deleteMessage(flagKey!)}
                onConfirm={async () => {
                  await app.api.del(url)
                  toast(S.flags.deleted)
                  navigate(project.to('flags'))
                }}
              />
            )}
          </>
        ) : (
          <span class="muted">{S.flags.readOnly}</span>
        )}
      </PageHead>
      {!isNew && (
        <Tabs
          label={S.flags.title}
          value={tab}
          onChange={setTab}
          tabs={[
            ['definition', S.flags.tabDefinition],
            ['preview', S.flags.tabPreview],
            ['history', S.flags.tabHistory],
          ]}
        />
      )}
      <ErrorNote error={save.error} />
      {tab == 'preview' && flagKey && <Preview flagKey={flagKey} dirty={dirty} />}
      {tab == 'history' && flagKey && (
        <History
          flagKey={flagKey}
          stamp={flag?.version ?? 0}
          canWrite={canWrite}
          onRestore={(old) => {
            // load the old definition into the form; nothing changes until it is saved
            const next = flagToForm({ ...old, key: flagKey })
            setForm(next)
            setTab('definition')
            toast(S.flags.restored)
          }}
        />
      )}
      {tab == 'definition' && (
        <form
          class="stack"
          onSubmit={(e) => {
            e.preventDefault()
            void save.run()
          }}
        >
          <Card title={S.flags.general}>
            <div class="form-grid form-grid-2">
              <Field label={S.flags.key} hint={isNew ? S.flags.keyHint : S.flags.keyFixed} error={top('key')}>
                {(id, d) => <Input id={id} name="key" class="mono" value={form.key} onChange={(key) => setForm({ ...form, key })} readOnly={!isNew} aria-describedby={d} maxLength={64} autocomplete="off" />}
              </Field>
              <Field label={S.flags.name}>{(id) => <Input id={id} name="name" value={form.name} onChange={(name) => setForm({ ...form, name })} disabled={!canWrite} maxLength={200} />}</Field>
            </div>
            <Field label={S.flags.description}>
              {(id) => <textarea id={id} class="input" rows={2} value={form.description} disabled={!canWrite} onInput={(e) => setForm({ ...form, description: e.currentTarget.value })} />}
            </Field>
            <div class="form-grid form-grid-2">
              <div class="field">
                <span class="field-label">{S.flags.state}</span>
                <Switch checked={form.enabled} onChange={(enabled) => setForm({ ...form, enabled })} label={form.enabled ? S.flags.enabledLabel : S.flags.disabledLabel} disabled={!canWrite} />
                <p class="field-hint">{S.flags.enabledHint}</p>
              </div>
              <Field label={S.flags.default} hint={S.flags.defaultHint} error={top('default')}>
                {(id, d) => (
                  <div class="row">
                    <Select
                      id={id}
                      aria-describedby={d}
                      disabled={!canWrite}
                      value={form.defaultKind}
                      onChange={(defaultKind) => setForm({ ...form, defaultKind })}
                      options={[
                        ['off', S.flags.defaultOff],
                        ['on', S.flags.defaultOn],
                        ['variant', S.flags.defaultVariant],
                      ]}
                    />
                    {form.defaultKind == 'variant' && (
                      <Input class="mono" value={form.defaultVariant} onChange={(defaultVariant) => setForm({ ...form, defaultVariant })} aria-label={S.flags.variantKey} placeholder={S.flags.variantKey} disabled={!canWrite} />
                    )}
                  </div>
                )}
              </Field>
            </div>
          </Card>
          <Card
            title={S.flags.rules}
            actions={
              canWrite && (
                <Button small onClick={() => setForm({ ...form, rules: [...form.rules, newRule()] })}>
                  + {S.flags.addRule}
                </Button>
              )
            }
          >
            <p class="muted">{S.flags.rulesIntro}</p>
            {form.rules.length == 0 ? (
              <Empty title={S.flags.noRules}>{S.flags.noRulesHint}</Empty>
            ) : (
              <ol class="rules">
                {form.rules.map((r, i) => (
                  <RuleEditor
                    key={r.id}
                    rule={r}
                    index={i}
                    count={form.rules.length}
                    errors={showErrors ? errors : errors.filter((e) => e.code == 'weights_sum')}
                    disabled={!canWrite}
                    onChange={(rule) => setRule(i, rule)}
                    onMove={(d) => moveRule(i, d)}
                    onRemove={() => setForm({ ...form, rules: form.rules.filter((_, k) => k != i) })}
                  />
                ))}
              </ol>
            )}
          </Card>
          {canWrite && (
            <div>
              <Button kind="primary" type="submit" busy={save.busy} disabled={!dirty}>
                {isNew ? S.flags.createSubmit : S.common.save}
              </Button>
            </div>
          )}
        </form>
      )}
    </>
  )
}
