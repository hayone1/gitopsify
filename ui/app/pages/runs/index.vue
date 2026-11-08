<script setup lang="ts">
import type { Cluster, Run } from '~/utils/types'

const route = useRoute()
const router = useRouter()
const notify = useNotify()
const runs = ref<Run[]>([])
const clusters = ref<Cluster[]>([])
const loading = ref(true)
const loadingMore = ref(false)
const more = ref(false)
const pageSize = 200

const type = ref((route.query.type as string) || 'all')
const status = ref((route.query.status as string) || 'all')
const cluster = ref((route.query.cluster as string) || 'all')
const search = ref('')

const typeItems = [{ label: 'All types', value: 'all' }, { label: 'Migrations', value: 'migrate' }, { label: 'Scans', value: 'scan' }, { label: 'Exports', value: 'export' }]
const statusItems = ['all', 'queued', 'running', 'succeeded', 'failed', 'canceled'].map(s => ({ label: s === 'all' ? 'Any status' : s, value: s }))
const clusterItems = computed(() => [{ label: 'All clusters', value: 'all' }, ...clusters.value.map(c => ({ label: c.name, value: c.name }))])

function filters(): Record<string, string | number> {
  const query: Record<string, string | number> = { limit: pageSize }
  if (type.value !== 'all') query.type = type.value
  if (status.value !== 'all') query.status = status.value
  if (cluster.value !== 'all') query.cluster = cluster.value
  return query
}

// load refreshes the newest page, keeping older pages already loaded.
async function load(reset = false) {
  try {
    const page = await api<Run[]>('/runs', { query: filters() })
    const oldest = page.at(-1)?.createdAt
    const ids = new Set(page.map(r => r.id))
    const older = reset || !oldest ? [] : runs.value.filter(r => !ids.has(r.id) && Date.parse(r.createdAt) <= Date.parse(oldest))
    runs.value = [...page, ...older]
    if (reset || !older.length) more.value = page.length === pageSize
  } catch (e) {
    notify.error('Loading runs failed', e)
  } finally {
    loading.value = false
  }
}

async function loadMore() {
  const last = runs.value.at(-1)
  if (!last) return
  loadingMore.value = true
  try {
    const page = await api<Run[]>('/runs', { query: { ...filters(), before: last.createdAt, beforeID: last.id } })
    const ids = new Set(runs.value.map(r => r.id))
    runs.value = [...runs.value, ...page.filter(r => !ids.has(r.id))]
    more.value = page.length === pageSize
  } catch (e) {
    notify.error('Loading older runs failed', e)
  } finally {
    loadingMore.value = false
  }
}

watch([type, status, cluster], () => {
  router.replace({ query: { ...(type.value !== 'all' && { type: type.value }), ...(status.value !== 'all' && { status: status.value }), ...(cluster.value !== 'all' && { cluster: cluster.value }) } })
  loading.value = true
  load(true)
})

let timer: ReturnType<typeof setInterval> | undefined
onMounted(async () => {
  load(true)
  clusters.value = await api<Cluster[]>('/clusters').catch(() => [])
  timer = setInterval(() => { if (runs.value.some(r => r.status === 'running' || r.status === 'queued')) load() }, 4000)
})
onUnmounted(() => clearInterval(timer))

const shown = computed(() => {
  const q = search.value.toLowerCase()
  return q ? runs.value.filter(r => [r.name, r.id, r.createdBy, r.cluster, r.tool].some(v => v?.toLowerCase().includes(q))) : runs.value
})
</script>

<template>
  <UDashboardPanel id="runs">
    <template #header>
      <UDashboardNavbar title="Runs">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #right>
          <UButton icon="i-lucide-refresh-cw" color="neutral" variant="ghost" aria-label="Refresh" @click="load(true)" />
        </template>
      </UDashboardNavbar>
      <UDashboardToolbar>
        <template #left>
          <UInput v-model="search" icon="i-lucide-search" placeholder="Search name, ID, person" class="w-64" />
        </template>
        <template #right>
          <USelect v-model="type" :items="typeItems" class="w-36" />
          <USelect v-model="status" :items="statusItems" class="w-36" />
          <USelect v-model="cluster" :items="clusterItems" class="w-40" />
        </template>
      </UDashboardToolbar>
    </template>
    <template #body>
      <RunsTable :runs="shown" :loading="loading" />
      <div v-if="more && !loading" class="flex justify-center">
        <UButton color="neutral" variant="outline" icon="i-lucide-history" label="Older runs" :loading="loadingMore" @click="loadMore" />
      </div>
    </template>
  </UDashboardPanel>
</template>
