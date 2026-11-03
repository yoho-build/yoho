import { ChartLineIcon, CircleAlertIcon } from "lucide-react"

import type { RouteTraffic, Traffic } from "@/types"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"

export function rate(v?: number): string {
  if (v === undefined) return "—"
  if (v >= 100) return v.toFixed(0)
  if (v >= 10) return v.toFixed(1)
  return v.toFixed(2)
}

export function latency(s?: number): string {
  if (s === undefined) return "—"
  if (s < 0.001) return `${Math.round(s * 1e6)}µs`
  if (s < 1) return `${Math.round(s * 1000)}ms`
  return `${s.toFixed(2)}s`
}

// Sparkline of requests/min, scaled to the window's maximum.
function Sparkline({ values }: { values: number[] }) {
  const w = 160
  const h = 36
  if (values.length < 2) {
    return <div className="flex h-9 items-center text-xs text-muted-foreground">collecting…</div>
  }
  const hi = Math.max(...values, 1e-9)
  const step = w / (values.length - 1)
  const pts = values.map((v, i) => `${(i * step).toFixed(1)},${(h - 2 - (v / hi) * (h - 4)).toFixed(1)}`)
  return (
    <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className="h-9 w-full text-primary" role="img" aria-label={`requests per minute, last ${values.length} refreshes`}>
      <polygon points={`0,${h} ${pts.join(" ")} ${w},${h}`} className="fill-current opacity-15" />
      <polyline points={pts.join(" ")} fill="none" stroke="currentColor" strokeWidth="1.5" vectorEffect="non-scaling-stroke" strokeLinejoin="round" />
    </svg>
  )
}

function Stat({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div className="min-w-0">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={`truncate font-mono text-base font-semibold tabular-nums ${tone ?? ""}`}>{value}</div>
    </div>
  )
}

function RouteRow({ r }: { r: RouteTraffic }) {
  const errTone = r.error_percent === undefined ? "" : r.error_percent >= 5 ? "text-destructive" : r.error_percent > 0 ? "text-warning" : ""
  return (
    <div className="flex flex-col gap-3 rounded-lg border p-3 md:flex-row md:items-center md:gap-5">
      <div className="min-w-0 md:w-48">
        <div className="font-medium">{r.service}</div>
        <div className="truncate font-mono text-xs text-muted-foreground" title={r.route}>
          {r.hosts.length ? r.hosts.join(", ") : "* (catch-all)"}
        </div>
      </div>
      <div className="grid flex-1 grid-cols-2 gap-3 sm:grid-cols-4">
        <Stat label="req/min" value={rate(r.requests_per_min)} />
        <Stat label="5xx" value={r.error_percent === undefined ? "—" : `${r.error_percent.toFixed(1)}%`} tone={errTone} />
        <Stat label="p50" value={latency(r.p50_seconds)} />
        <Stat label="p95" value={latency(r.p95_seconds)} />
      </div>
      <div className="md:w-44">
        <Sparkline values={r.history} />
        <div className="text-right text-[11px] text-muted-foreground tabular-nums">
          {r.requests_total.toLocaleString()} total · {r.errors_5xx_total.toLocaleString()} 5xx
        </div>
      </div>
    </div>
  )
}

export function TrafficCard({ traffic }: { traffic?: Traffic }) {
  if (!traffic) return null
  return (
    <Card>
      <CardHeader>
        <div className="min-w-0">
          <CardTitle className="flex items-center gap-2">
            <ChartLineIcon className="size-4" /> Traffic
          </CardTitle>
          <CardDescription className="mt-1">
            {traffic.enabled ? "per proxied Service, from yoho-proxy metrics · rates since the previous refresh" : "Proxy request metrics are off"}
          </CardDescription>
        </div>
        {traffic.enabled ? <Badge variant="outline">live · in memory only</Badge> : <Badge variant="secondary">off</Badge>}
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {!traffic.enabled && (
          <p className="text-sm text-muted-foreground">
            Set <code>proxy.metrics: true</code> in the Yoho file, then run <code>yoho proxy boot</code> (or deploy) to see req/min, 5xx rate and latency.
          </p>
        )}
        {traffic.error && (
          <div className="flex items-start gap-2 text-sm text-destructive">
            <CircleAlertIcon className="mt-0.5 size-4 shrink-0" />
            <span className="font-mono text-xs break-all">{traffic.error}</span>
          </div>
        )}
        {traffic.enabled && traffic.routes.map((r) => <RouteRow key={r.route} r={r} />)}
      </CardContent>
    </Card>
  )
}
