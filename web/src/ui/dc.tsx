import { CSSProperties, ReactNode, useState } from 'react'
import type { RunStatus } from '../api/types'
import { fmtBytes, fmtThroughput } from '../api/view'
import './dc.css'

// Shared style constants + primitives translated from the Dashboard.dc design mock.
// Pages compose these to stay pixel-consistent with the comp.

// ---- Design tokens (engine + status → color) ------------------------------

export const ENGINE_COLORS: Record<string, string> = {
  postgres: 'var(--eng-postgres)',
  mysql: 'var(--eng-mysql)',
  mariadb: 'var(--eng-mysql)',
  mssql: 'var(--eng-mssql)',
  redis: 'var(--eng-redis)',
  mongodb: 'oklch(0.70 0.15 155)',
  sqlite: 'oklch(0.66 0.04 250)',
}

export function engineColor(engine: string): string {
  return ENGINE_COLORS[engine] ?? 'var(--ink-3)'
}

export interface StatusMeta {
  statusRu: string
  bg: string
  fg: string
  pulse: boolean
}

export const STATUS_META: Record<RunStatus, StatusMeta> = {
  success: { statusRu: 'успешно', bg: 'var(--ok-bg)', fg: 'var(--ok)', pulse: false },
  failed: { statusRu: 'ошибка', bg: 'var(--err-bg)', fg: 'var(--err)', pulse: false },
  running: { statusRu: 'выполняется', bg: 'var(--run-bg)', fg: 'var(--run)', pulse: true },
  skipped: { statusRu: 'пропущено', bg: 'var(--skip-bg)', fg: 'var(--skip)', pulse: false },
  queued: { statusRu: 'в очереди', bg: 'var(--run-bg)', fg: 'var(--run)', pulse: false },
}

export function statusMeta(status: string): StatusMeta {
  return STATUS_META[status as RunStatus] ?? { statusRu: status, bg: 'var(--panel-2)', fg: 'var(--ink-2)', pulse: false }
}

export const MONO = "'JetBrains Mono', ui-monospace, 'SF Mono', monospace"
export const SANS = "'Inter', system-ui, -apple-system, sans-serif"

export const inputStyle: CSSProperties = {
  width: '100%',
  padding: '9px 11px',
  background: 'var(--panel)',
  border: '1px solid var(--line-2)',
  borderRadius: 7,
  color: 'var(--ink)',
  fontFamily: MONO,
  fontSize: 12.5,
}

export const labelStyle: CSSProperties = {
  fontSize: 11,
  color: 'var(--ink-2)',
  fontWeight: 600,
  marginBottom: 6,
  display: 'block',
}

export const btnPrimary: CSSProperties = {
  display: 'inline-flex',
  alignItems: 'center',
  gap: 7,
  padding: '9px 18px',
  background: 'var(--accent)',
  color: 'var(--accent-ink)',
  border: 'none',
  borderRadius: 7,
  fontSize: 13,
  fontWeight: 600,
  cursor: 'pointer',
  fontFamily: SANS,
  whiteSpace: 'nowrap',
}

export const btnSecondary: CSSProperties = {
  display: 'inline-flex',
  alignItems: 'center',
  gap: 7,
  padding: '9px 16px',
  background: 'var(--panel)',
  border: '1px solid var(--line-2)',
  borderRadius: 7,
  color: 'var(--ink-2)',
  fontSize: 13,
  fontWeight: 600,
  cursor: 'pointer',
  fontFamily: SANS,
  whiteSpace: 'nowrap',
}

export const btnDanger: CSSProperties = {
  display: 'inline-flex',
  alignItems: 'center',
  gap: 7,
  padding: '9px 16px',
  background: 'transparent',
  border: '1px solid var(--err)',
  borderRadius: 7,
  color: 'var(--err)',
  fontSize: 13,
  fontWeight: 600,
  cursor: 'pointer',
  fontFamily: SANS,
  whiteSpace: 'nowrap',
}

// DeleteButton renders a destructive action with an inline two-step confirm, so
// removing an entity never rides on a single stray click. The parent's onDelete
// performs the API call: it should throw on failure (so the button re-arms and
// the parent can surface the error) and resolve on success (the parent unmounts
// this button by closing its modal/page, so no state is touched afterwards).
export function DeleteButton({
  onDelete,
  label = 'Удалить',
  confirmLabel = 'Удалить безвозвратно?',
}: {
  onDelete: () => Promise<void>
  label?: string
  confirmLabel?: string
}) {
  const [confirm, setConfirm] = useState(false)
  const [busy, setBusy] = useState(false)

  if (!confirm) {
    return (
      <button type="button" style={btnDanger} onClick={() => setConfirm(true)}>
        {label}
      </button>
    )
  }
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
      <span style={{ fontSize: 12, color: 'var(--err)', fontWeight: 600 }}>{confirmLabel}</span>
      <button
        type="button"
        style={{ ...btnDanger, background: 'var(--err)', color: 'var(--accent-ink)', borderColor: 'var(--err)', opacity: busy ? 0.6 : 1 }}
        disabled={busy}
        onClick={async () => {
          setBusy(true)
          try {
            await onDelete()
            // success → parent removes this component; leave state as-is.
          } catch {
            setBusy(false)
            setConfirm(false)
          }
        }}
      >
        {busy ? 'Удаление…' : 'Да, удалить'}
      </button>
      <button type="button" style={btnSecondary} disabled={busy} onClick={() => setConfirm(false)}>
        Отмена
      </button>
    </div>
  )
}

