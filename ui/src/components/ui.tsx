// Small building blocks shared by every screen.
import type { ComponentChildren, JSX } from 'preact'
import { useEffect, useRef, useState } from 'preact/hooks'
import { LOCALE, LOCALE_OPTIONS, S, chooseLocale } from '../i18n'
import type { Async } from '../lib/hooks'
import { fmtBytes, fmtInt } from '../lib/format'
import type { Meta } from '../lib/types'

type ButtonProps = Omit<JSX.HTMLAttributes<HTMLButtonElement>, 'size'> & {
  kind?: 'default' | 'primary' | 'danger' | 'ghost'
  small?: boolean
  busy?: boolean
  disabled?: boolean
  type?: 'button' | 'submit'
  /** id of the form a submit button outside it belongs to */
  form?: string
  children?: ComponentChildren
}

export const Button = ({ kind = 'default', small, busy, disabled, type = 'button', children, class: cls, ...rest }: ButtonProps) => (
  <button
    {...rest}
    type={type}
    class={`btn btn-${kind}${small ? ' btn-sm' : ''}${cls ? ' ' + cls : ''}`}
    disabled={disabled || busy}
    aria-busy={busy || undefined}
  >
    {busy && <span class="spinner spinner-sm" aria-hidden="true" />}
    {children}
  </button>
)

export const Spinner = ({ label }: { label?: string }) => (
  <div class="loading" role="status">
    <span class="spinner" aria-hidden="true" />
    <span>{label ?? S.common.loading}</span>
  </div>
)

export const ErrorNote = ({ error, onRetry }: { error: Error | string | undefined; onRetry?: () => void }) =>
  error ? (
    <div class="note note-error" role="alert">
      <span>{typeof error == 'string' ? error : error.message || S.common.unknownError}</span>
      {onRetry && (
        <Button small onClick={onRetry}>
          {S.common.retry}
        </Button>
      )}
    </div>
  ) : null

export const Empty = ({ title, children }: { title: string; children?: ComponentChildren }) => (
  <div class="empty">
    <p class="empty-title">{title}</p>
    {children && <div class="empty-body">{children}</div>}
  </div>
)

/** Loading / error / empty / data, in that order of precedence. */
export const AsyncView = <T,>({
  state,
  empty,
  isEmpty,
  children,
}: {
  state: Async<T>
  empty?: ComponentChildren
  isEmpty?: (data: T) => boolean
  children: (data: T) => ComponentChildren
}) => {
  if (state.error) return <ErrorNote error={state.error} onRetry={state.reload} />
  if (state.data === undefined) return <Spinner />
  if (empty && (isEmpty ? isEmpty(state.data) : Array.isArray(state.data) && state.data.length == 0)) return <>{empty}</>
  return <div class={state.loading ? 'refreshing' : undefined}>{children(state.data)}</div>
}

let fieldId = 0
export const useId = (prefix = 'f'): string => useState(() => `${prefix}${++fieldId}`)[0]

/** Label + control + hint/error. The control is rendered by `children(id)`. */
export const Field = ({
  label,
  hint,
  error,
  children,
  class: cls,
}: {
  label: string
  hint?: ComponentChildren
  error?: string | null
  children: (id: string, describedBy: string | undefined) => ComponentChildren
  class?: string
}) => {
  const id = useId()
  const describedBy = error ? id + '-e' : hint ? id + '-h' : undefined
  return (
    <div class={`field${error ? ' field-invalid' : ''}${cls ? ' ' + cls : ''}`}>
      <label for={id}>{label}</label>
      {children(id, describedBy)}
      {error ? (
        <p class="field-error" id={id + '-e'}>
          {error}
        </p>
      ) : hint ? (
        <p class="field-hint" id={id + '-h'}>
          {hint}
        </p>
      ) : null}
    </div>
  )
}

type InputProps = Omit<JSX.HTMLAttributes<HTMLInputElement>, 'onInput' | 'value' | 'onChange'> & {
  value: string
  onChange: (value: string) => void
  type?: string
  list?: string
  name?: string
  placeholder?: string
  required?: boolean
  disabled?: boolean
  readOnly?: boolean
  autocomplete?: string
  min?: number | string
  max?: number | string
  step?: number | string
  minLength?: number
  maxLength?: number
  inputMode?: string
  pattern?: string
  /** function components do not receive `ref` */
  inputRef?: (el: HTMLInputElement | null) => void
}
export const Input = ({ value, onChange, class: cls, inputRef, ...rest }: InputProps) => (
  <input
    {...(rest as JSX.HTMLAttributes<HTMLInputElement>)}
    ref={inputRef}
    class={`input${cls ? ' ' + cls : ''}`}
    value={value}
    onInput={(e) => onChange(e.currentTarget.value)}
  />
)

