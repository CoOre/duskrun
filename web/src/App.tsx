import { BrowserRouter, Navigate, Outlet, Route, Routes } from 'react-router-dom'
import { AuthProvider, useAuth } from './auth'
import Connections from './pages/Connections'
import Dashboard from './pages/Dashboard'
import History from './pages/History'
import Login from './pages/Login'
import Notifications from './pages/Notifications'
import Retention from './pages/Retention'
import Secrets from './pages/Secrets'
import Settings from './pages/Settings'
import Storages from './pages/Storages'
import TaskEditor from './pages/TaskEditor'
import Tasks from './pages/Tasks'
import TaskWizard from './pages/TaskWizard'

// AppRoutes holds the route table and the auth guard. It is exported separately
// so tests can drive it inside a MemoryRouter.
export function AppRoutes() {
  const { isAuthed } = useAuth()
  return (
    <Routes>
      <Route path="/login" element={isAuthed ? <Navigate to="/" replace /> : <Login />} />

      {/* Protected group: unauthenticated users bounce to /login. */}
      <Route element={isAuthed ? <Outlet /> : <Navigate to="/login" replace />}>
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
