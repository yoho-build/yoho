import { Fragment, useCallback, useEffect, useRef, useState } from "react"
import {
  ActivityIcon,
  BoxIcon,
  ChevronDownIcon,
  ChevronRightIcon,
  DatabaseIcon,
  CircleAlertIcon,
  HardDriveIcon,
  HistoryIcon,
  LockIcon,
  MoonIcon,
  PauseIcon,
  PlayIcon,
  RefreshCwIcon,
  ScrollTextIcon,
  ServerIcon,
  SunIcon,
  WaypointsIcon,
} from "lucide-react"

import { api, ApiError } from "@/api"
import { ago, bytes, duration, shortVersion } from "@/format"
import type { Container, Disk, DiskUsage, Meta, Release, Server, ServerDisk, Snapshot } from "@/types"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog"
import { Meter } from "@/components/ui/meter"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"

type Theme = "light" | "dark"

function initialTheme(): Theme {
  try {
    const t = localStorage.getItem("yoho-theme")
    if (t === "light" || t === "dark") return t
  } catch {
    // storage unavailable
  }
  return window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "light"
}

export default function App() {
  const [meta, setMeta] = useState<Meta | null>(null)
  const [snap, setSnap] = useState<Snapshot | null>(null)
  const [error, setError] = useState<ApiError | Error | null>(null)
  const [loading, setLoading] = useState(false)
  const [paused, setPaused] = useState(false)
  const [theme, setTheme] = useState<Theme>(initialTheme)
  const [now, setNow] = useState(() => Date.now())
  const [logs, setLogs] = useState<{ server: string; container: Container } | null>(null)
  const inflight = useRef<AbortController | null>(null)
  const disk = useDisk()

  useEffect(() => {
    document.documentElement.classList.toggle("dark", theme === "dark")
    try {
      localStorage.setItem("yoho-theme", theme)
    } catch {
      // storage unavailable
    }
  }, [theme])

  const load = useCallback(async (fresh: boolean) => {
    if (inflight.current) {
      if (!fresh) return
      inflight.current.abort()
    }
    const ctl = new AbortController()
    inflight.current = ctl
    setLoading(true)
    try {
      const s = await api.snapshot(fresh, ctl.signal)
      setSnap(s)
      setError(null)
    } catch (e) {
      if (!ctl.signal.aborted) setError(e as Error)
    } finally {
      if (inflight.current === ctl) {
        inflight.current = null
        setLoading(false)
      }
    }
  }, [])

  useEffect(() => {
    api.meta().then(setMeta, (e) => setError(e))
    load(false)
  }, [load])

  const interval = (meta?.interval_seconds ?? 5) * 1000
  useEffect(() => {
    if (paused) return
    const id = window.setInterval(() => {
      setNow(Date.now())
      if (!document.hidden) load(false)
    }, interval)
    return () => window.clearInterval(id)
  }, [interval, paused, load])

  if (error instanceof ApiError && error.status === 401) {
    return <SignedOut hint={error.hint} />
  }

  return (
    <div className="mx-auto flex min-h-svh max-w-7xl flex-col gap-5 px-4 py-5 sm:px-6">
      <header className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          <div className="grid size-9 place-items-center rounded-lg bg-primary text-primary-foreground font-bold">Y</div>
          <div className="min-w-0">
            <h1 className="truncate text-lg leading-tight font-semibold">
              {snap ? snap.app : "Yoho"}
              {snap && <span className="text-muted-foreground"> · {snap.destination}</span>}
            </h1>
            <p className="truncate text-xs text-muted-foreground">
              {snap ? (
                <>
                  {snap.runtime} · project <code>{snap.project}</code> · updated {ago(snap.collected_at, now)}
                </>
              ) : (
                "Connecting…"
              )}
              {meta && <> · yoho {meta.version}</>}
            </p>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => setPaused((p) => !p)} aria-pressed={paused}>
            {paused ? <PlayIcon /> : <PauseIcon />}
            {paused ? "Resume" : `Auto ${interval / 1000}s`}
          </Button>
          <Button variant="outline" size="sm" onClick={() => load(true)} disabled={loading}>
            <RefreshCwIcon className={loading ? "animate-spin" : undefined} />
            Refresh
          </Button>
          <Button
            variant="ghost"
            size="icon"
            aria-label="Toggle theme"
            onClick={() => setTheme((t) => (t === "dark" ? "light" : "dark"))}
          >
            {theme === "dark" ? <SunIcon /> : <MoonIcon />}
          </Button>
        </div>
      </header>

      {error && (
        <div className="flex items-start gap-2 rounded-lg border border-destructive/40 bg-destructive/10 px-4 py-3 text-sm text-destructive">
          <CircleAlertIcon className="mt-0.5 size-4 shrink-0" />
          <span className="break-all">{error.message}</span>
        </div>
      )}

      {!snap && !error && <SkeletonGrid />}

      {snap && (
        <>
          <Summary snap={snap} />
          {snap.servers.length === 1 ? (
            <ServerView server={snap.servers[0]} now={now} disk={disk} onLogs={(c) => setLogs({ server: snap.servers[0].name, container: c })} />
          ) : (
            <Tabs defaultValue={snap.servers[0]?.name}>
              <TabsList>
                {snap.servers.map((s) => (
                  <TabsTrigger key={s.name} value={s.name}>
                    <ServerIcon />
                    {s.name}
                    {s.error && <CircleAlertIcon className="text-destructive" />}
                  </TabsTrigger>
                ))}
              </TabsList>
              {snap.servers.map((s) => (
                <TabsContent key={s.name} value={s.name}>
                  <ServerView server={s} now={now} disk={disk} onLogs={(c) => setLogs({ server: s.name, container: c })} />
                </TabsContent>
              ))}
            </Tabs>
          )}
        </>
      )}

      <footer className="mt-auto pt-4 text-center text-xs text-muted-foreground">
        Read-only view over SSH · nothing runs on your Servers · stop with Ctrl-C in the terminal
      </footer>

      <LogsDialog target={logs} onClose={() => setLogs(null)} />
    </div>
  )
}

