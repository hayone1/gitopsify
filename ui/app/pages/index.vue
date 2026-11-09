<script setup lang="ts">
import type { Cluster, Run } from '~/utils/types'

const me = useMe()
const notify = useNotify()
const runs = ref<Run[]>([])
const clusters = ref<Cluster[]>([])
const loading = ref(true)

async function load() {
  try {
    const [r, c] = await Promise.all([api<Run[]>('/runs', { query: { limit: 200 } }), api<Cluster[]>('/clusters')])
    runs.value = r
    clusters.value = c
  } catch (e) {
    notify.error('Loading the overview failed', e)
  } finally {
    loading.value = false
  }
}

let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  timer = setInterval(() => { if (runs.value.some(r => r.status === 'running' || r.status === 'queued')) load() }, 5000)
})
onUnmounted(() => clearInterval(timer))

const week = computed(() => runs.value.filter(r => Date.now() - new Date(r.createdAt).getTime() < 7 * 86400e3))
const stats = computed(() => [
  { label: 'Runs this week', value: week.value.length, icon: 'i-lucide-activity' },
  { label: 'Succeeded', value: week.value.filter(r => r.status === 'succeeded').length, icon: 'i-lucide-circle-check', color: 'text-(--ui-success)' },
  { label: 'Failed', value: week.value.filter(r => r.status === 'failed').length, icon: 'i-lucide-circle-x', color: 'text-(--ui-error)' },
  { label: 'Clusters', value: clusters.value.length, icon: 'i-lucide-server' }
])
const recent = computed(() => runs.value.slice(0, 8))
// Migrations need the operator role on a cluster (tool grants don't count).
const canOperate = computed(() => Object.values(me.value?.clusters || {}).some(r => atLeast(r, 'operator')))
</script>

<template>
  <UDashboardPanel id="overview">
    <template #header>
      <UDashboardNavbar title="Overview">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #right>
          <UButton v-if="canOperate" icon="i-lucide-git-branch-plus" label="New migration" to="/migrations/new" />
        </template>
      </UDashboardNavbar>
    </template>

    <template #body>
      <UAlert
        v-if="me && !me.role"
        color="warning"
        variant="subtle"
        icon="i-lucide-shield-alert"
        title="You have no access yet"
        :description="`You're signed in as ${me.user.username}, but no role is bound to you or your groups. Ask an administrator of this gitopsify server to add you to spec.rbac.bindings.`"
      />

      <div class="grid grid-cols-2 lg:grid-cols-4 gap-4">
        <UCard v-for="s in stats" :key="s.label" :ui="{ body: 'flex items-center gap-4' }">
          <UIcon :name="s.icon" :class="['size-8 shrink-0', s.color || 'text-(--ui-primary)']" />
          <div>
            <p class="text-2xl font-semibold">
              <USkeleton v-if="loading" class="h-7 w-10" /><template v-else>{{ s.value }}</template>
            </p>
            <p class="text-sm text-(--ui-text-muted)">{{ s.label }}</p>
          </div>
        </UCard>
      </div>

      <div class="grid grid-cols-1 xl:grid-cols-3 gap-4">
        <UCard class="xl:col-span-2">
          <template #header>
            <div class="flex items-center justify-between">
              <h2 class="font-semibold">Recent runs</h2>
              <UButton variant="link" color="neutral" label="All runs" trailing-icon="i-lucide-arrow-right" to="/runs" />
            </div>
          </template>
          <RunsTable :runs="recent" :loading="loading" compact />
        </UCard>

        <UCard>
          <template #header>
            <div class="flex items-center justify-between">
              <h2 class="font-semibold">Clusters</h2>
              <UButton variant="link" color="neutral" label="Manage" trailing-icon="i-lucide-arrow-right" to="/clusters" />
            </div>
          </template>
          <USkeleton v-if="loading" class="h-24" />
          <p v-else-if="!clusters.length" class="text-sm text-(--ui-text-muted)">No clusters you can use. Clusters are configured in the server's spec.clusters.</p>
          <ul v-else class="divide-y divide-(--ui-border)">
            <li v-for="c in clusters" :key="c.name" class="flex items-center justify-between py-2 gap-2">
              <div class="min-w-0">
                <p class="font-medium truncate">{{ c.name }}</p>
                <p class="text-xs text-(--ui-text-muted) truncate">{{ c.description || ' ' }}</p>
              </div>
              <div class="flex items-center gap-2">
                <UBadge variant="subtle" color="neutral" class="capitalize">{{ c.role }}</UBadge>
                <UButton
                  v-if="atLeast(c.role, 'operator')"
                  size="xs"
                  variant="ghost"
                  icon="i-lucide-git-branch-plus"
                  :to="{ path: '/migrations/new', query: { cluster: c.name } }"
                  aria-label="New migration from this cluster"
                />
              </div>
            </li>
          </ul>
        </UCard>
      </div>
    </template>
  </UDashboardPanel>
</template>
