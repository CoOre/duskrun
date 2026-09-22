import { useEffect, useState } from 'react'

export interface AsyncState<T> {
  data: T | null
  error: string | null
  loading: boolean
}

// useApiData loads on mount and again whenever `deps` change (e.g. a reload
// counter bumped after a create, or a ticker driving periodic refresh).
// Minimal by design — no cache.
//
// A refresh (any load with data already on screen) never blanks the view: the
// previous data stays visible, `loading` stays false, and a failed refresh is
// swallowed rather than replacing a good view with an error. Only the very
// first load can report `loading` or `error`.
export function useApiData<T>(load: () => Promise<T>, deps: unknown[] = []): AsyncState<T> {
  const [state, setState] = useState<AsyncState<T>>({ data: null, error: null, loading: true })
  useEffect(() => {
    let alive = true
    setState((prev) => (prev.data === null ? { data: null, error: null, loading: true } : prev))
    load()
      .then((data) => alive && setState({ data, error: null, loading: false }))
      .catch((e) => {
        if (!alive) return
        setState((prev) =>
          prev.data === null
            ? { data: null, error: e?.message ?? String(e), loading: false }
            : prev, // keep the last good render; the next refresh may recover
        )
      })
    return () => {
      alive = false
    }
    // `load` is a fresh closure each render; we re-run only on explicit deps.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)
  return state
}