function SignedOut({ hint }: { hint?: string }) {
  return (
    <div className="grid min-h-svh place-items-center px-4">
      <Card className="max-w-md">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <LockIcon className="size-4" /> Not signed in
          </CardTitle>
        </CardHeader>
        <CardContent className="text-sm text-muted-foreground">
          {hint ?? "Open the login link printed by `yoho ui`."} Login links work once; this browser stays signed in until{" "}
          <code>yoho ui</code> stops. Restart it for a new link.
        </CardContent>
      </Card>
    </div>
  )
}

function SkeletonGrid() {
  return (
    <div className="grid gap-4 md:grid-cols-3">
      {[0, 1, 2].map((i) => (
        <div key={i} className="h-32 animate-pulse rounded-xl bg-muted" />
      ))}
    </div>
  )
}

function Summary({ snap }: { snap: Snapshot }) {
  const all = snap.servers.flatMap((s) => s.containers)
  const active = all.filter((c) => !c.completed)
  const running = active.filter((c) => c.state === "running").length
  const unhealthy = active.filter((c) => c.health === "unhealthy" || c.state !== "running").length
  const failing = snap.servers.filter((s) => s.error).length
  const current = snap.servers.map((s) => s.releases.find((r) => r.current)).find(Boolean)
  const stats = [
    { label: "Servers", value: `${snap.servers.length - failing}/${snap.servers.length}`, sub: failing ? `${failing} unreachable` : "reachable", bad: failing > 0, icon: ServerIcon },
    { label: "Containers", value: `${running}/${active.length}`, sub: all.length > active.length ? `running · ${all.length - active.length} completed` : "running", bad: running < active.length, icon: BoxIcon },
    { label: "Attention", value: String(unhealthy), sub: unhealthy ? "unhealthy or stopped" : "all healthy", bad: unhealthy > 0, icon: ActivityIcon },
    { label: "Current Release", value: current ? shortVersion(current.version) : "—", sub: current ? `by ${current.performer || "?"}` : "no Release yet", bad: false, icon: HistoryIcon },
  ]
  return (
    <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
      {stats.map((s) => (
        <Card key={s.label} className="gap-1 py-4">
          <CardContent className="flex flex-col gap-1">
            <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <s.icon className="size-3.5" />
              {s.label}
            </div>
            <div className={`truncate font-mono text-xl font-semibold ${s.bad ? "text-destructive" : ""}`}>{s.value}</div>
            <div className="truncate text-xs text-muted-foreground">{s.sub}</div>
          </CardContent>
        </Card>
      ))}
    </div>
  )
}

