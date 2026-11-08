<script setup lang="ts">
const me = useMe()
const roles = computed(() => Object.entries(me.value?.clusters || {}).sort(([a], [b]) => a.localeCompare(b)))
const roleText: Record<string, string> = {
  viewer: 'See clusters, runs and their files',
  operator: 'Also scan, generate and push migrations',
  admin: 'Also delete anyone\'s runs; without cluster limits, the audit log'
}
</script>

<template>
  <UDashboardPanel id="profile">
    <template #header>
      <UDashboardNavbar title="Profile">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
      </UDashboardNavbar>
    </template>
    <template #body>
      <div v-if="me" class="grid gap-4 lg:grid-cols-2">
        <UCard>
          <template #header><h2 class="font-semibold">You</h2></template>
          <div class="flex items-center gap-4">
            <UAvatar :alt="me.user.name || me.user.username" size="xl" />
            <div class="min-w-0">
              <p class="font-semibold truncate">{{ me.user.name || me.user.username }}</p>
              <p class="text-sm text-(--ui-text-muted) truncate">{{ me.user.email || me.user.username }}</p>
              <p class="text-xs text-(--ui-text-dimmed)">Signed in with {{ me.user.via === 'oidc' ? 'single sign-on' : me.user.via }}<template v-if="me.expires"> until {{ formatTime(me.expires) }}</template></p>
            </div>
          </div>
          <div v-if="me.user.groups?.length" class="mt-4">
            <p class="text-sm font-medium mb-2">Groups</p>
            <div class="flex flex-wrap gap-1">
              <UBadge v-for="g in me.user.groups" :key="g" variant="outline" color="neutral">{{ g }}</UBadge>
            </div>
          </div>
        </UCard>
        <UCard>
          <template #header><h2 class="font-semibold">Your roles</h2></template>
          <p v-if="!roles.length" class="text-sm text-(--ui-text-muted)">No roles yet. An administrator binds roles to people and groups in spec.rbac.bindings.</p>
          <ul v-else class="divide-y divide-(--ui-border)">
            <li v-for="[cluster, role] in roles" :key="cluster" class="py-2 flex items-center justify-between gap-3">
              <span class="font-medium">{{ cluster }}</span>
              <span class="text-right">
                <UBadge variant="subtle" class="capitalize">{{ role }}</UBadge>
                <span class="block text-xs text-(--ui-text-muted)">{{ roleText[role] }}</span>
              </span>
            </li>
          </ul>
        </UCard>
        <UCard class="lg:col-span-2">
          <template #header><h2 class="font-semibold">Automation</h2></template>
          <p class="text-sm text-(--ui-text-muted)">
            CI jobs call the same API with an API token: an administrator makes one with
            <code>gitopsify server token --name ci --role operator</code> and adds its hash to spec.auth.tokens.
            Send it as <code>Authorization: Bearer &lt;token&gt;</code> to <code>/api/v1</code>.
          </p>
          <p class="text-xs text-(--ui-text-dimmed) mt-2">gitopsify {{ me.version }}</p>
        </UCard>
      </div>
    </template>
  </UDashboardPanel>
</template>
