<template>
  <div class="min-h-svh flex flex-col bg-(--ui-bg-muted) p-4">
    <div class="flex justify-end">
      <UColorModeSelect color="neutral" variant="ghost" size="sm" class="w-32" aria-label="Color mode" />
    </div>
    <div class="flex-1 flex items-center justify-center py-4">
      <slot />
    </div>
  </div>
</template>
