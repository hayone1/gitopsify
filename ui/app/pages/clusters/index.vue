<script setup lang="ts">
import type { Cluster, ClusterCheck } from '~/utils/types'

const notify = useNotify()
const clusters = ref<Cluster[]>([])
const loading = ref(true)
const checks = ref<Record<string, ClusterCheck | 'checking'>>({})

onMounted(async () => {
  try {
    clusters.value = await api<Cluster[]>('/clusters')
  } catch (e) {
    notify.error('Loading clusters failed', e)
  } finally {
    loading.value = false
  }
  for (const c of clusters.value.slice(0, 20)) check(c.name)
})

async function check(name: string) {
  checks.value[name] = 'checking'
  try {
    checks.value[name] = await api<ClusterCheck>(`/clusters/${encodeURIComponent(name)}/check`)
  } catch (e) {
    checks.value[name] = { ok: false, error: e instanceof Error ? e.message : String(e) }
  }
}
</script>

<template>
  <UDashboardPanel id="clusters">
    <template #header>
      <UDashboardNavbar title="Clusters">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
      </UDashboardNavbar>
    </template>
    <template #body>
      <p class="text-sm text-(--ui-text-muted)">
        The clusters you can use, and your role on each. Administrators add clusters in the server's configuration (spec.clusters);
        with impersonation on, gitopsify reads them as you, so your own Kubernetes permissions apply.
      </p>
      <USkeleton v-if="loading" class="h-40" />
      <UAlert v-else-if="!clusters.length" color="neutral" variant="subtle" icon="i-lucide-server-off" title="No clusters" description="No cluster is configured, or none is bound to your role." />
      <div v-else class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        <UCard v-for="c in clusters" :key="c.name">
          <div class="flex items-start justify-between gap-2">
            <div class="min-w-0">
              <h3 class="font-semibold truncate">{{ c.name }}</h3>
              <p class="text-sm text-(--ui-text-muted)">{{ c.description || ' ' }}</p>
            </div>
            <UBadge variant="subtle" color="neutral" class="capitalize shrink-0">{{ c.role }}</UBadge>
          </div>
          <div class="mt-4 text-sm min-h-12">
            <div v-if="checks[c.name] === 'checking'" class="flex items-center gap-2 text-(--ui-text-muted)">
              <UIcon name="i-lucide-loader-circle" class="animate-spin" /> Connecting…
            </div>
            <template v-else-if="checks[c.name]">
              <div v-if="(checks[c.name] as ClusterCheck).ok" class="space-y-1">
                <p class="flex items-center gap-2 text-(--ui-success)"><UIcon name="i-lucide-plug-zap" /> Reachable, Kubernetes {{ (checks[c.name] as ClusterCheck).version }}</p>
                <p v-if="(checks[c.name] as ClusterCheck).identity" class="text-xs text-(--ui-text-muted) truncate">as {{ (checks[c.name] as ClusterCheck).identity }}</p>
              </div>
              <p v-else class="flex items-start gap-2 text-(--ui-error) text-xs break-words"><UIcon name="i-lucide-unplug" class="shrink-0 mt-0.5" />{{ (checks[c.name] as ClusterCheck).error }}</p>
            </template>
          </div>
          <template #footer>
            <div class="flex items-center justify-between">
              <UBadge v-if="c.impersonate" variant="outline" color="info" icon="i-lucide-user-check">impersonates you</UBadge>
              <span v-else />
              <div class="flex gap-2">
                <UButton size="xs" color="neutral" variant="ghost" icon="i-lucide-refresh-cw" aria-label="Check again" @click="check(c.name)" />
                <UButton size="xs" variant="ghost" icon="i-lucide-list-checks" label="Runs" :to="{ path: '/runs', query: { cluster: c.name } }" />
                <UButton v-if="atLeast(c.role, 'operator')" size="xs" icon="i-lucide-git-branch-plus" label="Migrate" :to="{ path: '/migrations/new', query: { cluster: c.name } }" />
              </div>
            </div>
          </template>
        </UCard>
      </div>
    </template>
  </UDashboardPanel>
</template>
