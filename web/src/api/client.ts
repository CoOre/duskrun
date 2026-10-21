import type {
  Connection,
  ConnectionTestResult,
  CreateConnectionReq,
  CreateNotifierReq,
  CreateSecretReq,
  CreateStorageReq,
  CreateTaskReq,
  CreateUserReq,
  DatabaseList,
  LoginMFAReq,
  LoginResp,
  LoginResult,
  Me,
  MFAStatus,
  Passkey,
  PublicKeyOptionsJSON,
  PushDevice,
  PushStatus,
  PushSubscriptionJSONBody,
  Notification,
  NotifierChannel,
  NotifierTestResult,
  RetentionSummary,
  Run,
  RunProgressEvent,
  Secret,
  Storage,
  Session,
  Settings,
  Sweep,
  Task,
  TOTPSetup,
  UpdateUserReq,
  User,
  WatchdogAlert,
} from './types'

// ApiError carries the HTTP status and the server's {error} message.
export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

export interface ClientOptions {
  getToken: () => string | null
  onUnauthorized?: () => void
  fetchImpl?: typeof fetch
  baseUrl?: string
}

// ApiClient wraps fetch: it prefixes /api, attaches the bearer token, parses
// JSON, and turns a 401 into a logout + ApiError. It is framework-agnostic so it
// can be unit-tested with a mock fetch.
export class ApiClient {
  private getToken: () => string | null
  private onUnauthorized?: () => void
  private fetchImpl: typeof fetch
  private baseUrl: string

  constructor(opts: ClientOptions) {
    this.getToken = opts.getToken
    this.onUnauthorized = opts.onUnauthorized
    this.fetchImpl = opts.fetchImpl ?? ((...a: Parameters<typeof fetch>) => fetch(...a))
    this.baseUrl = opts.baseUrl ?? '/api'
  }

  async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = {}
    const token = this.getToken()
    if (token) headers['Authorization'] = `Bearer ${token}`
    if (body !== undefined) headers['Content-Type'] = 'application/json'

