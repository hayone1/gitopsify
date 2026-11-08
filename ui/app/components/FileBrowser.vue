<script setup lang="ts">
import type { TreeItem } from '@nuxt/ui'
import type { FileEntry } from '~/utils/types'

const props = defineProps<{ runId: string }>()
const notify = useNotify()

const files = ref<FileEntry[]>([])
const loading = ref(true)
const selected = ref<string>('')
const content = ref('')
const contentLoading = ref(false)
const filter = ref('')
const rendered = ref(true)

onMounted(async () => {
  try {
    files.value = await api<FileEntry[]>(`/runs/${props.runId}/files`)
    const first = files.value.find(f => f.path.endsWith('MIGRATION_REPORT.md') || f.path.endsWith('EXPORT_REPORT.md')) || files.value[0]
    if (first) await open(first.path)
  } catch (e) {
    notify.error('Loading the files failed', e)
  } finally {
    loading.value = false
  }
})

async function open(p: string) {
  selected.value = p
  contentLoading.value = true
  try {
    content.value = await $fetch<string>(`/api/v1/runs/${props.runId}/files/${p.split('/').map(encodeURIComponent).join('/')}`, { responseType: 'text' })
  } catch (e) {
    content.value = ''
    notify.error('Opening the file failed', e)
  } finally {
    contentLoading.value = false
  }
}

function icon(p: string) {
  if (p.endsWith('.md')) return 'i-lucide-file-text'
  if (p.endsWith('.yaml') || p.endsWith('.yml')) return 'i-lucide-file-code'
  if (p.endsWith('.sh')) return 'i-lucide-file-terminal'
  if (p.endsWith('.tf') || p.endsWith('.json')) return 'i-lucide-file-code-2'
  return 'i-lucide-file'
}

// tree turns paths into nested items; directories start open near the top.
const tree = computed<TreeItem[]>(() => {
  const q = filter.value.toLowerCase()
  const root: TreeItem[] = []
  const dirs = new Map<string, TreeItem>()
  for (const f of files.value) {
    if (q && !f.path.toLowerCase().includes(q)) continue
    const parts = f.path.split('/')
    let level = root
    let prefix = ''
    parts.forEach((part, i) => {
      prefix = prefix ? `${prefix}/${part}` : part
      if (i === parts.length - 1) {
        level.push({ label: part, icon: icon(part), onSelect: () => open(f.path) })
        return
      }
      let d = dirs.get(prefix)
      if (!d) {
        d = { label: part, icon: 'i-lucide-folder', children: [], defaultExpanded: !!q || i < 1 }
        dirs.set(prefix, d)
        level.push(d)
      }
      level = d.children!
    })
  }
  return root
})

const isMarkdown = computed(() => selected.value.endsWith('.md'))
const lines = computed(() => content.value.split('\n'))
</script>

<template>
  <div class="grid grid-cols-1 lg:grid-cols-[minmax(16rem,22rem)_1fr] gap-4 min-h-[60vh]">
    <UCard :ui="{ body: 'p-2 sm:p-2' }">
      <UInput v-model="filter" icon="i-lucide-search" placeholder="Filter files" size="sm" class="w-full mb-2" />
      <USkeleton v-if="loading" class="h-40" />
      <p v-else-if="!files.length" class="text-sm text-(--ui-text-muted) p-2">This run wrote no files.</p>
      <div v-else class="overflow-auto max-h-[65vh]">
        <UTree :items="tree" size="sm" />
      </div>
    </UCard>
    <UCard :ui="{ header: 'py-2 sm:py-2', body: 'p-0 sm:p-0' }">
      <template #header>
        <div class="flex items-center justify-between gap-2">
          <span class="font-mono text-sm truncate">{{ selected || 'No file selected' }}</span>
          <UButtonGroup v-if="isMarkdown" size="xs">
            <UButton :variant="rendered ? 'solid' : 'outline'" color="neutral" label="Preview" @click="rendered = true" />
            <UButton :variant="rendered ? 'outline' : 'solid'" color="neutral" label="Source" @click="rendered = false" />
          </UButtonGroup>
        </div>
      </template>
      <USkeleton v-if="contentLoading" class="h-64 m-4" />
      <div v-else-if="isMarkdown && rendered" class="p-4 overflow-auto max-h-[70vh]">
        <MarkdownView :source="content" />
      </div>
      <div v-else class="overflow-auto max-h-[70vh] font-mono text-xs">
        <table class="w-full">
          <tbody>
            <tr v-for="(l, i) in lines" :key="i" class="hover:bg-(--ui-bg-muted)">
              <td class="select-none text-right pr-3 pl-3 text-(--ui-text-dimmed) align-top w-1">{{ i + 1 }}</td>
              <td class="whitespace-pre pr-4">{{ l }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </UCard>
  </div>
</template>
