import { CSSProperties, ReactNode, useEffect, useState } from 'react'
import { NavLink, Outlet } from 'react-router-dom'
import { useAuthOptional } from '../auth'
import type { Me } from '../api/types'
import { MONO } from './dc'
import './dc.css'

type NavItem = { label: string; to: string; end?: boolean; count?: number }
type NavGroup = { title: string; items: NavItem[] }

// Navigation mirrors the comp's navDef.
const NAV: NavGroup[] = [
  {
    title: 'ОБЗОР',
    items: [
      { label: 'Дашборд', to: '/', end: true },
      { label: 'Задачи', to: '/tasks' },
      { label: 'История запусков', to: '/history' },
    ],
  },
  {
    title: 'КОНФИГУРАЦИЯ',
    items: [
      { label: 'Соединения', to: '/connections' },
      { label: 'Хранилища', to: '/storages' },
      { label: 'Политики хранения', to: '/retention' },
      { label: 'Уведомления', to: '/notifications' },
    ],
  },
  {
    title: 'СИСТЕМА',
    items: [
      { label: 'Секреты', to: '/secrets' },
      { label: 'Настройки', to: '/settings' },
    ],
  },
]

export type Theme = 'dark' | 'light'
const THEME_KEY = 'duskrun.theme'

export function readTheme(): Theme {
  return localStorage.getItem(THEME_KEY) === 'light' ? 'light' : 'dark'
}

export function useTheme(): [Theme, (t: Theme) => void] {
  const [theme, setTheme] = useState<Theme>(readTheme)
  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme)
    localStorage.setItem(THEME_KEY, theme)
  }, [theme])
  return [theme, setTheme]
}

type LayoutProps = {
  title?: string
  subtitle?: string
  actions?: ReactNode
  onLogout?: () => void
  children?: ReactNode
}

/** Identity as the sidebar renders it. */
type Identity = { initials: string; title: string; subtitle: string }

/**
 * identityOf turns /me into the three strings the sidebar shows.
 *
 * The static API token is labelled as a token, not dressed up as a person: it
 * is a shared machine credential, and showing a plausible-looking account name
 * for it would misrepresent who is logged in.
 */
export function identityOf(me: Me | null | undefined): Identity {
  if (!me) return { initials: '—', title: 'Не определён', subtitle: '…' }
  if (me.static) return { initials: 'API', title: 'API-токен', subtitle: me.role }
  const label = me.name?.trim() || me.email || ''
  return { initials: initialsOf(label), title: me.email ?? label, subtitle: me.role }
}

/** initialsOf takes up to two leading letters — of the words in a name, or of
 *  the local part of an email when there is no name. */
function initialsOf(label: string): string {
  const local = label.includes('@') ? label.slice(0, label.indexOf('@')) : label
  const words = local.split(/[\s._-]+/).filter(Boolean)
  const letters = words.slice(0, 2).map((w) => w[0])
  return (letters.join('') || '?').toUpperCase()
}

