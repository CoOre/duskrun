// Retention policy helpers shared by the task wizard, the task editor and the
// retention page.
//
// The backend stores two independent rules — `keep_last N` and GFS
// (daily/weekly/monthly) — and UNIONS them: an artifact any rule keeps is kept
// (core.Forget). The FORMS present them as a choice instead: one rule, or
// neither. That is a UI decision, not the engine's — a policy carrying both
// still arrives from the API and still prunes as a union, so `formatPolicy` and
// the retention page keep rendering both.

import type { GFS, Retention } from '../api/types'

// RetentionMode is what the forms let an operator pick: one rule, or no pruning
// at all. `none` is a real choice, not an absence — it means the task's
// artifacts accumulate forever, which is worth stating out loud.
export type RetentionMode = 'none' | 'keep_last' | 'gfs'

export const GFS_OFF: GFS = { daily: 0, weekly: 0, monthly: 0 }

// GFS_DEFAULT is what the switched-off GFS row shows: a week of dailies, a
// month of weeklies, a year of monthlies. The inputs are pre-filled with it so
// that turning the rule on is one click rather than three fields — an empty
// (all-zero) block would switch on and keep nothing.
export const GFS_DEFAULT: GFS = { daily: 7, weekly: 4, monthly: 12 }

// KEEP_LAST_DEFAULT is the same idea for the other rule.
export const KEEP_LAST_DEFAULT = 14

// gfsActive reports whether a GFS block keeps anything at all. All-zero is off,
// which is why the forms read this rather than the block's presence.
export function gfsActive(gfs: GFS | undefined): boolean {
  return Boolean(gfs && (gfs.daily > 0 || gfs.weekly > 0 || gfs.monthly > 0))
}

// gfsFrom fills in a policy's GFS block for editing. A task with GFS off has no
// block at all, and its inputs show the defaults rather than zeros — otherwise
// switching the rule on would arm a policy that keeps nothing.
export function gfsFrom(ret: Retention | undefined): GFS {
  const gfs = ret?.gfs
  if (!gfsActive(gfs) || !gfs) return { ...GFS_DEFAULT }
  return { daily: gfs.daily ?? 0, weekly: gfs.weekly ?? 0, monthly: gfs.monthly ?? 0 }
}

// keepLastFrom is the same for keep_last: a task without the rule edits from the
// default, not from 0.
export function keepLastFrom(ret: Retention | undefined): number {
  return ret?.keep_last && ret.keep_last > 0 ? ret.keep_last : KEEP_LAST_DEFAULT
}

// modeOf picks which rule a stored policy opens as. GFS wins when a policy
// carries both — the API allows that union and the forms cannot express it, so
// the form opens on the rule that is harder to retype and `bothRules` makes the
// loss explicit instead of letting a save swallow it.
export function modeOf(ret: Retention | undefined): RetentionMode {
  if (gfsActive(ret?.gfs)) return 'gfs'
  if (ret?.keep_last && ret.keep_last > 0) return 'keep_last'
  return 'none'
}

// bothRules reports a policy the forms cannot represent: keep_last AND GFS, as
// only the API can set it. Saving such a task through a form drops one rule, so
// the form has to say so first.
export function bothRules(ret: Retention | undefined): boolean {
  return Boolean(ret?.keep_last && ret.keep_last > 0 && gfsActive(ret?.gfs))
}

// buildRetention assembles the payload for the selected mode. Exactly one rule
// is sent; `none` sends an empty policy, which the backend reads as "keep
// everything" (an empty policy is never handed to Forget).
export function buildRetention(mode: RetentionMode, keepLast: number, gfs: GFS): Retention {
  if (mode === 'keep_last' && keepLast > 0) return { keep_last: keepLast }
  if (mode === 'gfs' && gfsActive(gfs)) {
    return { gfs: { daily: gfs.daily, weekly: gfs.weekly, monthly: gfs.monthly } }
  }
  return {}
}

// formatPolicy renders a policy in the notation the backend stores:
// "keep_last 14", "GFS 7/4/12", or both unioned — the last only reachable via
// the API. An empty policy is not a formatting edge case: it means the task
// keeps everything forever, which the caller flags.
export function formatPolicy(ret: Retention | undefined): string {
  const parts: string[] = []
  if (ret?.keep_last) parts.push(`keep_last ${ret.keep_last}`)
  const gfs = ret?.gfs
  if (gfsActive(gfs) && gfs) {
    parts.push(`GFS ${gfs.daily ?? 0}/${gfs.weekly ?? 0}/${gfs.monthly ?? 0}`)
  }
  return parts.join(' + ')
}

// clampCount parses a count input. A blank field is 0 — clearing a box is how a
// bucket is turned off. A negative or non-numeric entry is refused instead:
// `current` (the value the field already holds) is kept, because folding it to 0
// would silently disarm the rule the user is editing, which is exactly what the
// backend refuses to store (core.Retention.Validate).
export function clampCount(raw: string, current = 0): number {
  if (!raw.trim()) return 0
  const n = Number(raw)
  if (!Number.isFinite(n) || n < 0) return current
  return Math.floor(n)
}