    const resp = await this.fetchImpl(this.baseUrl + path, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    })

    // The /login routes are public: their 401 means "wrong password/code", not
    // "your session died", so it must not wipe the stored credential.
    const isPublic = path === '/login' || path.startsWith('/login/')
    if (resp.status === 401 && !isPublic) {
      this.onUnauthorized?.()
      throw new ApiError(401, 'необходима авторизация')
    }

    const text = await resp.text()
    let data: unknown = null
    if (text) {
      try {
        data = JSON.parse(text)
      } catch {
        data = text
      }
    }

    if (!resp.ok) {
      const msg =
        data && typeof data === 'object' && 'error' in data
          ? String((data as { error: unknown }).error)
          : resp.statusText || `HTTP ${resp.status}`
      throw new ApiError(resp.status, msg)
    }
    return data as T
  }

  get<T>(path: string): Promise<T> {
    return this.request<T>('GET', path)
  }

  post<T>(path: string, body: unknown): Promise<T> {
    return this.request<T>('POST', path, body)
  }

  // Typed endpoint helpers -------------------------------------------------
  ping(): Promise<{ status: string }> {
    return this.get('/ping')
  }

  listTasks(): Promise<Task[]> {
    return this.get('/tasks')
  }

  listRuns(params?: { task?: number; status?: string; limit?: number; before?: number; after?: number; anchor?: number }): Promise<Run[]> {
    const q = new URLSearchParams()
    if (params?.task) q.set('task', String(params.task))
    if (params?.status) q.set('status', params.status)
    if (params?.limit) q.set('limit', String(params.limit))
    if (params?.before) q.set('before', String(params.before))
    if (params?.after) q.set('after', String(params.after))
    if (params?.anchor) q.set('anchor', String(params.anchor))
    const qs = q.toString()
    return this.get(`/runs${qs ? `?${qs}` : ''}`)
  }

  getRun(id: number): Promise<Run> {
    return this.get(`/runs/${id}`)
  }

  // streamRun opens the SSE progress stream for a run and invokes onEvent for
  // each event until the run finishes (the server closes the stream) or `signal`
  // aborts. Uses fetch + ReadableStream rather than EventSource so the bearer
  // token rides in the Authorization header (EventSource can't set headers).
  // Resolves when the stream ends; a caller abort is swallowed (not an error).
  async streamRun(
    id: number,
    onEvent: (ev: RunProgressEvent) => void,
    signal?: AbortSignal,
  ): Promise<void> {
    const token = this.getToken()
    let resp: Response
    try {
      resp = await this.fetchImpl(`${this.baseUrl}/runs/${id}/events`, {
        headers: token ? { Authorization: `Bearer ${token}` } : {},
        signal,
      })
    } catch (e) {
      if (signal?.aborted) return
      throw e
    }
    if (resp.status === 401) {
      this.onUnauthorized?.()
      throw new ApiError(401, 'необходима авторизация')
    }
    if (!resp.ok || !resp.body) {
      throw new ApiError(resp.status, `не удалось открыть поток (HTTP ${resp.status})`)
    }

    const reader = resp.body.getReader()
    const decoder = new TextDecoder()
    let buf = ''
    try {
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buf += decoder.decode(value, { stream: true })
        // SSE frames are separated by a blank line.
        let sep: number
        while ((sep = buf.indexOf('\n\n')) !== -1) {
          const frame = buf.slice(0, sep)
          buf = buf.slice(sep + 2)
          for (const line of frame.split('\n')) {
            if (!line.startsWith('data:')) continue // skip comments/heartbeats
            const json = line.slice(5).trim()
            if (!json) continue
            try {
              onEvent(JSON.parse(json) as RunProgressEvent)
            } catch {
              // ignore a malformed frame; the stream continues
            }
          }
        }
      }
    } catch (e) {
      if (signal?.aborted) return
      throw e
    }
  }

  listConnections(): Promise<Connection[]> {
    return this.get('/connections')
  }

  listConnectionDatabases(id: number): Promise<DatabaseList> {
    return this.get(`/connections/${id}/databases`)
  }

  testConnection(body: CreateConnectionReq): Promise<ConnectionTestResult> {
    return this.post('/connections/test', body)
  }

  listStorages(): Promise<Storage[]> {
    return this.get('/storages')
  }

  listSecrets(): Promise<Secret[]> {
    return this.get('/secrets')
  }

  createSecret(body: CreateSecretReq): Promise<{ id: number }> {
    return this.post('/secrets', body)
  }

  deleteSecret(id: number): Promise<void> {
    return this.request('DELETE', `/secrets/${id}`)
  }

  listNotifiers(): Promise<NotifierChannel[]> {
    return this.get('/notifiers')
  }

  createNotifier(body: CreateNotifierReq): Promise<{ id: number }> {
    return this.post('/notifiers', body)
  }

  updateNotifier(id: number, body: CreateNotifierReq): Promise<{ id: number }> {
    return this.request('PATCH', `/notifiers/${id}`, body)
  }

  deleteNotifier(id: number): Promise<void> {
    return this.request('DELETE', `/notifiers/${id}`)
  }

  testNotifier(id: number): Promise<NotifierTestResult> {
    return this.post(`/notifiers/${id}/test`, {})
  }

  // Browser push ---------------------------------------------------------

  pushStatus(): Promise<PushStatus> {
    return this.get('/push')
  }

  listPushDevices(): Promise<PushDevice[]> {
    return this.get('/push/subscriptions')
  }

  subscribePush(sub: PushSubscriptionJSONBody): Promise<{ id: number }> {
    return this.post('/push/subscriptions', sub)
  }

  /** Drops this browser's subscription; the endpoint travels in the body, not the URL. */
  unsubscribePush(endpoint: string): Promise<void> {
    return this.post('/push/unsubscribe', { endpoint })
  }

  deletePushDevice(id: number): Promise<void> {
    return this.request('DELETE', `/push/subscriptions/${id}`)
  }

  testPushDevice(id: number): Promise<void> {
    return this.post(`/push/subscriptions/${id}/test`, {})
  }

  listNotifications(limit?: number): Promise<Notification[]> {
    return this.get(`/notifications${limit ? `?limit=${limit}` : ''}`)
  }

  listWatchdogAlerts(): Promise<WatchdogAlert[]> {
    return this.get('/watchdog')
  }

  getRetention(): Promise<RetentionSummary> {
    return this.get('/retention')
  }

  listSweeps(limit?: number): Promise<Sweep[]> {
    return this.get(`/retention/sweeps${limit ? `?limit=${limit}` : ''}`)
  }

  runSweep(): Promise<Sweep> {
    return this.post('/retention/sweep', {})
  }

  createConnection(body: CreateConnectionReq): Promise<{ id: number }> {
    return this.post('/connections', body)
  }

  updateConnection(id: number, body: CreateConnectionReq): Promise<{ id: number }> {
    return this.request('PATCH', `/connections/${id}`, body)
  }

  deleteConnection(id: number): Promise<void> {
    return this.request('DELETE', `/connections/${id}`)
  }

  createStorage(body: CreateStorageReq): Promise<{ id: number }> {
    return this.post('/storages', body)
  }

  updateStorage(id: number, body: CreateStorageReq): Promise<{ id: number }> {
    return this.request('PATCH', `/storages/${id}`, body)
  }

  deleteStorage(id: number): Promise<void> {
    return this.request('DELETE', `/storages/${id}`)
  }

  createTask(body: CreateTaskReq): Promise<{ id: number }> {
    return this.post('/tasks', body)
  }

  updateTask(id: number, body: CreateTaskReq): Promise<{ id: number }> {
    return this.request('PATCH', `/tasks/${id}`, body)
  }

  deleteTask(id: number): Promise<void> {
    return this.request('DELETE', `/tasks/${id}`)
  }

  runTask(id: number): Promise<{ run_id: number }> {
    return this.post(`/tasks/${id}/run`, {})
  }

  restoreHint(id: number): Promise<{ restore_hint: string }> {
    return this.get(`/tasks/${id}/restore-hint`)
  }

  // Auth, users and settings ----------------------------------------------

  // login is the only unauthenticated call. The returned token is shown once by
  // the server, so the caller must store it — it cannot be fetched again.
  //
  // An account with 2FA gets a challenge instead of a token; the session is
  // then issued by loginMFA once the second factor checks out.
  login(email: string, password: string): Promise<LoginResult> {
    return this.post('/login', { email, password })
  }

  loginMFA(body: LoginMFAReq): Promise<LoginResp> {
    return this.post('/login/mfa', body)
  }

  loginWebAuthnOptions(mfaToken: string): Promise<PublicKeyOptionsJSON> {
    return this.post('/login/mfa/webauthn/options', { mfa_token: mfaToken })
  }

  logout(): Promise<{ status: string }> {
    return this.post('/logout', {})
  }

  me(): Promise<Me> {
    return this.get('/me')
  }

  changeOwnPassword(oldPassword: string, newPassword: string): Promise<{ status: string }> {
    return this.post('/me/password', { old_password: oldPassword, new_password: newPassword })
  }

  // Second factors ---------------------------------------------------------
  // Every change takes the current password: a stolen session alone must not be
  // enough to bind an attacker's authenticator or strip the owner's.

  getMFA(): Promise<MFAStatus> {
    return this.get('/me/mfa')
  }

  setupTOTP(password: string): Promise<TOTPSetup> {
    return this.post('/me/mfa/totp/setup', { password })
  }

  confirmTOTP(code: string): Promise<{ recovery_codes?: string[] }> {
    return this.post('/me/mfa/totp/confirm', { code })
  }

  disableTOTP(password: string): Promise<{ status: string }> {
    return this.post('/me/mfa/totp/disable', { password })
  }

  passkeyOptions(password: string, name: string): Promise<PublicKeyOptionsJSON> {
    return this.post('/me/mfa/passkeys/options', { password, name })
  }

  registerPasskey(credential: unknown): Promise<{ passkey: Passkey; recovery_codes?: string[] }> {
    return this.post('/me/mfa/passkeys', { credential })
  }

  deletePasskey(id: number, password: string): Promise<void> {
    return this.request('DELETE', `/me/mfa/passkeys/${id}`, { password })
  }

  regenerateRecoveryCodes(password: string): Promise<{ recovery_codes: string[] }> {
    return this.post('/me/mfa/recovery-codes', { password })
  }

  resetUserMFA(id: number): Promise<User> {
    return this.post(`/users/${id}/mfa/reset`, {})
  }

  listSessions(): Promise<Session[]> {
    return this.get('/sessions')
  }

  deleteSession(id: number): Promise<void> {
    return this.request('DELETE', `/sessions/${id}`)
  }

  listUsers(): Promise<User[]> {
    return this.get('/users')
  }

  createUser(body: CreateUserReq): Promise<User> {
    return this.post('/users', body)
  }

  updateUser(id: number, body: UpdateUserReq): Promise<User> {
    return this.request('PATCH', `/users/${id}`, body)
  }

  deleteUser(id: number): Promise<void> {
    return this.request('DELETE', `/users/${id}`)
  }

  getSettings(): Promise<Settings> {
    return this.get('/settings')
  }

  updateSettings(body: Partial<Pick<Settings, 'instance_name' | 'ssh_host_key_mode_default' | 'mfa_required'>>): Promise<Settings> {
    return this.request('PATCH', '/settings', body)
  }

  // Absolute path to stream an artifact download.
  artifactDownloadUrl(id: number): string {
    return `${this.baseUrl}/artifacts/${id}/download`
  }

  runArtifactDownloadUrl(id: number): string {
    return `${this.baseUrl}/runs/${id}/artifact/download`
  }

  // downloadArtifact fetches an artifact with the bearer token attached (a plain
  // <a href> can't send Authorization), returning the bytes as a Blob.
  async downloadArtifact(id: number): Promise<Blob> {
    return this.download(this.artifactDownloadUrl(id))
  }

  async downloadRunArtifact(runId: number): Promise<Blob> {
    return this.download(this.runArtifactDownloadUrl(runId))
  }

  private async download(url: string): Promise<Blob> {
    const token = this.getToken()
    const resp = await this.fetchImpl(url, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    })
    if (resp.status === 401) {
      this.onUnauthorized?.()
      throw new ApiError(401, 'необходима авторизация')
    }
    if (!resp.ok) {
      throw new ApiError(resp.status, `не удалось скачать артефакт (HTTP ${resp.status})`)
    }
    return resp.blob()
  }
}
