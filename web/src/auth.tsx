import {
  createContext,
  ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from 'react'
import { ApiClient } from './api/client'
import type { Me, Role } from './api/types'
import { disablePush } from './push'

const TOKEN_KEY = 'duskrun.token'

export interface AuthState {
  token: string | null
  isAuthed: boolean
  /** Identity behind the current token; null until /me answers. */
  me: Me | null
  /** Effective role. Unknown identity is treated as the least privileged. */
  role: Role
  /** True while /me is in flight, so the UI can avoid flashing a wrong role. */
  loadingMe: boolean
  login: (token: string) => void
  logout: () => void
  /** Re-reads /me after a change that alters the caller's own identity. */
  refreshMe: () => void
  api: ApiClient
}

const AuthContext = createContext<AuthState | null>(null)

export function AuthProvider({
  children,
  fetchImpl,
}: {
  children: ReactNode
  fetchImpl?: typeof fetch
}) {
  const [token, setToken] = useState<string | null>(() => localStorage.getItem(TOKEN_KEY))
  const [me, setMe] = useState<Me | null>(null)
  // Starts true when a token is already stored, so the very first render can
  // wait for /me instead of mounting pages that the MFA policy may forbid.
  const [loadingMe, setLoadingMe] = useState(() => !!localStorage.getItem(TOKEN_KEY))
  const [meEpoch, setMeEpoch] = useState(0)

  const login = useCallback((t: string) => {
    localStorage.setItem(TOKEN_KEY, t)
    setToken(t)
  }, [])

  // clearToken drops the local credential without calling the server. It is the
  // 401 path: the session is already gone, and calling /logout would only
  // produce another 401.
  const clearToken = useCallback(() => {
    localStorage.removeItem(TOKEN_KEY)
    setToken(null)
    setMe(null)
  }, [])

  // getToken reads localStorage directly so every request uses the live token
  // even across a logout triggered mid-flight.
  const api = useMemo(
    () =>
      new ApiClient({
        getToken: () => localStorage.getItem(TOKEN_KEY),
        onUnauthorized: clearToken,
        fetchImpl,
      }),
    [clearToken, fetchImpl],
  )

  // logout ends the server-side session too. The static API token has no
  // session, and a failing call must not trap the user in the app, so the local
  // credential is cleared either way.
  //
  // Signing out also stops push to this browser: whoever signs out is saying
  // the browser is not theirs to use, and backup alerts must not keep arriving
  // on it. That call needs the session, so it goes first, on a client bound to
  // the token being dropped — the shared one reads localStorage, which is
  // cleared right away so the UI does not wait on the network.
  const logout = useCallback(() => {
    const had = localStorage.getItem(TOKEN_KEY)
    if (had) {
      const bound = new ApiClient({ getToken: () => had, fetchImpl })
      void disablePush(bound)
        .catch(() => undefined)
        .finally(() => bound.logout().catch(() => undefined))
    }
    clearToken()
  }, [clearToken, fetchImpl])

  const refreshMe = useCallback(() => setMeEpoch((n) => n + 1), [])

  useEffect(() => {
    if (!token) {
      setMe(null)
      return
    }
    let alive = true
    setLoadingMe(true)
    api
      .me()
      .then((v) => {
        if (alive) setMe(v)
      })
      // A 401 already cleared the token via onUnauthorized; anything else
      // leaves me null, which the UI renders as the least-privileged state
      // rather than guessing.
      .catch(() => undefined)
      .finally(() => {
        if (alive) setLoadingMe(false)
      })
    return () => {
      alive = false
    }
  }, [api, token, meEpoch])

  const value = useMemo<AuthState>(
    () => ({
      token,
      isAuthed: !!token,
      me,
      role: me?.role ?? 'viewer',
      loadingMe,
      login,
      logout,
      refreshMe,
      api,
    }),
    [token, me, loadingMe, login, logout, refreshMe, api],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthState {
  const v = useContext(AuthContext)
  if (!v) throw new Error('useAuth must be used within an AuthProvider')
  return v
}

/**
 * useAuthOptional returns null outside a provider instead of throwing.
 *
 * Layout lives in ui/ and is rendered standalone by its own tests, so it must
 * degrade rather than crash when there is no session to describe.
 */
export function useAuthOptional(): AuthState | null {
  return useContext(AuthContext)
}

/** Role ordering, mirroring core.Role.AtLeast on the server. */
const RANK: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 }

/**
 * can reports whether the current role covers `want`.
 *
 * This hides actions the caller cannot perform. It is convenience only — every
 * one of these routes is enforced server-side, and the UI check is not what
 * makes them safe.
 */
export function useCan(want: Role): boolean {
  const { role, me } = useAuth()
  if (!me) return false
  return (RANK[role] ?? 0) >= RANK[want]
}
