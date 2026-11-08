<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'
import type { ProviderInfo, Tool, ToolCheck } from '~/utils/types'

const me = useMe()
const notify = useNotify()
const tools = ref<Tool[]>([])
const providers = ref<ProviderInfo[]>([])
const loading = ref(true)
const checks = ref<Record<string, ToolCheck | 'checking'>>({})
const installing = ref('')
const UBadge = resolveComponent('UBadge')
const UButton = resolveComponent('UButton')

const isAdmin = computed(() => atLeast(me.value?.serverRole, 'admin'))

async function load() {
  try {
    const [t, p] = await Promise.all([api<Tool[]>('/tools'), api<ProviderInfo[]>('/providers')])
    tools.value = t
    providers.value = p
  } catch (e) {
    notify.error('Loading tools failed', e)
  } finally {
    loading.value = false
  }
}
onMounted(load)

async function check(name: string) {
  checks.value[name] = 'checking'
  try {
    checks.value[name] = await api<ToolCheck>(`/tools/${encodeURIComponent(name)}/check`)
  } catch (e) {
    checks.value[name] = { error: e instanceof Error ? e.message : String(e) }
  }
}

async function install(p: ProviderInfo) {
  installing.value = p.name
  try {
    await api('/providers', { method: 'POST', body: { source: p.name } })
    notify.ok(`Installed ${p.name}`)
    await load()
  } catch (e) {
    notify.error(`Installing ${p.name} failed`, e)
  } finally {
    installing.value = ''
  }
}

const columns = computed<TableColumn<ProviderInfo>[]>(() => [
  {
    accessorKey: 'name',
    header: 'Provider',
    cell: ({ row }) => h('div', {}, [
      h('p', { class: 'font-medium' }, row.original.title || row.original.name),
      h('p', { class: 'text-xs text-(--ui-text-muted) font-mono' }, row.original.source || row.original.name)
    ])
  },
  { accessorKey: 'description', header: 'Reads', cell: ({ row }) => h('span', { class: 'text-sm whitespace-normal' }, row.original.description || '') },
  {
    id: 'outputs',
    header: 'Writes',
    cell: ({ row }) => h('div', { class: 'flex gap-1' }, (row.original.outputs || []).map(o => h(UBadge, { variant: 'outline', color: 'neutral' }, () => o)))
  },
  { accessorKey: 'maturity', header: 'Maturity' },
  {
    id: 'installed',
    header: 'Installed',
    cell: ({ row }) => row.original.installed
      ? h(UBadge, { color: 'success', variant: 'subtle' }, () => row.original.installed)
      : (isAdmin.value
          ? h(UButton, { size: 'xs', icon: 'i-lucide-download', label: 'Install', loading: installing.value === row.original.name, onClick: () => install(row.original) })
          : h('span', { class: 'text-xs text-(--ui-text-muted)' }, 'not installed'))
  }
])
</script>

<template>
  <UDashboardPanel id="tools">
    <template #header>
      <UDashboardNavbar title="Tools">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
      </UDashboardNavbar>
    </template>
    <template #body>
      <p class="text-sm text-(--ui-text-muted)">
        Tools whose objects (dashboards, realms, policies...) gitopsify exports as Terraform/OpenTofu or Crossplane through providers.
        Administrators register tools and their connections in spec.tools; credentials stay on the server.
      </p>
      <USkeleton v-if="loading" class="h-32" />
      <UAlert v-else-if="!tools.length" color="neutral" variant="subtle" icon="i-lucide-blocks" title="No tools"
        description="No tool is registered, or none is bound to your role. An administrator adds them to spec.tools." />
      <div v-else class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        <UCard v-for="t in tools" :key="t.name">
          <div class="flex items-start justify-between gap-2">
            <div class="min-w-0">
              <h3 class="font-semibold truncate">{{ t.name }}</h3>
              <p class="text-sm text-(--ui-text-muted)">{{ t.title || t.provider }}<template v-if="t.description"> · {{ t.description }}</template></p>
            </div>
            <UBadge variant="subtle" color="neutral" class="capitalize shrink-0">{{ t.role }}</UBadge>
          </div>
          <div class="mt-3 text-sm min-h-10">
            <p v-if="!t.installed" class="text-(--ui-warning) text-xs">The {{ t.provider }} provider isn't installed{{ isAdmin ? '; install it below.' : '.' }}</p>
            <div v-else-if="checks[t.name] === 'checking'" class="flex items-center gap-2 text-(--ui-text-muted)"><UIcon name="i-lucide-loader-circle" class="animate-spin" /> Connecting…</div>
            <template v-else-if="checks[t.name]">
              <div v-if="(checks[t.name] as ToolCheck).check" class="space-y-1">
                <p class="flex items-center gap-2 text-(--ui-success)"><UIcon name="i-lucide-plug-zap" />{{ (checks[t.name] as ToolCheck).check!.target }}</p>
                <p class="text-xs text-(--ui-text-muted)">
                  {{ (checks[t.name] as ToolCheck).check!.partitions.length }} {{ (checks[t.name] as ToolCheck).description?.partition.plural }}
                  <template v-if="(checks[t.name] as ToolCheck).check!.identity"> · as {{ (checks[t.name] as ToolCheck).check!.identity }}</template>
                </p>
              </div>
              <p v-else class="text-(--ui-error) text-xs break-words">{{ (checks[t.name] as ToolCheck).error }}</p>
            </template>
          </div>
          <template #footer>
            <div class="flex justify-end gap-2">
              <UButton size="xs" color="neutral" variant="ghost" icon="i-lucide-plug" label="Check" :disabled="!t.installed" @click="check(t.name)" />
              <UButton size="xs" variant="ghost" icon="i-lucide-list-checks" label="Runs" :to="{ path: '/runs', query: { type: 'export' } }" />
              <UButton v-if="atLeast(t.role, 'operator')" size="xs" icon="i-lucide-file-output" label="Export" :disabled="!t.installed" :to="{ path: '/exports/new', query: { tool: t.name } }" />
            </div>
          </template>
        </UCard>
      </div>

      <UCard>
        <template #header>
          <div>
            <h2 class="font-semibold">Providers</h2>
            <p class="text-sm text-(--ui-text-muted)">Installed on this server, and listed in the provider index.</p>
          </div>
        </template>
        <UTable :data="providers" :columns="columns" :loading="loading" empty="No providers." />
      </UCard>
    </template>
  </UDashboardPanel>
</template>
