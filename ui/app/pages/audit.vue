<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'
import type { AuditEvent } from '~/utils/types'

definePageMeta({ role: 'admin' })

const notify = useNotify()
const events = ref<AuditEvent[]>([])
const loading = ref(true)
const more = ref(true)
const actor = ref('')
const action = ref('')
const outcome = ref('all')
const UBadge = resolveComponent('UBadge')

const outcomeColor: Record<string, 'success' | 'error' | 'warning'> = { ok: 'success', denied: 'warning', error: 'error' }

async function load(append = false) {
  loading.value = true
  try {
    const query: Record<string, string | number> = { limit: 100 }
    if (actor.value) query.actor = actor.value
    if (action.value) query.action = action.value
    if (outcome.value !== 'all') query.outcome = outcome.value
    if (append && events.value.length) query.before = events.value[events.value.length - 1]!.time
    const page = await api<AuditEvent[]>('/audit', { query })
    events.value = append ? [...events.value, ...page] : page
    more.value = page.length === 100
  } catch (e) {
    notify.error('Loading the audit log failed', e)
  } finally {
    loading.value = false
  }
}

onMounted(() => load())
let t: ReturnType<typeof setTimeout> | undefined
watch([actor, action, outcome], () => { clearTimeout(t); t = setTimeout(() => load(), 300) })

const columns: TableColumn<AuditEvent>[] = [
  { accessorKey: 'time', header: 'Time', cell: ({ row }) => h('span', { class: 'whitespace-nowrap' }, formatTime(row.original.time)) },
  { accessorKey: 'actor', header: 'Who', cell: ({ row }) => h('span', {}, [row.original.actor || '—', row.original.via ? h('span', { class: 'text-xs text-(--ui-text-dimmed) ml-1' }, `(${row.original.via})`) : null]) },
  { accessorKey: 'action', header: 'Action', cell: ({ row }) => h('span', { class: 'font-mono text-xs' }, row.original.action) },
  { accessorKey: 'outcome', header: 'Outcome', cell: ({ row }) => h(UBadge, { color: outcomeColor[row.original.outcome] || 'neutral', variant: 'subtle' }, () => row.original.outcome) },
  { accessorKey: 'cluster', header: 'Cluster' },
  { id: 'what', header: 'Details', cell: ({ row }) => h('span', { class: 'text-xs break-words' }, [row.original.target, row.original.detail].filter(Boolean).join(' — ')) },
  { accessorKey: 'ip', header: 'Address', cell: ({ row }) => h('span', { class: 'font-mono text-xs' }, row.original.ip || '') }
]

function exportJSON() {
  const blob = new Blob([events.value.map(e => JSON.stringify(e)).join('\n') + '\n'], { type: 'application/x-ndjson' })
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = `gitopsify-audit-${new Date().toISOString().slice(0, 10)}.jsonl`
  a.click()
  URL.revokeObjectURL(a.href)
}
</script>

<template>
  <UDashboardPanel id="audit">
    <template #header>
      <UDashboardNavbar title="Audit log">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #right>
          <UButton color="neutral" variant="outline" icon="i-lucide-download" label="Export shown" :disabled="!events.length" @click="exportJSON" />
        </template>
      </UDashboardNavbar>
      <UDashboardToolbar>
        <template #left>
          <UInput v-model="actor" icon="i-lucide-user-round" placeholder="Person" class="w-48" />
          <UInput v-model="action" icon="i-lucide-activity" placeholder="Action prefix (run., auth.)" class="w-56" />
        </template>
        <template #right>
          <USelect v-model="outcome" :items="[{ label: 'Any outcome', value: 'all' }, { label: 'ok', value: 'ok' }, { label: 'denied', value: 'denied' }, { label: 'error', value: 'error' }]" class="w-36" />
        </template>
      </UDashboardToolbar>
    </template>
    <template #body>
      <p class="text-sm text-(--ui-text-muted)">Sign-ins, runs, pushes, downloads and refused requests. The server also writes them as JSON lines to its data directory (and its log, with spec.audit.stdout) for your SIEM.</p>
      <UTable :data="events" :columns="columns" :loading="loading" empty="No events." />
      <div class="flex justify-center">
        <UButton v-if="more && events.length" variant="ghost" color="neutral" label="Older events" :loading="loading" @click="load(true)" />
      </div>
    </template>
  </UDashboardPanel>
</template>
