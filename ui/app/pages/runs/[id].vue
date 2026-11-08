<script setup lang="ts">
import { stringify } from 'yaml'
import type { TabsItem } from '@nuxt/ui'
import type { LogLine, Run, ScanResult } from '~/utils/types'

const route = useRoute()
const me = useMe()
const notify = useNotify()
const id = computed(() => route.params.id as string)

const run = ref<Run | null>(null)
const lines = ref<LogLine[]>([])
const scan = ref<ScanResult | null>(null)
const live = ref(false)
const notFound = ref(false)
const pushing = ref(false)
const pushOpen = ref(false)
const pushMessage = ref('')
const tab = ref('log')
let source: EventSource | null = null

const role = computed(() => {
  if (!run.value) return ''
  return (run.value.tool ? me.value?.tools?.[run.value.tool] : me.value?.clusters[run.value.cluster || '']) || ''
})
const canOperate = computed(() => atLeast(role.value, 'operator'))
const canDelete = computed(() => run.value && (run.value.createdBy === me.value?.user.username || atLeast(role.value, 'admin')))
const finished = computed(() => run.value && ['succeeded', 'failed', 'canceled'].includes(run.value.status))
const spec = computed(() => run.value?.spec as Record<string, any> | undefined)
const specYAML = computed(() => spec.value ? stringify(spec.value) : '')
const repoURL = computed(() => {
  if (run.value?.type === 'export') return run.value.repo?.url
  const sp = spec.value?.spec
  return (sp?.output?.repo?.url as string | undefined) || (sp?.environments || []).map((e: any) => e.repoURL).filter(Boolean).join(', ') || undefined
})
const produces = computed(() => run.value && run.value.type !== 'scan')

async function loadScan() {
  try {
    scan.value = await api<ScanResult>(`/runs/${id.value}/scan`)
  } catch {
    scan.value = null
  }
}

function connect() {
  source?.close()
  lines.value = []
  live.value = true
  source = new EventSource(`/api/v1/runs/${id.value}/events`)
  source.addEventListener('log', (e) => { lines.value.push(JSON.parse((e as MessageEvent).data)) })
  source.addEventListener('run', (e) => {
    const r = JSON.parse((e as MessageEvent).data) as Run
    const wasFinished = finished.value
    run.value = r
    if (!wasFinished && ['succeeded', 'failed', 'canceled'].includes(r.status)) loadScan()
  })
  source.addEventListener('end', () => { live.value = false; source?.close() })
  source.onerror = () => {
    // The browser reconnects by itself and the server carries on after the
    // last line it sent; give up only when it closed for good.
    if (source?.readyState === EventSource.CLOSED) {
      live.value = false
      if (!run.value) notFound.value = true
    }
  }
}

onMounted(async () => {
  try {
    run.value = await api<Run>(`/runs/${id.value}`)
  } catch (e) {
    notFound.value = true
    notify.error('Loading the run failed', e)
    return
  }
  connect()
  if (finished.value) loadScan()
})
onUnmounted(() => source?.close())

const tabs = computed<TabsItem[]>(() => {
  const t: TabsItem[] = [{ label: 'Log', value: 'log', icon: 'i-lucide-terminal' }]
  if (scan.value) t.push({ label: `Apps (${scan.value.apps.length})`, value: 'apps', icon: 'i-lucide-boxes' })
  if (produces.value && run.value?.status === 'succeeded') t.push({ label: 'Files', value: 'files', icon: 'i-lucide-folder-tree' })
  t.push({ label: 'Configuration', value: 'config', icon: 'i-lucide-file-cog' })
  if (run.value?.publishes?.length) t.push({ label: `Pushes (${run.value.publishes.length})`, value: 'pushes', icon: 'i-lucide-git-commit-horizontal' })
  return t
})

async function cancel() {
  try {
    await api(`/runs/${id.value}/cancel`, { method: 'POST' })
    notify.ok('Canceling the run')
  } catch (e) {
    notify.error('Canceling failed', e)
  }
}

async function push() {
  pushing.value = true
  try {
    run.value = await api<Run>(`/runs/${id.value}/publish`, { method: 'POST', body: pushMessage.value ? { message: pushMessage.value } : {} })
    pushOpen.value = false
    notify.ok('Pushed', repoURL.value)
    tab.value = 'pushes'
  } catch (e) {
    notify.error('Pushing failed', e)
  } finally {
    pushing.value = false
  }
}

async function remove() {
  if (!confirm('Delete this run and its files?')) return
  try {
    await api(`/runs/${id.value}`, { method: 'DELETE' })
    notify.ok('Run deleted')
    await navigateTo('/runs')
  } catch (e) {
    notify.error('Deleting failed', e)
  }
}