/** Stable ref callback that focuses an input once, when it first mounts. */
export const focusOnMount = (el: HTMLInputElement | null): void => {
  if (el && !el.dataset.focused) {
    el.dataset.focused = '1'
    el.focus()
  }
}

export const Select = <V extends string>({
  value,
  onChange,
  options,
  class: cls,
  ...rest
}: {
  value: V
  onChange: (value: V) => void
  options: readonly (readonly [V, string])[]
  class?: string
  id?: string
  disabled?: boolean
  'aria-label'?: string
  'aria-describedby'?: string
}) => (
  <select {...rest} class={`input select${cls ? ' ' + cls : ''}`} value={value} onChange={(e) => onChange(e.currentTarget.value as V)}>
    {options.map(([v, label]) => (
      <option value={v} key={v}>
        {label}
      </option>
    ))}
  </select>
)

/** Set while a language change is rebuilding the screen: the new switch takes the focus back. */
let refocusLanguage = false

/**
 * Language switch. The choice is stored (`la_locale`) and the app re-renders
 * in the new language at once (see `App`). Languages are listed by their own
 * names so they can be found by someone who cannot read the current one.
 * The caller renders the `<label for={id}>`.
 */
export const LanguageSelect = ({ id, describedBy }: { id: string; describedBy?: string }) => (
  <select
    id={id}
    name="locale"
    class="input select"
    data-testid="lang-switch"
    aria-describedby={describedBy}
    value={LOCALE}
    // the switch itself is rebuilt with the rest of the screen: keep the keyboard user where they were
    ref={(el) => {
      if (el && refocusLanguage) {
        refocusLanguage = false
        el.focus()
      }
    }}
    onChange={(e) => {
      const code = e.currentTarget.value
      refocusLanguage = code != LOCALE && document.activeElement == e.currentTarget
      chooseLocale(code)
    }}
  >
    {LOCALE_OPTIONS.map(([code, name]) => (
      <option value={code} lang={code} key={code}>
        {name}
      </option>
    ))}
  </select>
)

export const Switch = ({
  checked,
  onChange,
  label,
  disabled,
  hideLabel,
}: {
  checked: boolean
  onChange: (checked: boolean) => void
  label: string
  disabled?: boolean
  hideLabel?: boolean
}) => (
  <label class={`switch${disabled ? ' switch-disabled' : ''}`}>
    <input type="checkbox" role="switch" checked={checked} disabled={disabled} aria-label={hideLabel ? label : undefined} onChange={(e) => onChange(e.currentTarget.checked)} />
    <span class="switch-track" aria-hidden="true" />
    {!hideLabel && <span>{label}</span>}
  </label>
)

export const Checkbox = ({ checked, onChange, label, disabled }: { checked: boolean; onChange: (checked: boolean) => void; label: string; disabled?: boolean }) => (
  <label class="check">
    <input type="checkbox" checked={checked} disabled={disabled} onChange={(e) => onChange(e.currentTarget.checked)} />
    <span>{label}</span>
  </label>
)

/** Native modal dialog: focus is trapped and Esc closes it. */
export const Modal = ({
  title,
  onClose,
  children,
  footer,
  wide,
}: {
  title: string
  onClose: () => void
  children: ComponentChildren
  footer?: ComponentChildren
  wide?: boolean
}) => {
  const ref = useRef<HTMLDialogElement>(null)
  const id = useId('dlg')
  useEffect(() => {
    const d = ref.current
    if (d && !d.open) d.showModal()
    return () => d?.close()
  }, [])
  return (
    <dialog
      ref={ref}
      class={`modal${wide ? ' modal-wide' : ''}`}
      aria-labelledby={id}
      onCancel={(e) => {
        e.preventDefault()
        onClose()
      }}
      onClick={(e) => {
        if (e.target == ref.current) onClose()
      }}
    >
      <div class="modal-box">
        <header class="modal-head">
          <h2 id={id}>{title}</h2>
          <button type="button" class="btn btn-ghost btn-sm" onClick={onClose} aria-label={S.common.close}>
            ✕
          </button>
        </header>
        <div class="modal-body">{children}</div>
        {footer && <footer class="modal-foot">{footer}</footer>}
      </div>
    </dialog>
  )
}

