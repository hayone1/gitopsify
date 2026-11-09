<script setup lang="ts">
import type { LogLine } from '~/utils/types'

const props = defineProps<{ lines: LogLine[], live?: boolean }>()
const box = ref<HTMLElement | null>(null)
const follow = ref(true)

watch(() => props.lines.length, async () => {
  if (!follow.value) return
  await nextTick()
  if (box.value) box.value.scrollTop = box.value.scrollHeight
})

function onScroll() {
  const el = box.value
  if (el) follow.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}

const levelClass: Record<string, string> = {
  info: 'text-(--ui-text-muted)',
  warn: 'text-(--ui-warning)',
  error: 'text-(--ui-error)'
}
</script>

<template>
  <div ref="box" class="font-mono text-xs leading-relaxed bg-(--ui-bg-muted) p-3 overflow-auto max-h-[60vh] min-h-48" @scroll="onScroll">
    <div v-for="(l, i) in lines" :key="i" class="flex gap-3 whitespace-pre-wrap break-words">
      <span class="text-(--ui-text-dimmed) shrink-0 select-none">{{ new Date(l.time).toLocaleTimeString() }}</span>
      <span :class="levelClass[l.level]">{{ l.message }}</span>
    </div>
    <div v-if="live" class="flex items-center gap-2 text-(--ui-text-dimmed) mt-1">
      <UIcon name="i-lucide-loader-circle" class="animate-spin size-3.5" /> working…
    </div>
    <div v-if="!lines.length && !live" class="text-(--ui-text-dimmed)">No log.</div>
  </div>
</template>
