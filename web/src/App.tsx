import { useEffect } from 'react'
import { BrowserRouter, Navigate, Outlet, Route, Routes, useNavigate } from 'react-router-dom'
import { AuthProvider, useAuth } from './auth'
import Connections from './pages/Connections'
import Dashboard from './pages/Dashboard'
import History from './pages/History'
import Login from './pages/Login'
import MFAEnroll from './pages/MFAEnroll'
import Notifications from './pages/Notifications'
import Retention from './pages/Retention'
import Secrets from './pages/Secrets'
import Settings from './pages/Settings'
import Storages from './pages/Storages'
import TaskEditor from './pages/TaskEditor'
import Tasks from './pages/Tasks'
import TaskWizard from './pages/TaskWizard'
import { syncPush } from './push'

// PushBridge connects the page to the push service worker. It re-reports this
// browser's subscription once per signed-in user (browsers rotate them, and
// the worker cannot reach the API), and follows the worker's request to open
// a page when a notification is clicked in a tab it does not control yet.
function PushBridge() {
  const { api, me } = useAuth()
  const navigate = useNavigate()
  const userID = me && !me.static && !me.mfa_enroll_required ? (me.user_id ?? null) : null

  useEffect(() => {
    if (userID === null) return
    void syncPush(api).catch(() => undefined)
  }, [api, userID])

  useEffect(() => {
    const sw = 'serviceWorker' in navigator ? navigator.serviceWorker : null
    if (!sw) return
    const onMessage = (e: MessageEvent) => {
      const d = e.data as { type?: string; url?: string } | null
      if (d?.type === 'duskrun:navigate' && typeof d.url === 'string' && d.url.startsWith('/')) navigate(d.url)
    }
    sw.addEventListener('message', onMessage)
    return () => sw.removeEventListener('message', onMessage)
  }, [navigate])

  return null
}

// AppRoutes holds the route table and the auth guard. It is exported separately
// so tests can drive it inside a MemoryRouter.
export function AppRoutes() {
  const { isAuthed, me, loadingMe } = useAuth()
  // While the policy demands a second factor this account lacks, every
  // protected path shows the enrolment screen instead — the server would 403
  // the page's own requests anyway. Until /me has answered nothing is mounted,
  // or those pages would fire their requests first and flash the 403s.
  const gate = !isAuthed ? (
    <Navigate to="/login" replace />
  ) : !me && loadingMe ? null : me?.mfa_enroll_required ? (
    <MFAEnroll />
  ) : (
    <Outlet />
  )
  return (
    <>
      <PushBridge />
      <Routes>
        <Route path="/login" element={isAuthed ? <Navigate to="/" replace /> : <Login />} />

        {/* Protected group: unauthenticated users bounce to /login. */}
        <Route element={gate}>
          <Route path="/" element={<Dashboard />} />
          <Route path="/tasks" element={<Tasks />} />
          <Route path="/tasks/new" element={<TaskWizard />} />
          <Route path="/tasks/:id/edit" element={<TaskEditor />} />
          <Route path="/history" element={<History />} />
          <Route path="/connections" element={<Connections />} />
          <Route path="/storages" element={<Storages />} />
          <Route path="/retention" element={<Retention />} />
          <Route path="/notifications" element={<Notifications />} />
          <Route path="/secrets" element={<Secrets />} />
          <Route path="/settings" element={<Settings />} />
          {/* Unknown path → dashboard. */}
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Routes>
    </>
  )
}

export default function App() {
  return (
    <AuthProvider>
      <BrowserRouter>
        <AppRoutes />
      </BrowserRouter>
    </AuthProvider>
  )
}