export function Layout({ title, subtitle, actions, onLogout, children }: LayoutProps) {
  const [theme, setTheme] = useTheme()
  const auth = useAuthOptional()
  const identity = identityOf(auth?.me)
  const sub = subtitle ?? ''

  return (
    <div
      style={{
        fontFamily: "'Inter', sans-serif",
        display: 'flex',
        height: '100vh',
        width: '100%',
        background: 'var(--app)',
        color: 'var(--ink)',
        overflow: 'hidden',
      }}
    >
      {/* Sidebar */}
      <div
        style={{
          width: 240,
          flexShrink: 0,
          background: 'var(--sidebar)',
          borderRight: '1px solid var(--line)',
          display: 'flex',
          flexDirection: 'column',
        }}
      >
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 11,
            height: 56,
            padding: '0 18px',
            borderBottom: '1px solid var(--line)',
            flexShrink: 0,
          }}
        >
          <BrandMark />
          <div style={{ display: 'flex', alignItems: 'baseline', gap: 8 }}>
            <div style={{ fontFamily: MONO, fontSize: 15, fontWeight: 700, letterSpacing: '-0.03em', color: 'var(--ink)', lineHeight: 1 }}>
              Duskrun
            </div>
            <div style={{ fontSize: 10, color: 'var(--ink-3)', fontFamily: MONO }}>v1.0</div>
          </div>
        </div>

        <div style={{ flex: 1, overflowY: 'auto', padding: '16px 10px' }}>
          {NAV.map((g) => (
            <div key={g.title} style={{ marginBottom: 18 }}>
              <div style={{ fontSize: 10, fontWeight: 600, letterSpacing: '0.1em', color: 'var(--ink-3)', padding: '0 12px 9px 12px', fontFamily: MONO }}>
                {g.title}
              </div>
              {g.items.map((it) => (
                <NavLink key={it.to} to={it.to} end={it.end} style={{ textDecoration: 'none' }}>
                  {({ isActive }) => <NavRow item={it} active={isActive} />}
                </NavLink>
              ))}
            </div>
          ))}
        </div>

        <div style={{ padding: 12, borderTop: '1px solid var(--line)' }}>
          <div
            className="dc-h-panel"
            style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '6px 8px', borderRadius: 8, marginBottom: 4 }}
          >
            <div style={{ display: 'flex', alignItems: 'center', gap: 9, minWidth: 0 }}>
              <div style={{ width: 26, height: 26, borderRadius: '50%', background: 'var(--panel-2)', border: '1px solid var(--line-2)', display: 'flex', alignItems: 'center', justifyContent: 'center', fontFamily: MONO, fontSize: 10, fontWeight: 700, color: 'var(--accent)', flexShrink: 0 }}>
                {identity.initials}
              </div>
              <div style={{ minWidth: 0 }}>
                <div title={identity.title} style={{ fontSize: 12, color: 'var(--ink)', fontWeight: 500, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                  {identity.title}
                </div>
                <div style={{ fontSize: 10, color: 'var(--ink-3)', fontFamily: MONO }}>{identity.subtitle}</div>
              </div>
            </div>
            {onLogout && (
              <button
                type="button"
                onClick={onLogout}
                title="Выйти"
                aria-label="Выйти"
                className="dc-reset dc-h-panel2"
                style={{ width: 26, height: 26, borderRadius: 6, display: 'flex', alignItems: 'center', justifyContent: 'center', cursor: 'pointer', color: 'var(--ink-3)', flexShrink: 0 }}
              >
                <LogoutGlyph />
              </button>
            )}
          </div>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '2px 8px' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <div className="dc-pulse" style={{ width: 7, height: 7, borderRadius: '50%', background: 'var(--ok)' }} />
              <span style={{ fontSize: 12, color: 'var(--ink-2)' }}>воркеры 3/4</span>
            </div>
            <span style={{ fontFamily: MONO, fontSize: 10.5, color: 'var(--ink-3)' }}>node-01</span>
          </div>
        </div>
      </div>

      {/* Main column */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', minWidth: 0, overflow: 'hidden' }}>
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            padding: '0 26px',
            height: 56,
            borderBottom: '1px solid var(--line)',
            flexShrink: 0,
            background: 'var(--topbar)',
          }}
        >
          <div style={{ display: 'flex', alignItems: 'center', gap: 14, minWidth: 0 }}>
            <span style={{ fontSize: 16, fontWeight: 700, color: 'var(--ink)', letterSpacing: '-0.02em', whiteSpace: 'nowrap' }}>{title}</span>
            <span style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-3)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{sub}</span>
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexShrink: 0 }}>
            <div style={{ position: 'relative', display: 'flex', alignItems: 'center' }}>
              <div style={{ position: 'absolute', left: 11, width: 11, height: 11, border: '1.5px solid var(--ink-3)', borderRadius: '50%' }} />
              <input
                type="text"
                placeholder="поиск…"
                className="dc-input"
                style={{ width: 190, padding: '7px 12px 7px 30px', background: 'var(--panel)', border: '1px solid var(--line)', borderRadius: 7, color: 'var(--ink)', fontFamily: MONO, fontSize: 12 }}
              />
            </div>
            <ThemeToggle theme={theme} setTheme={setTheme} />
            {actions}
          </div>
        </div>
        <div style={{ flex: 1, overflowY: 'auto' }}>{children ?? <Outlet />}</div>
      </div>
    </div>
  )
}

