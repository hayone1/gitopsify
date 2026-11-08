// Types of gitopsify's API (internal/server).

export type Role = '' | 'viewer' | 'operator' | 'admin'

export interface User {
  username: string
  name?: string
  email?: string
  groups?: string[]
  via: 'local' | 'oidc' | 'token'
}

export interface Me {
  user: User
  role: Role
  serverRole: Role
  clusters: Record<string, Role>
  tools: Record<string, Role>
  csrf?: string
  authMode: 'local' | 'oidc'
  version: string
  expires?: string
}

export interface Info {
  version: string
  authMode: 'local' | 'oidc'
  local: boolean
}

export interface Cluster {
  name: string
  description?: string
  impersonate?: boolean
  role: Role
}

export interface ClusterCheck {
  ok: boolean
  version?: string
  identity?: string
  error?: string
}

export type RunType = 'scan' | 'migrate' | 'export'
export type RunStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'canceled'

export interface RunSummary {
  apps: number
  objects: number
  excluded: number
  files?: number
  warnings: number
  secretActions: number
}

export interface Publish {
  at: string
  by: string
  dir?: string
  remote: string
  branch: string
  commit?: string
  pushed: boolean
  error?: string
}

export interface Run {
  id: string
  type: RunType
  name: string
  cluster?: string
  tool?: string
  repo?: { url: string, branch?: string }
  status: RunStatus
  createdBy: string
  createdAt: string
  startedAt?: string
  finishedAt?: string
  error?: string
  spec?: unknown
  summary?: RunSummary
  publishes?: Publish[]
  from?: string
}

export interface LogLine {
  time: string
  level: 'info' | 'warn' | 'error'
  message: string
}

export interface AppSummary {
  name: string
  kind: string
  namespace?: string
  wave: number
  phase?: string
  objects: number
  kinds: string[]
  dependsOn?: string[]
  helm?: { release: string, chart: string, version: string, mode: string }
  notes?: string[]
}

export interface ScanResult {
  source: string
  apps: AppSummary[]
  warnings: string[]
  secretActions: string[]
  excluded?: { object: string, reason: string }[]
}

export interface FileEntry {
  path: string
  size: number
}

export interface AuditEvent {
  time: string
  actor: string
  via?: string
  action: string
  outcome: 'ok' | 'denied' | 'error'
  cluster?: string
  target?: string
  detail?: string
  ip?: string
  request?: string
}

const ranks: Record<string, number> = { viewer: 1, operator: 2, admin: 3 }

export function atLeast(role: Role | undefined, want: Exclude<Role, ''>): boolean {
  return (ranks[role || ''] || 0) >= ranks[want]!
}

export function statusColor(s: RunStatus): 'success' | 'error' | 'warning' | 'info' | 'neutral' {
  switch (s) {
    case 'succeeded': return 'success'
    case 'failed': return 'error'
    case 'canceled': return 'warning'
    case 'running': return 'info'
    default: return 'neutral'
  }
}

export function statusIcon(s: RunStatus): string {
  switch (s) {
    case 'succeeded': return 'i-lucide-circle-check'
    case 'failed': return 'i-lucide-circle-x'
    case 'canceled': return 'i-lucide-circle-slash'
    case 'running': return 'i-lucide-loader-circle'
    default: return 'i-lucide-clock'
  }
}

export function ago(iso?: string): string {
  if (!iso) return ''
  const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000)
  if (s < 45) return 'just now'
  if (s < 3600) return `${Math.round(s / 60)} min ago`
  if (s < 86400) return `${Math.round(s / 3600)} h ago`
  return new Date(iso).toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' })
}

export function duration(from?: string, to?: string): string {
  if (!from) return ''
  const ms = (to ? new Date(to).getTime() : Date.now()) - new Date(from).getTime()
  const s = Math.max(0, Math.round(ms / 1000))
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  return m < 60 ? `${m}m ${s % 60}s` : `${Math.floor(m / 60)}h ${m % 60}m`
}

export function formatTime(iso?: string): string {
  if (!iso) return ''
  return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

export function bytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`
  return `${(n / 1024 / 1024).toFixed(1)} MiB`
}

export interface Tool {
  name: string
  description?: string
  provider: string
  role: Role
  installed: boolean
  title?: string
}

export interface ProviderInfo {
  name: string
  title?: string
  description?: string
  source?: string
  homepage?: string
  maturity?: string
  outputs?: string[]
  installed?: string
  kinds?: string[]
}

// What a provider describes (the parts the UI uses).
export interface ProviderDescription {
  name: string
  title: string
  version: string
  partition: { singular: string, plural: string }
  kinds: { name: string, plural?: string, description?: string, optional?: boolean, terraform?: unknown, crossplane?: unknown }[]
  terraform?: { source: string, version: string }
  crossplane?: { package: string }
}

export interface ToolCheck {
  description?: ProviderDescription
  check?: {
    target: string
    identity?: string
    partitions: { id: string, name: string }[]
    kinds?: { kind: string, ok: boolean, message?: string }[]
    warnings?: string[]
  }
  error?: string
}

export function runTarget(r: Run): string {
  return r.cluster || r.tool || ''
}

export function runTypeLabel(t: RunType): string {
  return t === 'migrate' ? 'migration' : t
}