/** A button that asks before running a destructive action. */
export const ConfirmButton = ({
  label,
  title,
  message,
  confirmLabel,
  onConfirm,
  small,
  kind = 'danger',
  disabled,
}: {
  label: string
  title: string
  message: ComponentChildren
  confirmLabel?: string
  onConfirm: () => Promise<unknown>
  small?: boolean
  kind?: 'danger' | 'default' | 'ghost'
  disabled?: boolean
}) => {
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<Error>()
  return (
    <>
      <Button kind={kind} small={small} disabled={disabled} onClick={() => setOpen(true)}>
        {label}
      </Button>
      {open && (
        <Modal
          title={title}
          onClose={() => setOpen(false)}
          footer={
            <>
              <Button onClick={() => setOpen(false)}>{S.common.cancel}</Button>
              <Button
                kind="danger"
                busy={busy}
                onClick={async () => {
                  setBusy(true)
                  setError(undefined)
                  try {
                    await onConfirm()
                    setOpen(false)
                  } catch (e) {
                    setError(e as Error)
                  }
                  setBusy(false)
                }}
              >
                {confirmLabel ?? label}
              </Button>
            </>
          }
        >
          <div class="stack">
            <div>{message}</div>
            <ErrorNote error={error} />
          </div>
        </Modal>
      )}
    </>
  )
}

export const Badge = ({ kind = 'neutral', children }: { kind?: 'neutral' | 'ok' | 'warn' | 'danger' | 'accent'; children: ComponentChildren }) => (
  <span class={`badge badge-${kind}`}>{children}</span>
)

export const CopyButton = ({ text, label, testId }: { text: string; label?: string; testId?: string }) => {
  const [done, setDone] = useState(false)
  return (
    <Button
      small
      data-testid={testId}
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text)
          setDone(true)
          setTimeout(() => setDone(false), 1500)
        } catch {
          toast(S.common.copyFailed, 'error')
        }
      }}
    >
      {done ? S.common.copied : (label ?? S.common.copy)}
    </Button>
  )
}

export const PageHead = ({ title, sub, children }: { title: ComponentChildren; sub?: ComponentChildren; children?: ComponentChildren }) => (
  <header class="page-head">
    <div class="page-title">
      <h1>{title}</h1>
      {sub && <p class="page-sub">{sub}</p>}
    </div>
    {children && <div class="page-actions">{children}</div>}
  </header>
)

export const Card = ({ title, actions, children, class: cls }: { title?: ComponentChildren; actions?: ComponentChildren; children: ComponentChildren; class?: string }) => (
  <section class={`card${cls ? ' ' + cls : ''}`}>
    {(title || actions) && (
      <header class="card-head">
        {title && <h2>{title}</h2>}
        {actions && <div class="card-actions">{actions}</div>}
      </header>
    )}
    <div class="card-body">{children}</div>
  </section>
)

export const Tabs = <V extends string>({ value, onChange, tabs, label }: { value: V; onChange: (v: V) => void; tabs: readonly (readonly [V, string])[]; label: string }) => (
  <div class="tabs" role="tablist" aria-label={label}>
    {tabs.map(([v, text]) => (
      <button type="button" role="tab" key={v} aria-selected={v == value} class={`tab${v == value ? ' tab-active' : ''}`} onClick={() => onChange(v)}>
        {text}
      </button>
    ))}
  </div>
)

/** Query cost line under a result: elapsed, scanned bytes, cache hit. */
export const MetaLine = ({ meta }: { meta: Meta | undefined }) =>
  meta ? (
    <p class="meta-line">
      {fmtInt(meta.elapsed_ms)} ms
      {meta.cached ? (
        <> · {S.query.cached}</>
      ) : (
        <>
          {' '}
          · {fmtBytes(meta.bytes_scanned)} · {S.query.files(meta.files)}
        </>
      )}
    </p>
  ) : null

// ---- toasts ----

interface Toast {
  id: number
  text: string
  kind: 'info' | 'error'
}
let toasts: Toast[] = []
let toastSeq = 0
const toastListeners = new Set<() => void>()
export const toast = (text: string, kind: 'info' | 'error' = 'info'): void => {
  const t = { id: ++toastSeq, text, kind }
  toasts = [...toasts, t]
  toastListeners.forEach((l) => l())
  setTimeout(() => {
    toasts = toasts.filter((x) => x != t)
    toastListeners.forEach((l) => l())
  }, kind == 'error' ? 6000 : 3000)
}

export const Toaster = () => {
  const [, bump] = useState(0)
  useEffect(() => {
    const l = () => bump((n) => n + 1)
    toastListeners.add(l)
    return () => void toastListeners.delete(l)
  }, [])
  return (
    <div class="toasts" aria-live="polite">
      {toasts.map((t) => (
        <div key={t.id} class={`toast toast-${t.kind}`} role={t.kind == 'error' ? 'alert' : 'status'}>
          {t.text}
        </div>
      ))}
    </div>
  )
}
