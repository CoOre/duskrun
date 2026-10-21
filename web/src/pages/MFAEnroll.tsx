import { useAuth } from '../auth'
import { BrandMark } from '../ui/Layout'
import { MFASettings } from '../ui/MFASettings'
import { MONO, btnSecondary } from '../ui/dc'

// MFAEnroll replaces every page while the instance policy demands a second
// factor this account does not have yet. The server answers 403 to everything
// but /me, /me/mfa/* and /logout in that state, so rendering the normal pages
// would only show a wall of errors.
//
// Once a factor is in place (and any recovery codes acknowledged) MFASettings
// calls back, /me is re-read, mfa_enroll_required drops, and the app returns.
export default function MFAEnroll() {
  const { me, logout, refreshMe } = useAuth()
  return (
    <div style={{ minHeight: '100vh', background: 'var(--app)', display: 'flex', justifyContent: 'center', padding: '48px 24px' }}>
      <div style={{ width: 620, maxWidth: '100%' }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 28 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
            <BrandMark size={30} />
            <div style={{ fontFamily: MONO, fontSize: 20, fontWeight: 700, letterSpacing: '-0.03em', color: 'var(--ink)' }}>Duskrun</div>
          </div>
          <button type="button" style={btnSecondary} onClick={logout}>Выйти</button>
        </div>

        <div style={{ fontSize: 18, fontWeight: 700, color: 'var(--ink)', marginBottom: 6 }}>Администратор требует 2FA</div>
        <div style={{ fontSize: 13, color: 'var(--ink-2)', lineHeight: 1.6, marginBottom: 22 }}>
          Чтобы продолжить работу{me?.email ? <> под <b>{me.email}</b></> : null}, подключите второй
          фактор: приложение-аутентификатор или passkey. После этого вы получите коды
          восстановления — сохраните их, они понадобятся при потере устройства.
        </div>

        <MFASettings onChange={refreshMe} />
      </div>
    </div>
  )
}
