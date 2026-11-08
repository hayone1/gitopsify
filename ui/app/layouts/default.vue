<script setup lang="ts">
import type { DropdownMenuItem, NavigationMenuItem } from '@nuxt/ui'

const me = useMe()
const colorMode = useColorMode()
const notify = useNotify()
const open = ref(false)

const links = computed<NavigationMenuItem[][]>(() => {
  const main: NavigationMenuItem[] = [
    { label: 'Overview', icon: 'i-lucide-layout-dashboard', to: '/', onSelect: () => { open.value = false } },
    { label: 'Runs', icon: 'i-lucide-list-checks', to: '/runs', onSelect: () => { open.value = false } },
    { label: 'Clusters', icon: 'i-lucide-server', to: '/clusters', onSelect: () => { open.value = false } }
  ]
  if (atLeast(me.value?.role, 'operator')) {
    main.splice(1, 0, { label: 'New migration', icon: 'i-lucide-git-branch-plus', to: '/migrations/new', onSelect: () => { open.value = false } })
  }
  const admin: NavigationMenuItem[] = []
  if (atLeast(me.value?.serverRole, 'admin')) {
    admin.push({ label: 'Audit log', icon: 'i-lucide-scroll-text', to: '/audit', onSelect: () => { open.value = false } })
  }
  admin.push({ label: 'Profile', icon: 'i-lucide-user-round', to: '/profile', onSelect: () => { open.value = false } })
  return [main, admin]
})

const userItems = computed<DropdownMenuItem[][]>(() => [
  [{ label: me.value?.user.name || me.value?.user.username || '', type: 'label', avatar: { alt: me.value?.user.name || me.value?.user.username } }],
  [
    { label: 'Profile', icon: 'i-lucide-user-round', to: '/profile' },
    {
      label: colorMode.value === 'dark' ? 'Light mode' : 'Dark mode',
      icon: colorMode.value === 'dark' ? 'i-lucide-sun' : 'i-lucide-moon',
      onSelect: (e: Event) => { e.preventDefault(); colorMode.preference = colorMode.value === 'dark' ? 'light' : 'dark' }
    }
  ],
  [{ label: 'Sign out', icon: 'i-lucide-log-out', onSelect: signOut }]
])

async function signOut() {
  try {
    await api('/auth/logout', { method: 'POST' })
  } catch (e) {
    notify.error('Signing out failed', e)
  }
  me.value = null
  await navigateTo('/login')
}
</script>

<template>
  <UDashboardGroup unit="rem">
    <UDashboardSidebar
      v-model:open="open"
      collapsible
      resizable
      class="bg-(--ui-bg-elevated)/25"
      :ui="{ footer: 'lg:border-t lg:border-(--ui-border)' }"
    >
      <template #header="{ collapsed }">
        <NuxtLink to="/" class="flex items-center gap-2 font-semibold">
          <img src="/favicon.svg" alt="" class="size-7 shrink-0">
          <span v-if="!collapsed" class="text-lg">gitopsify</span>
        </NuxtLink>
      </template>

      <template #default="{ collapsed }">
        <UNavigationMenu :collapsed="collapsed" :items="links[0]" orientation="vertical" tooltip popover />
        <UNavigationMenu :collapsed="collapsed" :items="links[1]" orientation="vertical" tooltip class="mt-auto" />
      </template>

      <template #footer="{ collapsed }">
        <UDropdownMenu :items="userItems" :content="{ align: 'center', collisionPadding: 12 }" :ui="{ content: collapsed ? 'w-48' : 'w-(--reka-dropdown-menu-trigger-width)' }">
          <UButton
            :avatar="{ alt: me?.user.name || me?.user.username }"
            :label="collapsed ? undefined : (me?.user.name || me?.user.username)"
            :trailing-icon="collapsed ? undefined : 'i-lucide-chevrons-up-down'"
            color="neutral"
            variant="ghost"
            block
            :square="collapsed"
            class="data-[state=open]:bg-(--ui-bg-elevated)"
            :ui="{ trailingIcon: 'text-(--ui-text-dimmed)' }"
          />
        </UDropdownMenu>
      </template>
    </UDashboardSidebar>

    <slot />
  </UDashboardGroup>
</template>
