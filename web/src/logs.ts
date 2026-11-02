// Parse `docker logs --timestamps` output into lines with a separate
// timestamp and ANSI SGR color spans. Other escape sequences are dropped.

export type LogSpan = { text: string; fg?: number; bold?: boolean; dim?: boolean }
export type LogLine = { ts?: string; spans: LogSpan[] }

const TS = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})) ?/
// SGR with ESC, SGR whose ESC was lost upstream, and any other CSI/OSC escape.
const ESC = /\x1b\[([0-9;]*)m|\[((?:\d{1,3};)*\d{1,3})m|\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b/g

type State = { fg?: number; bold?: boolean; dim?: boolean }

function applySGR(s: State, params: string): State {
  const codes = params === "" ? [0] : params.split(";").map(Number)
  const next = { ...s }
  for (let i = 0; i < codes.length; i++) {
    const c = codes[i]
    if (c === 0) {
      delete next.fg
      delete next.bold
      delete next.dim
    } else if (c === 1) next.bold = true
    else if (c === 2) next.dim = true
    else if (c === 22) {
      delete next.bold
      delete next.dim
    } else if ((c >= 30 && c <= 37) || (c >= 90 && c <= 97)) next.fg = c
    else if (c === 39) delete next.fg
    else if (c === 38 || c === 48) i += codes[i + 1] === 5 ? 2 : codes[i + 1] === 2 ? 4 : 0
  }
  return next
}

export function parseLogs(raw: string): LogLine[] {
  const text = raw.replace(/\r\n/g, "\n").replace(/\n$/, "")
  if (!text) return []
  let state: State = {}
  return text.split("\n").map((line) => {
    let ts: string | undefined
    const m = TS.exec(line)
    if (m) {
      ts = m[1]
      line = line.slice(m[0].length)
    }
    // Progress bars rewrite the line with \r; keep the final state.
    line = line.slice(line.lastIndexOf("\r") + 1)
    const spans: LogSpan[] = []
    let last = 0
    const push = (t: string) => t && spans.push({ text: t, ...state })
    for (const e of line.matchAll(ESC)) {
      push(line.slice(last, e.index))
      last = e.index + e[0].length
      const params = e[1] ?? e[2]
      if (params !== undefined) state = applySGR(state, params)
    }
    push(line.slice(last))
    return { ts, spans }
  })
}

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]

// shortTime renders an RFC 3339 timestamp as "Nov 01 19:05:45" in the
// Server's own offset, so it matches what the container printed.
export function shortTime(ts: string): string {
  const m = /^\d{4}-(\d{2})-(\d{2})T(\d{2}:\d{2}:\d{2})/.exec(ts)
  if (!m) return ts
  return `${MONTHS[Number(m[1]) - 1] ?? m[1]} ${m[2]} ${m[3]}`
}

const FG: Record<number, string> = {
  30: "text-zinc-500",
  31: "text-red-600 dark:text-red-400",
  32: "text-green-700 dark:text-green-400",
  33: "text-amber-700 dark:text-amber-300",
  34: "text-blue-700 dark:text-blue-400",
  35: "text-fuchsia-700 dark:text-fuchsia-400",
  36: "text-cyan-700 dark:text-cyan-400",
  37: "text-zinc-600 dark:text-zinc-300",
  90: "text-zinc-500",
  91: "text-red-500 dark:text-red-300",
  92: "text-green-600 dark:text-green-300",
  93: "text-amber-600 dark:text-amber-200",
  94: "text-blue-600 dark:text-blue-300",
  95: "text-fuchsia-600 dark:text-fuchsia-300",
  96: "text-cyan-600 dark:text-cyan-300",
  97: "text-foreground",
}

export function spanClass(s: LogSpan): string | undefined {
  const c = [s.fg !== undefined ? FG[s.fg] : "", s.bold ? "font-semibold" : "", s.dim ? "opacity-60" : ""]
    .filter(Boolean)
    .join(" ")
  return c || undefined
}
