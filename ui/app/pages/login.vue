<script setup lang="ts">
import type { Info } from '~/utils/types'

definePageMeta({ layout: 'blank' })

const route = useRoute()
const info = ref<Info | null>(null)
// Fixed messages by code: the page never shows text from its URL.
const messages: Record<string, string> = {
  expired: 'The sign-in took too long or your browser blocks cookies. Sign in again.',
  refused: 'Your identity provider refused the sign-in.',
  token: 'The identity provider\'s answer couldn\'t be verified. Sign in again; if it keeps happening, tell your administrator.',
  domain: 'Your account isn\'t in a domain this server admits, or its email isn\'t verified.',
  claims: 'Your account can\'t be used here: it has no usable name. Ask your administrator.',
  link: 'That sign-in link was already used or is wrong. Stop gitopsify ui and start it again for a new one.',
  failed: 'The sign-in failed. Try again, or ask your administrator.'
}
const error = computed(() => {
  const code = route.query.error as string | undefined
  return code ? (messages[code] || messages.failed) : ''
})
const redirect = computed(() => {
  const r = (route.query.redirect as string) || '/'
  // eslint-disable-next-line no-control-regex
  return r.startsWith('/') && !r.startsWith('//') && !/[\\\x00-\x1f\x7f]/.test(r) ? r : '/'
})

onMounted(async () => {
  if (await loadMe()) return navigateTo(redirect.value)
  try {
    info.value = await $fetch<Info>('/api/v1/info')
  } catch {
    info.value = null
  }
})

function signIn() {
  window.location.href = '/api/v1/auth/login?redirect=' + encodeURIComponent(redirect.value)
}
</script>

<template>
  <UCard class="w-full max-w-sm">
    <div class="flex flex-col items-center gap-4 text-center">
      <img src="/favicon.svg" alt="" class="size-12">
      <div>
        <h1 class="text-2xl font-semibold">gitopsify</h1>
        <p class="text-sm text-(--ui-text-muted)">Running clusters and tools, into Git.</p>
      </div>

      <UAlert v-if="error" color="error" variant="subtle" icon="i-lucide-circle-alert" :description="error" class="text-left" />

      <template v-if="info?.authMode === 'oidc'">
        <UButton block size="lg" icon="i-lucide-log-in" label="Sign in with single sign-on" @click="signIn" />
        <p class="text-xs text-(--ui-text-dimmed)">You'll be sent to your organization's identity provider.</p>
      </template>
      <template v-else-if="info?.authMode === 'local'">
        <UAlert
          color="neutral"
          variant="subtle"
          icon="i-lucide-terminal"
          title="Open the link gitopsify ui printed"
          description="This UI runs on your machine for you alone. The address gitopsify ui printed in your terminal signs you in."
          class="text-left"
        />
      </template>
      <USkeleton v-else class="h-10 w-full" />

      <p v-if="info" class="text-xs text-(--ui-text-dimmed)">Version {{ info.version }}</p>
    </div>
  </UCard>
</template>