function ServerView({ server, now, disk, onLogs }: { server: Server; now: number; disk: DiskState; onLogs: (c: Container) => void }) {
  const h = server.host
  const memUsed = h.mem_total_bytes - h.mem_available_bytes
  const memPct = h.mem_total_bytes ? (memUsed / h.mem_total_bytes) * 100 : 0
  const loadPct = h.cpus ? (h.load[0] / h.cpus) * 100 : 0
  return (
    <div className="flex flex-col gap-4">
      {server.error && (
        <div className="flex items-start gap-2 rounded-lg border border-destructive/40 bg-destructive/10 px-4 py-3 text-sm text-destructive">
          <CircleAlertIcon className="mt-0.5 size-4 shrink-0" />
          <div className="min-w-0">
            <div className="font-medium">{server.name} could not be read completely</div>
            <div className="font-mono text-xs break-all">{server.error}</div>
          </div>
        </div>
      )}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader>
            <div className="min-w-0">
              <CardTitle className="flex items-center gap-2">
                <ServerIcon className="size-4" />
                {server.name}
                {h.hostname && h.hostname !== server.name && <span className="font-normal text-muted-foreground">({h.hostname})</span>}
              </CardTitle>
              <CardDescription className="mt-1 break-words">
                <code>{server.ssh}</code> · {h.os || "unknown OS"} · {h.kernel} · {h.arch}
              </CardDescription>
            </div>
            <div className="flex flex-wrap gap-1.5">
              <Badge variant="outline">up {duration(h.uptime_seconds)}</Badge>
              {h.docker_version ? <Badge variant="outline">docker {h.docker_version}</Badge> : <Badge variant="destructive">docker unavailable</Badge>}
              <Badge variant="secondary">{server.elapsed_seconds.toFixed(1)}s</Badge>
            </div>
          </CardHeader>
          <CardContent className="grid gap-4 sm:grid-cols-2">
            <Meter label={`Load (${h.cpus} CPU)`} value={loadPct} detail={h.load.map((l) => l.toFixed(2)).join(" · ")} />
            <Meter label="Memory" value={memPct} detail={`${bytes(memUsed)} / ${bytes(h.mem_total_bytes)}`} />
            {(h.disks ?? []).map((d) => (
              <Meter
                key={d.mount + d.label}
                label={`Disk ${d.mount}`}
                value={d.total_bytes ? (d.used_bytes / d.total_bytes) * 100 : 0}
                detail={`${bytes(d.avail_bytes)} free of ${bytes(d.total_bytes)}`}
              />
            ))}
            {h.docker_error && <p className="font-mono text-xs break-all text-destructive sm:col-span-2">{h.docker_error}</p>}
          </CardContent>
        </Card>
        <ProxyCard proxy={server.proxy} onLogs={onLogs} />
      </div>

      <VolumesCard server={server} disk={disk} now={now} />

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <BoxIcon className="size-4" /> Services
          </CardTitle>
          <CardDescription>{server.containers.length} containers</CardDescription>
        </CardHeader>
        <CardContent>
          {server.containers.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">No containers for this App on {server.name}. Deploy with <code>yoho deploy</code>.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Service</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead className="text-right">CPU</TableHead>
                  <TableHead>Memory</TableHead>
                  <TableHead className="hidden md:table-cell">Net I/O</TableHead>
                  <TableHead className="hidden lg:table-cell">Image</TableHead>
                  <TableHead className="hidden xl:table-cell">Ports</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {server.containers.map((c) => (
                  <TableRow key={c.id || c.name}>
                    <TableCell>
                      <div className="font-medium">{c.service || "—"}</div>
                      <div className="max-w-56 truncate font-mono text-xs text-muted-foreground" title={c.name}>
                        {c.name}
                      </div>
                    </TableCell>
                    <TableCell>
                      <StateBadge c={c} />
                      <div className="mt-1 max-w-48 truncate text-xs text-muted-foreground" title={c.status}>
                        {c.status}
                      </div>
                    </TableCell>
                    <TableCell className="text-right font-mono">{c.state === "running" ? `${c.cpu_percent.toFixed(1)}%` : "—"}</TableCell>
                    <TableCell className="font-mono text-xs">{c.mem_usage || "—"}</TableCell>
                    <TableCell className="hidden font-mono text-xs md:table-cell">{c.net_io || "—"}</TableCell>
                    <TableCell className="hidden max-w-64 truncate font-mono text-xs lg:table-cell" title={c.image}>
                      {c.image}
                    </TableCell>
                    <TableCell className="hidden max-w-48 truncate font-mono text-xs xl:table-cell" title={c.ports}>
                      {c.ports || "—"}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button variant="ghost" size="sm" onClick={() => onLogs(c)}>
                        <ScrollTextIcon />
                        Logs
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <ReleasesCard releases={server.releases} now={now} />
    </div>
  )
}

function StateBadge({ c }: { c: Container }) {
  if (c.completed) return <Badge variant="secondary">completed</Badge>
  if (c.state !== "running") return <Badge variant="destructive">{c.state || "unknown"}</Badge>
  if (c.health === "unhealthy") return <Badge variant="destructive">unhealthy</Badge>
  if (c.health === "starting") return <Badge variant="warning">starting</Badge>
  if (c.health === "healthy") return <Badge variant="success">healthy</Badge>
  return <Badge variant="success">running</Badge>
}

function ProxyCard({ proxy, onLogs }: { proxy?: Container; onLogs: (c: Container) => void }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <WaypointsIcon className="size-4" /> Proxy
        </CardTitle>
        {proxy ? <StateBadge c={proxy} /> : <Badge variant="outline">not running</Badge>}
      </CardHeader>
      <CardContent className="flex flex-col gap-2 text-sm">
        {proxy ? (
          <>
            <div className="truncate font-mono text-xs" title={proxy.image}>
              {proxy.image}
            </div>
            <div className="text-xs text-muted-foreground">{proxy.status}</div>
            <div className="truncate font-mono text-xs text-muted-foreground" title={proxy.ports}>
              {proxy.ports || "no published ports"}
            </div>
            <div className="flex items-center justify-between pt-1">
              <span className="font-mono text-xs">
                {proxy.cpu_percent.toFixed(1)}% · {proxy.mem_usage || "—"}
              </span>
              <Button variant="ghost" size="sm" onClick={() => onLogs(proxy)}>
                <ScrollTextIcon />
                Logs
              </Button>
            </div>
          </>
        ) : (
          <p className="text-xs text-muted-foreground">
            kamal-proxy (<code>yoho-proxy</code>) is not on this Server. Apps without proxied Services do not need it.
          </p>
        )}
      </CardContent>
    </Card>
  )
}

function ReleasesCard({ releases, now }: { releases: Release[]; now: number }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <HistoryIcon className="size-4" /> Releases
        </CardTitle>
        <CardDescription>newest first</CardDescription>
      </CardHeader>
      <CardContent>
        {releases.length === 0 ? (
          <p className="py-4 text-center text-sm text-muted-foreground">No Releases recorded.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Version</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Deployed</TableHead>
                <TableHead>By</TableHead>
                <TableHead className="hidden md:table-cell">Images</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {releases.slice(0, 10).map((r) => (
                <TableRow key={r.version + r.deployed_at}>
                  <TableCell className="font-mono">
                    {shortVersion(r.version)} {r.current && <Badge className="ml-1">current</Badge>}
                  </TableCell>
                  <TableCell>
                    <Badge variant={r.status === "deployed" ? "success" : r.status === "failed" ? "destructive" : "secondary"}>{r.status}</Badge>
                  </TableCell>
                  <TableCell title={r.deployed_at}>{ago(r.deployed_at, now)}</TableCell>
                  <TableCell>{r.performer || "—"}</TableCell>
                  <TableCell className="hidden max-w-96 truncate font-mono text-xs text-muted-foreground md:table-cell">
                    {Object.entries(r.images ?? {})
                      .map(([s, i]) => `${s}=${i}`)
                      .join("  ")}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  )
}

function LogsDialog({ target, onClose }: { target: { server: string; container: Container } | null; onClose: () => void }) {
  const [lines, setLines] = useState(200)
  const [text, setText] = useState("")
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const pre = useRef<HTMLPreElement>(null)

  const fetchLogs = useCallback(
    async (signal?: AbortSignal) => {
      if (!target) return
      setBusy(true)
      try {
        const r = await api.logs(target.server, target.container.name, lines, signal)
        setText(r.logs)
        setErr(null)
      } catch (e) {
        if (!signal?.aborted) setErr((e as Error).message)
      } finally {
        if (!signal?.aborted) setBusy(false)
      }
    },
    [target, lines],
  )

  useEffect(() => {
    if (!target) {
      setText("")
      setErr(null)
      return
    }
    const ctl = new AbortController()
    fetchLogs(ctl.signal)
    return () => ctl.abort()
  }, [target, fetchLogs])

  useEffect(() => {
    if (pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [text])

  return (
    <Dialog open={target !== null} onOpenChange={(open) => !open && onClose()}>
      <DialogContent>
        <DialogTitle className="flex items-center gap-2">
          <ScrollTextIcon className="size-4" />
          {target?.container.service || "container"} logs
        </DialogTitle>
        <DialogDescription className="font-mono text-xs">
          {target?.container.name} on {target?.server}
        </DialogDescription>
        <div className="flex flex-wrap items-center gap-2">
          {[100, 200, 1000].map((n) => (
            <Button key={n} size="sm" variant={lines === n ? "secondary" : "ghost"} onClick={() => setLines(n)}>
              {n} lines
            </Button>
          ))}
          <Button size="sm" variant="outline" className="ml-auto" disabled={busy} onClick={() => fetchLogs()}>
            <RefreshCwIcon className={busy ? "animate-spin" : undefined} />
            Reload
          </Button>
        </div>
        {err && <p className="font-mono text-xs break-all text-destructive">{err}</p>}
        <pre
          ref={pre}
          className="min-h-48 flex-1 overflow-auto rounded-lg bg-muted p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap break-all"
        >
          {text || (busy ? "Loading…" : "No output.")}
        </pre>
        <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <HardDriveIcon className="size-3" /> Container output is shown as Docker returns it. Avoid printing secrets in application logs.
        </p>
      </DialogContent>
    </Dialog>
  )
}

type DiskState = {
  data: DiskUsage | null
  loading: boolean
  error: Error | null
  load: (fresh: boolean) => void
}

// useDisk loads volume sizes on demand only: du walks every file, so the
// page never polls it. One request covers every Server.
function useDisk(): DiskState {
  const [data, setData] = useState<DiskUsage | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<Error | null>(null)
  const busy = useRef(false)
  const load = useCallback(async (fresh: boolean) => {
    if (busy.current) return
    busy.current = true
    setLoading(true)
    try {
      setData(await api.disk(fresh))
      setError(null)
    } catch (e) {
      setError(e as Error)
    } finally {
      busy.current = false
      setLoading(false)
    }
  }, [])
  return { data, loading, error, load }
}

// dockerDisk is the filesystem holding Docker's root, where plain local
// volumes live; the Yoho root's when Docker shares it.
function dockerDisk(server: Server): Disk | undefined {
  const disks = server.host.disks ?? []
  return disks.find((d) => d.label === "docker") ?? disks[0]
}

function accessLabel(d: ServerDisk): string {
  if (d.access === "root") return "as root"
  if (d.access === "sudo") return `${d.user ?? "SSH user"} via sudo`
  if (d.access === "scoped") return "scoped access"
  return "not sized"
}

function VolumesCard({ server, disk, now }: { server: Server; disk: DiskState; now: number }) {
  const [open, setOpen] = useState<Record<string, boolean>>({})
  const d = disk.data?.servers.find((s) => s.name === server.name)
  const fs = dockerDisk(server)
  return (
    <Card>
      <CardHeader>
        <div className="min-w-0">
          <CardTitle className="flex items-center gap-2">
            <DatabaseIcon className="size-4" /> Volumes
          </CardTitle>
          <CardDescription>
            {disk.data
              ? <>sized {ago(disk.data.collected_at, now)}{d && <> · {accessLabel(d)} · {d.elapsed_seconds.toFixed(1)}s</>}</>
              : "Sizes of the App directory and the App's volumes, measured with du on demand."}
          </CardDescription>
        </div>
        <Button variant="outline" size="sm" onClick={() => disk.load(!!disk.data)} disabled={disk.loading}>
          <HardDriveIcon className={disk.loading ? "animate-pulse" : undefined} />
          {disk.loading ? "Sizing…" : disk.data ? "Rescan" : "Show disk usage"}
        </Button>
      </CardHeader>
      {(disk.error || d) && (
        <CardContent className="flex flex-col gap-3">
          {disk.error && <p className="font-mono text-xs break-all text-destructive">{disk.error.message}</p>}
          {d?.error && <p className="font-mono text-xs break-all text-destructive">{d.error}</p>}
          {d?.note && (
            <div className="flex items-start gap-2 rounded-lg border border-warning/40 bg-warning/10 px-3 py-2 text-sm">
              <CircleAlertIcon className="mt-0.5 size-4 shrink-0" />
              <span className="break-words">{d.note}</span>
            </div>
          )}
          {d && (
            <p className="text-sm">
              <span className="font-medium">App dir</span>{" "}
              <span className="tabular-nums">{d.app_dir_sized ? `${d.app_dir_partial ? "≥" : ""}${bytes(d.app_dir_bytes)}` : "—"}</span>{" "}
              <code className="text-xs text-muted-foreground break-all">{d.app_dir}</code>
            </p>
          )}
          {d && d.volumes.length === 0 && <p className="text-sm text-muted-foreground">No volumes for this App on {server.name}.</p>}
          {d && d.volumes.length > 0 && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Volume</TableHead>
                  <TableHead className="hidden sm:table-cell">Services</TableHead>
                  <TableHead className="text-right">Size</TableHead>
                  <TableHead className="hidden md:table-cell w-48">{fs ? `Of disk ${fs.mount}` : "Of disk"}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {d.volumes.map((v) => {
                  const expandable = v.sized && v.top.length > 0
                  const isOpen = !!open[v.name]
                  const pct = v.sized && fs?.total_bytes ? (v.bytes / fs.total_bytes) * 100 : null
                  return (
                    <Fragment key={v.name}>
                      <TableRow>
                        <TableCell>
                          <button
                            type="button"
                            className="flex items-center gap-1 text-left disabled:cursor-default"
                            disabled={!expandable}
                            aria-expanded={expandable ? isOpen : undefined}
                            onClick={() => setOpen((o) => ({ ...o, [v.name]: !o[v.name] }))}
                          >
                            {expandable ? (isOpen ? <ChevronDownIcon className="size-3.5" /> : <ChevronRightIcon className="size-3.5" />) : <span className="w-3.5" />}
                            <span className="max-w-72 truncate font-mono text-xs" title={v.name}>{v.name}</span>
                          </button>
                          {v.error && <div className="pl-4.5 text-xs text-warning break-words">{v.error}</div>}
                        </TableCell>
                        <TableCell className="hidden sm:table-cell text-xs">{v.services.join(", ") || "—"}</TableCell>
                        <TableCell className="text-right tabular-nums">
                          {v.sized ? <span className={v.partial ? "text-warning" : undefined}>{v.partial ? "≥" : ""}{bytes(v.bytes)}</span> : "—"}
                        </TableCell>
                        <TableCell className="hidden md:table-cell">
                          {pct !== null && <Meter label={v.name} value={pct} detail={`${pct.toFixed(pct < 1 ? 1 : 0)}%`} className="[&_span:first-child]:sr-only" />}
                        </TableCell>
                      </TableRow>
                      {isOpen &&
                        v.top.map((e) => (
                          <TableRow key={v.name + "/" + e.name} className="text-muted-foreground">
                            <TableCell className="pl-9">
                              <span className="max-w-72 truncate font-mono text-xs block" title={e.name}>{e.name}</span>
                            </TableCell>
                            <TableCell className="hidden sm:table-cell" />
                            <TableCell className="text-right tabular-nums text-xs">{bytes(e.bytes)}</TableCell>
                            <TableCell className="hidden md:table-cell" />
                          </TableRow>
                        ))}
                    </Fragment>
                  )
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      )}
    </Card>
  )
}
