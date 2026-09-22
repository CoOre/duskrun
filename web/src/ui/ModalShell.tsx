import { ReactNode } from 'react'

// Modal chrome ported from the comp (overlay + centered panel + header). Body
// content (padding, fields, footer) is supplied by the caller.
export function ModalShell({
  title,
  onClose,
  width = 480,
  zIndex = 30,
  children,
}: {
  title: string
  onClose: () => void
  width?: number
  zIndex?: number
  children: ReactNode
}) {
  return (
    <>
      <div onClick={onClose} style={{ position: 'fixed', inset: 0, background: 'var(--overlay)', zIndex }} />
      <div
        role="dialog"
        aria-label={title}
        className="dc-modal-in"
        onClick={(e) => e.stopPropagation()}
        style={{ position: 'fixed', top: '50%', left: '50%', transform: 'translate(-50%, -50%)', width, maxWidth: 'calc(100% - 40px)', maxHeight: 'calc(100vh - 60px)', overflowY: 'auto', overflowX: 'hidden', background: 'var(--app)', border: '1px solid var(--line-2)', borderRadius: 13, zIndex: zIndex + 1, boxShadow: '0 24px 60px oklch(0 0 0 / 0.45)' }}
      >
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '18px 22px', borderBottom: '1px solid var(--line)', position: 'sticky', top: 0, background: 'var(--app)' }}>
          <div style={{ fontSize: 15, fontWeight: 700, color: 'var(--ink)' }}>{title}</div>
          <button type="button" onClick={onClose} aria-label="Закрыть" className="dc-reset dc-h-panel2" style={{ width: 30, height: 30, borderRadius: 6, display: 'flex', alignItems: 'center', justifyContent: 'center', cursor: 'pointer', color: 'var(--ink-2)', fontSize: 15 }}>✕</button>
        </div>
        {children}
      </div>
    </>
  )
}