const summaryCards = computed(() => {
  const s = run.value?.summary
  if (!s || !run.value) return []
  if (run.value.type === 'export') {
    return [
      { label: 'Workspaces', value: s.apps },
      { label: 'Objects', value: s.objects },
      { label: 'Skipped', value: s.excluded },
      { label: 'Warnings', value: s.warnings },
      { label: 'Files written', value: s.files }
    ]
  }
  return [
    { label: 'Apps', value: s.apps },
    { label: 'Objects', value: s.objects },
    { label: 'Left out', value: s.excluded },
    { label: 'Warnings', value: s.warnings },
    { label: run.value.type === 'migrate' ? 'Files written' : 'Secrets to handle', value: run.value.type === 'migrate' ? s.files : s.secretActions }
  ]
})

const cli = computed(() => {
  if (!run.value) return ''
  if (run.value.type === 'export') {
    const p = (spec.value?.spec?.provider?.name as string) || 'provider'
    return `# gitopsify-${p}.yaml holds the export above (it's also in the output);\n# set the provider's environment variables for the connection, then:\ngitopsify export --config gitopsify-${p}.yaml --no-input`
  }
  const cmd = run.value.type === 'scan' ? 'gitopsify scan' : 'gitopsify generate'
  return `# gitopsify.yaml holds the configuration above\n${cmd} --config gitopsify.yaml --context ${run.value.cluster}`
})

const appColumns = [
  { accessorKey: 'wave', header: 'Wave' },
  { accessorKey: 'name', header: 'App' },
  { accessorKey: 'namespace', header: 'Namespace' },
  { accessorKey: 'objects', header: 'Objects' },
  { id: 'what', header: 'What', cell: ({ row }: { row: { original: ScanResult['apps'][number] } }) => row.original.helm ? `Helm ${row.original.helm.chart} ${row.original.helm.version} (${row.original.helm.mode})` : row.original.kinds.join(', ') }
]

async function copy(text: string) {
  await navigator.clipboard.writeText(text)
  notify.ok('Copied')
}
</script>

