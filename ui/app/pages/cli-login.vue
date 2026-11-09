<script setup lang="ts">
definePageMeta({ layout: 'blank' })

// After single sign-on for `gitopsify login`, the server waits for the
// person to confirm here that they started it: a sign-in link anyone can
// send would otherwise hand a session to someone else's command line.
interface Pending {
  user: { username: string, name?: string }
  checkCode: string
  paste: boolean
}

const pending = ref<Pending | null>(null)
const state = ref<'loading' | 'waiting' | 'none' | 'code' | 'done' | 'canceled'>('loading')
const code = ref('')
const busy = ref(false)
const failed = ref('')
const copied = ref(false)

onMounted(async () => {
  try {
    pending.value = await $fetch<Pending>('/api/v1/auth/cli')
    state.value = 'waiting'
  } catch {
    state.value = 'none'
  }
})

async function answer(confirm: boolean) {
  busy.value = true
  failed.value = ''
  try {
    const res = await $fetch<{ redirect?: string, code?: string }>('/api/v1/auth/cli', { method: 'POST', body: { confirm } })
    if (!confirm) state.value = 'canceled'
    else if (res.redirect) {
      state.value = 'done'
      window.location.href = res.redirect
    } else {
      code.value = res.code || ''
      state.value = 'code'
    }
  } catch {
    failed.value = 'The sign-in expired. Run gitopsify login again.'
  } finally {
    busy.value = false
  }
}

async function copy() {
  try {
    await navigator.clipboard.writeText(code.value)
    copied.value = true
    setTimeout(() => (copied.value = false), 2000)
  } catch {
    copied.value = false
  }
}
</script>

<template>
  <UCard class="w-full max-w-lg">
    <div class="flex flex-col gap-4">
      <div class="flex items-center gap-3">
        <img src="/favicon.svg" alt="" class="size-8">
        <h1 class="text-xl font-semibold">Sign in on the command line</h1>
      </div>

      <USkeleton v-if="state === 'loading'" class="h-24 w-full" />

      <template v-else-if="state === 'waiting' && pending">
        <p class="text-sm">
          Sign in the gitopsify command line as <span class="font-medium">{{ pending.user.name || pending.user.username }}</span>
          <span v-if="pending.user.name" class="text-(--ui-text-muted)"> ({{ pending.user.username }})</span>?
        </p>
        <div class="flex items-center justify-between gap-3 rounded border border-(--ui-border) bg-(--ui-bg-muted) p-3">
          <span class="text-sm text-(--ui-text-muted)">Check code</span>
          <span class="font-mono text-lg font-semibold tracking-wider">{{ pending.checkCode }}</span>
        </div>
        <UAlert
          color="warning"
          variant="subtle"
          icon="i-lucide-shield-alert"
          description="Continue only if you just ran gitopsify login and your terminal shows the same check code. Someone who sent you this link would get your access."
        />
        <UAlert v-if="failed" color="error" variant="subtle" icon="i-lucide-circle-alert" :description="failed" />
        <div class="flex justify-end gap-2">
          <UButton color="neutral" variant="ghost" label="Cancel" :disabled="busy" @click="answer(false)" />
          <UButton icon="i-lucide-terminal" label="Sign in the command line" :loading="busy" @click="answer(true)" />
        </div>
      </template>

      <template v-else-if="state === 'code'">
        <p class="text-sm text-(--ui-text-muted)">
          Copy this and paste it where <span class="font-mono">gitopsify login</span> asks for it. It works for 5 minutes, only for the terminal that asked.
        </p>
        <div class="flex items-start gap-2">
          <code class="flex-1 min-w-0 break-all rounded border border-(--ui-border) bg-(--ui-bg-muted) p-3 font-mono text-xs max-h-40 overflow-auto">{{ code }}</code>
          <UButton :icon="copied ? 'i-lucide-check' : 'i-lucide-copy'" :label="copied ? 'Copied' : 'Copy'" color="neutral" variant="outline" @click="copy" />
        </div>
      </template>

      <p v-else-if="state === 'done'" class="text-sm text-(--ui-text-muted)">Handing the sign-in to your terminal…</p>

      <UAlert
        v-else-if="state === 'canceled'"
        color="neutral"
        variant="subtle"
        icon="i-lucide-circle-x"
        title="Canceled"
        description="The command line wasn't signed in. You can close this tab."
      />

      <UAlert
        v-else
        color="warning"
        variant="subtle"
        icon="i-lucide-circle-alert"
        title="No sign-in waiting"
        description="This page confirms a gitopsify login started in a terminal. Run gitopsify login again and open the address it prints."
      />
    </div>
  </UCard>
</template>
