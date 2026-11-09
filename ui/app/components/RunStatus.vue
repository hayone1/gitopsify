<script setup lang="ts">
import type { RunStatus } from '~/utils/types'

defineProps<{ status: RunStatus }>()
</script>

<template>
  <UBadge :color="statusColor(status)" variant="subtle" class="capitalize gap-1">
    <UIcon :name="statusIcon(status)" :class="['size-3.5', status === 'running' && 'animate-spin']" />
    {{ status }}
  </UBadge>
</template>