<template>
  <UDashboardPanel id="run">
    <template #header>
      <UDashboardNavbar :title="run ? (run.name || run.id) : 'Run'">
        <template #leading>
          <UDashboardSidebarCollapse />
          <UButton icon="i-lucide-arrow-left" color="neutral" variant="ghost" to="/runs" aria-label="All runs" />
        </template>
        <template #right>
          <template v-if="run">
            <UButton v-if="!finished && canOperate" icon="i-lucide-square" color="warning" variant="soft" label="Cancel" @click="cancel" />
            <UButton
              v-if="run.type === 'scan' && run.status === 'succeeded' && canOperate"
              icon="i-lucide-git-branch-plus"
              label="Generate from this scan"
              :to="{ path: '/migrations/new', query: { from: run.id } }"
            />
            <template v-if="produces && run.status === 'succeeded'">
              <UButton icon="i-lucide-download" color="neutral" variant="outline" label="Download" :href="`/api/v1/runs/${run.id}/archive`" external />
              <UButton v-if="canOperate && repoURL" icon="i-lucide-upload" label="Push to Git" @click="pushOpen = true" />
            </template>
            <UButton v-if="finished && canDelete" icon="i-lucide-trash-2" color="error" variant="ghost" aria-label="Delete" @click="remove" />
          </template>
        </template>
      </UDashboardNavbar>
    </template>

    <template #body>
      <UAlert v-if="notFound" color="error" variant="subtle" icon="i-lucide-search-x" title="No such run" description="It was deleted, expired, or belongs to a cluster you can't see." />
      <template v-else-if="run">
        <div class="flex flex-wrap items-center gap-x-6 gap-y-2 text-sm">
          <RunStatus :status="run.status" />
          <span class="flex items-center gap-1.5 capitalize"><UIcon name="i-lucide-tag" class="text-(--ui-text-dimmed)" />{{ runTypeLabel(run.type) }}</span>
          <span class="flex items-center gap-1.5"><UIcon :name="run.tool ? 'i-lucide-blocks' : 'i-lucide-server'" class="text-(--ui-text-dimmed)" />{{ runTarget(run) }}</span>
          <span class="flex items-center gap-1.5"><UIcon name="i-lucide-user-round" class="text-(--ui-text-dimmed)" />{{ run.createdBy }}</span>
          <span class="flex items-center gap-1.5" :title="formatTime(run.createdAt)"><UIcon name="i-lucide-clock" class="text-(--ui-text-dimmed)" />{{ ago(run.createdAt) }}<template v-if="run.startedAt"> · {{ duration(run.startedAt, run.finishedAt) }}</template></span>
          <NuxtLink v-if="run.from" :to="`/runs/${run.from}`" class="flex items-center gap-1.5 hover:underline"><UIcon name="i-lucide-scan-search" class="text-(--ui-text-dimmed)" />from scan {{ run.from }}</NuxtLink>
          <span class="font-mono text-xs text-(--ui-text-dimmed)">{{ run.id }}</span>
        </div>

        <UAlert v-if="run.error" color="error" variant="subtle" icon="i-lucide-circle-x" :title="run.status === 'canceled' ? 'Canceled' : 'The run failed'" :description="run.error" />

        <div v-if="run.summary" class="grid grid-cols-2 md:grid-cols-5 gap-3">
          <UCard v-for="s in summaryCards" :key="s.label" :ui="{ body: 'py-3 sm:py-3' }">
            <p class="text-xl font-semibold">{{ s.value ?? 0 }}</p>
            <p class="text-xs text-(--ui-text-muted)">{{ s.label }}</p>
          </UCard>
        </div>

        <UTabs v-model="tab" :items="tabs" variant="link" class="w-full" :content="false" />

        <LogView v-if="tab === 'log'" :lines="lines" :live="live" />

        <template v-else-if="tab === 'apps' && scan">
          <UAlert v-if="scan.warnings.length" color="warning" variant="subtle" icon="i-lucide-triangle-alert" :title="`${scan.warnings.length} warnings`">
            <template #description>
              <ul class="list-disc pl-4 space-y-1 max-h-48 overflow-auto">
                <li v-for="(w, i) in scan.warnings" :key="i">{{ w }}</li>
              </ul>
            </template>
          </UAlert>
          <UTable :data="[...scan.apps].sort((a, b) => a.wave - b.wave || a.name.localeCompare(b.name))" :columns="appColumns" />
          <UCollapsible v-if="scan.excluded?.length" class="mt-2">
            <UButton color="neutral" variant="ghost" trailing-icon="i-lucide-chevron-down" :label="`${scan.excluded.length} objects left out, and why`" />
            <template #content>
              <ul class="text-xs font-mono space-y-1 max-h-80 overflow-auto p-2">
                <li v-for="x in scan.excluded" :key="x.object"><span class="text-(--ui-text-highlighted)">{{ x.object }}</span> — {{ x.reason }}</li>
              </ul>
            </template>
          </UCollapsible>
        </template>

        <FileBrowser v-else-if="tab === 'files'" :run-id="run.id" />

        <div v-else-if="tab === 'config'" class="space-y-4">
          <UCard>
            <template #header>
              <div class="flex items-center justify-between">
                <h3 class="font-semibold">{{ run.type === 'export' ? 'Export' : 'gitopsify.yaml' }}</h3>
                <UButton size="xs" variant="ghost" icon="i-lucide-copy" label="Copy" @click="copy(specYAML)" />
              </div>
            </template>
            <pre class="text-xs overflow-auto max-h-[50vh]">{{ specYAML }}</pre>
          </UCard>
          <UCard>
            <template #header>
              <div class="flex items-center justify-between">
                <h3 class="font-semibold">The same from the command line or CI</h3>
                <UButton size="xs" variant="ghost" icon="i-lucide-copy" label="Copy" @click="copy(cli)" />
              </div>
            </template>
            <pre class="text-xs overflow-auto">{{ cli }}</pre>
          </UCard>
        </div>

        <UCard v-else-if="tab === 'pushes'">
          <ul class="divide-y divide-(--ui-border)">
            <li v-for="(p, i) in run.publishes" :key="i" class="py-3 flex items-start gap-3">
              <UIcon :name="p.error ? 'i-lucide-circle-x' : 'i-lucide-git-commit-horizontal'" :class="['size-5 mt-0.5', p.error ? 'text-(--ui-error)' : 'text-(--ui-success)']" />
              <div class="text-sm min-w-0">
                <p><span class="font-mono">{{ p.commit || '—' }}</span> to <span class="font-medium break-all">{{ p.remote }}</span> ({{ p.branch }})</p>
                <p class="text-(--ui-text-muted) text-xs">{{ p.by }} · {{ formatTime(p.at) }}<template v-if="p.dir"> · {{ p.dir }}</template></p>
                <p v-if="p.error" class="text-(--ui-error) text-xs break-words">{{ p.error }}</p>
              </div>
            </li>
          </ul>
        </UCard>
      </template>
      <USkeleton v-else class="h-64" />

      <UModal v-model:open="pushOpen" title="Push to Git" :description="repoURL">
        <template #body>
          <UFormField label="Commit message" hint="optional">
            <UInput v-model="pushMessage" class="w-full" placeholder="gitopsify: migration from the cluster" />
          </UFormField>
        </template>
        <template #footer>
          <div class="flex justify-end gap-2 w-full">
            <UButton color="neutral" variant="ghost" label="Cancel" @click="pushOpen = false" />
            <UButton icon="i-lucide-upload" label="Commit and push" :loading="pushing" @click="push" />
          </div>
        </template>
      </UModal>
    </template>
  </UDashboardPanel>
</template>