export const card: CSSProperties = {
  border: '1px solid var(--line)',
  borderRadius: 10,
  background: 'var(--panel)',
}

// Selected/unselected option-card styling (comp `pill(active)`).
export function pill(active: boolean): { border: string; bg: string; color: string } {
  return active
    ? { border: 'var(--accent)', bg: 'var(--run-bg)', color: 'var(--ink)' }
    : { border: 'var(--line-2)', bg: 'var(--panel)', color: 'var(--ink-2)' }
}

export function EngineDot({ engine, size = 6 }: { engine: string; size?: number }) {
  return (
    <div
      style={{ width: size, height: size, borderRadius: 2, background: engineColor(engine), flexShrink: 0 }}
    />
  )
}

// Status pill with the leading dot (comp run/task/sweep status badges).
export function StatusBadge({ status }: { status: string }) {
  const m = statusMeta(status)
  return (
    <span
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: 6,
        padding: '3px 9px',
        borderRadius: 5,
        fontSize: 11,
        fontWeight: 600,
        background: m.bg,
        color: m.fg,
      }}
    >
      <span
        className={m.pulse ? 'dc-pulse-fast' : undefined}
        style={{ width: 6, height: 6, borderRadius: '50%', background: m.fg }}
      />
      {m.statusRu}
    </span>
  )
}

// Toggle is a real switch, not a styled div: it has to be reachable by keyboard
// and announce its state, and `label` names what is being switched when several
// sit in one row (the notification matrix has one per event per channel).
export function Toggle({ on, onClick, label }: { on: boolean; onClick?: (e: React.MouseEvent) => void; label?: string }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={on}
      aria-label={label}
      onClick={onClick}
      className="dc-reset"
      style={{
        width: 34,
        height: 20,
        borderRadius: 20,
        padding: 2,
        cursor: 'pointer',
        background: on ? 'var(--accent)' : 'var(--line-2)',
        transition: 'background 0.15s',
        flexShrink: 0,
      }}
    >
      <div
        style={{
          width: 16,
          height: 16,
          borderRadius: '50%',
          background: 'oklch(1 0 0)',
          transform: `translateX(${on ? 14 : 0}px)`,
          transition: 'transform 0.15s',
        }}
      />
    </button>
  )
}

// Selectable option card used by the wizard and the create modals.
export function OptionCard({
  active,
  onClick,
  children,
  style,
}: {
  active: boolean
  onClick?: () => void
  children: ReactNode
  style?: CSSProperties
}) {
  const p = pill(active)
  return (
    <div
      onClick={onClick}
      style={{
        padding: 12,
        borderRadius: 8,
        cursor: 'pointer',
        border: `1px solid ${p.border}`,
        background: p.bg,
        ...style,
      }}
    >
      {children}
    </div>
  )
}

// LiveMeter shows streamed bytes and, while active, throughput. When an estimated
// `total` is known (the previous run's artifact size) it renders a determinate bar
// with an approximate percentage and ETA; otherwise an indeterminate sweep, since
// a streaming dump has no exact total.
export function LiveMeter({ bytes, bps, total = 0, active }: { bytes: number; bps: number; total?: number; active: boolean }) {
  const hasEst = total > 0
  // Cap below 100% while streaming: the estimate may undershoot the real size, and
  // the bar should not read "done" before the terminal status arrives.
  const pct = hasEst ? Math.min(active ? 99 : 100, Math.round((bytes / total) * 100)) : 0
  const remaining = hasEst ? total - bytes : 0
  const etaSec = active && hasEst && bps > 0 && remaining > 0 ? Math.round(remaining / bps) : null

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', fontFamily: MONO, fontSize: 12, color: 'var(--ink)', marginBottom: 6 }}>
        <span>
          {bytes > 0 ? fmtBytes(bytes) : '—'}
          {hasEst && <span style={{ color: 'var(--ink-3)' }}> {' / ≈'}{fmtBytes(total)}</span>}
        </span>
        {active && (
          <span style={{ color: 'var(--run)' }}>
            {fmtThroughput(bps)}
            {etaSec !== null && <span style={{ color: 'var(--ink-3)' }}> · ≈{fmtEta(etaSec)}</span>}
          </span>
        )}
      </div>
      {hasEst ? (
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <div style={{ flex: 1, height: 4, borderRadius: 3, background: 'var(--line-2)', overflow: 'hidden' }}>
            <div role="progressbar" aria-label="прогресс передачи" aria-valuenow={pct} style={{ width: `${pct}%`, height: '100%', borderRadius: 3, background: active ? 'var(--run)' : 'var(--ok)', transition: 'width 0.4s ease' }} />
          </div>
          <span style={{ fontFamily: MONO, fontSize: 11, color: active ? 'var(--run)' : 'var(--ok)', minWidth: 34, textAlign: 'right' }}>≈{pct}%</span>
        </div>
      ) : (
        active && (
          <div className="dc-bar" role="progressbar" aria-label="передача данных">
            <span />
          </div>
        )
      )}
    </div>
  )
}

// fmtEta renders a short "Nм NNс" / "NNс" estimate.
function fmtEta(sec: number): string {
  if (sec < 60) return `${sec}с`
  const m = Math.floor(sec / 60)
  const s = sec % 60
  return `${m}м ${String(s).padStart(2, '0')}с`
}

// Section header used inside cards/tables (e.g. "Политики по задачам").
export function SectionLabel({ children }: { children: ReactNode }) {
  return (
    <div style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)', marginBottom: 12 }}>{children}</div>
  )
}
