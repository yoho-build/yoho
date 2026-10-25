import type { DiskUsage, Meta, Snapshot } from "./types"

export class ApiError extends Error {
  status: number
  hint?: string
  constructor(status: number, message: string, hint?: string) {
    super(message)
    this.status = status
    this.hint = hint
  }
}

// The login link redirects to /#session=TOKEN. Keep the token in this
// origin's storage (scoped by port, unlike cookies) and drop it from the URL
// and history. Storage can be unavailable; then it lives for this page only.
const KEY = "yoho-session"
let session = ""

function takeSession(): string {
  const m = /^#session=([0-9a-f]{64})$/.exec(window.location.hash)
  if (m) {
    session = m[1]
    try {
      localStorage.setItem(KEY, session)
    } catch {
      // keep it in memory
    }
    window.history.replaceState(null, "", window.location.pathname + window.location.search)
    return session
  }
  if (!session) {
    try {
      session = localStorage.getItem(KEY) ?? ""
    } catch {
      // no storage
    }
  }
  return session
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const token = takeSession()
  const res = await fetch(path, {
    credentials: "omit",
    cache: "no-store",
    signal,
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  })
  let body: unknown = null
  try {
    body = await res.json()
  } catch {
    // non-JSON error page
  }
  if (!res.ok) {
    const b = (body ?? {}) as { error?: string; hint?: string }
    throw new ApiError(res.status, b.error ?? `HTTP ${res.status}`, b.hint)
  }
  return body as T
}

export const api = {
  meta: (signal?: AbortSignal) => get<Meta>("/api/meta", signal),
  snapshot: (fresh: boolean, signal?: AbortSignal) => get<Snapshot>(`/api/snapshot${fresh ? "?fresh=1" : ""}`, signal),
  disk: (fresh: boolean, signal?: AbortSignal) => get<DiskUsage>(`/api/disk${fresh ? "?fresh=1" : ""}`, signal),
  logs: (server: string, container: string, lines: number, signal?: AbortSignal) =>
    get<{ logs: string }>(
      `/api/logs?${new URLSearchParams({ server, container, lines: String(lines) })}`,
      signal,
    ),
}
