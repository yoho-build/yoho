// Mirrors internal/dash (json tags are the contract).

export interface Snapshot {
  app: string
  destination: string
  runtime: string
  project: string
  collected_at: string
  servers: Server[]
}

export interface Server {
  name: string
  ssh: string
  error?: string
  host: Host
  proxy?: Container
  containers: Container[]
  releases: Release[]
  elapsed_seconds: number
}

export interface Host {
  hostname: string
  os: string
  kernel: string
  arch: string
  cpus: number
  uptime_seconds: number
  load: [number, number, number]
  mem_total_bytes: number
  mem_available_bytes: number
  disks: Disk[] | null
  docker_version: string
  docker_error?: string
}

export interface Disk {
  label: string
  path: string
  mount: string
  total_bytes: number
  used_bytes: number
  avail_bytes: number
}

export interface Container {
  id: string
  name: string
  service: string
  image: string
  state: string
  completed: boolean
  health: string
  status: string
  ports: string
  running_for: string
  cpu_percent: number
  mem_percent: number
  mem_usage: string
  net_io: string
  block_io: string
  pids: string
}

export interface Release {
  version: string
  status: string
  deployed_at: string
  performer: string
  current: boolean
  images: Record<string, string> | null
}

export interface Meta {
  version: string
  interval_seconds: number
}

// Mirrors dash.DiskUsage: on-demand sizes from du (GET /api/disk).
export interface DiskUsage {
  collected_at: string
  servers: ServerDisk[]
}

export interface ServerDisk {
  name: string
  error?: string
  access: "root" | "sudo" | "none" | "no-timeout" | "scoped" | string
  user?: string
  note?: string
  app_dir: string
  app_dir_sized: boolean
  app_dir_partial: boolean
  app_dir_bytes: number
  volumes: Volume[]
  elapsed_seconds: number
}

export interface Volume {
  name: string
  key: string
  driver: string
  services: string[]
  sized: boolean
  partial: boolean
  bytes: number
  top: DirEntry[]
  error?: string
}

export interface DirEntry {
  name: string
  bytes: number
}
