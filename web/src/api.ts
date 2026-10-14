import type { Meta, Snapshot } from "./types"

export class ApiError extends Error {
  status: number
  hint?: string
  constructor(status: number, message: string, hint?: string) {
    super(message)
    this.status = status
    this.hint = hint
  }
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(path, { credentials: "same-origin", cache: "no-store", signal })
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
  logs: (server: string, container: string, lines: number, signal?: AbortSignal) =>
    get<{ logs: string }>(
      `/api/logs?${new URLSearchParams({ server, container, lines: String(lines) })}`,
      signal,
    ),
}
