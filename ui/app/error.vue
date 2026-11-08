<script setup lang="ts">
import type { NuxtError } from '#app'

const props = defineProps<{ error: NuxtError }>()
const title = computed(() => props.error.statusCode === 404 ? 'Page not found' : props.error.statusCode === 403 ? 'Not allowed' : 'Something went wrong')
</script>

<template>
  <UApp>
    <div class="min-h-svh flex items-center justify-center p-6">
      <UCard class="max-w-md w-full">
        <div class="flex flex-col items-center text-center gap-3">
          <UIcon name="i-lucide-triangle-alert" class="size-10 text-(--ui-warning)" />
          <h1 class="text-xl font-semibold">{{ title }}</h1>
          <p class="text-(--ui-text-muted)">{{ error.statusMessage || error.message }}</p>
          <UButton icon="i-lucide-house" label="Back to the overview" @click="clearError({ redirect: '/' })" />
        </div>
      </UCard>
    </div>
  </UApp>
</template>