function NavRow({ item, active }: { item: NavItem; active: boolean }) {
  return (
    <div
      className="dc-h-panel"
      style={{
        position: 'relative',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        padding: '7px 12px',
        cursor: 'pointer',
        fontSize: 13,
        fontWeight: 500,
        marginBottom: 1,
        borderRadius: 6,
        background: active ? 'var(--panel)' : 'transparent',
        color: active ? 'var(--ink)' : 'var(--ink-2)',
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        {active && (
          <div style={{ position: 'absolute', left: 0, top: 7, bottom: 7, width: 2, borderRadius: 2, background: 'var(--accent)' }} />
        )}
        <div style={{ width: 5, height: 5, borderRadius: 1, background: active ? 'var(--accent)' : 'var(--ink-3)' }} />
        <span>{item.label}</span>
      </div>
      {item.count != null && (
        <span style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-3)' }}>{item.count}</span>
      )}
    </div>
  )
}

function ThemeToggle({ theme, setTheme }: { theme: Theme; setTheme: (t: Theme) => void }) {
  const opts: { key: Theme; label: string; glyph: string }[] = [
    { key: 'light', label: 'Светлая тема', glyph: '☀' },
    { key: 'dark', label: 'Тёмная тема', glyph: '☾' },
  ]
  return (
    <div style={{ display: 'flex', padding: 2, background: 'var(--panel)', border: '1px solid var(--line)', borderRadius: 7, gap: 2 }}>
      {opts.map((t) => {
        const active = theme === t.key
        return (
          <button
            key={t.key}
            type="button"
            title={t.label}
            aria-label={t.label}
            aria-pressed={active}
            onClick={() => setTheme(t.key)}
            className="dc-reset"
            style={{ width: 28, height: 24, display: 'flex', alignItems: 'center', justifyContent: 'center', borderRadius: 5, cursor: 'pointer', background: active ? 'var(--accent)' : 'transparent', color: active ? 'var(--accent-ink)' : 'var(--ink-2)' }}
          >
            <span style={{ fontSize: 12 }}>{t.glyph}</span>
          </button>
        )
      })}
    </div>
  )
}

function BrandMark({ size = 26 }: { size?: number }) {
  return (
    <div style={{ width: size, height: size, border: '1.5px solid var(--accent)', borderRadius: 3, flexShrink: 0, position: 'relative' }}>
      <div style={{ position: 'absolute', inset: 4, border: '1.5px solid var(--accent)', borderRadius: 1, opacity: 0.5 }} />
    </div>
  )
}

function LogoutGlyph() {
  const s: CSSProperties = { position: 'absolute' }
  return (
    <div style={{ width: 11, height: 13, border: '1.5px solid currentColor', borderRight: 'none', borderRadius: '2px 0 0 2px', position: 'relative' }}>
      <div style={{ ...s, top: '50%', left: 9, width: 6, height: 1.5, background: 'currentColor', transform: 'translateY(-50%)' }} />
      <div style={{ ...s, top: '50%', left: 12, width: 5, height: 5, borderTop: '1.5px solid currentColor', borderRight: '1.5px solid currentColor', transform: 'translateY(-50%) rotate(45deg)' }} />
    </div>
  )
}

export { BrandMark }
