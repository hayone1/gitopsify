<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'
import type { Run } from '~/utils/types'

const props = defineProps<{ runs: Run[], loading?: boolean, compact?: boolean }>()
const UBadge = resolveComponent('UBadge')
const NuxtLink = resolveComponent('NuxtLink')
const RunStatus = resolveComponent('RunStatus')

const columns = computed<TableColumn<Run>[]>(() => {
  const cols: TableColumn<Run>[] = [
    {
      accessorKey: 'name',
      header: 'Run',
      cell: ({ row }) => h(NuxtLink, { to: `/runs/${row.original.id}`, class: 'font-medium hover:underline' }, () => [
        h('span', row.original.name || row.original.id),
        h('span', { class: 'block text-xs text-(--ui-text-dimmed) font-mono' }, row.original.id)
      ])
    },
    {
      accessorKey: 'type',
      header: 'Type',
      cell: ({ row }) => h(UBadge, { variant: 'outline', color: 'neutral', class: 'capitalize' }, () => row.original.type === 'migrate' ? 'migration' : 'scan')
    },
    { accessorKey: 'cluster', header: 'Cluster' },
    { accessorKey: 'status', header: 'Status', cell: ({ row }) => h(RunStatus, { status: row.original.status }) }
  ]
  if (!props.compact) {
    cols.push(
      {
        id: 'result',
        header: 'Result',
        cell: ({ row }) => {
          const s = row.original.summary
          if (!s) return row.original.error ? h('span', { class: 'text-(--ui-error) text-xs line-clamp-2' }, row.original.error) : ''
          return `${s.apps} apps · ${s.objects} objects` + (s.files ? ` · ${s.files} files` : '') + (s.warnings ? ` · ${s.warnings} warnings` : '')
        }
      },
      { accessorKey: 'createdBy', header: 'By' }
    )
  }
  cols.push({
    accessorKey: 'createdAt',
    header: 'Started',
    cell: ({ row }) => h('span', { title: formatTime(row.original.createdAt) }, ago(row.original.createdAt) + (row.original.finishedAt ? ` · ${duration(row.original.startedAt, row.original.finishedAt)}` : ''))
  })
  return cols
})
</script>

<template>
  <UTable :data="runs" :columns="columns" :loading="loading" class="flex-1" :empty="'No runs yet.'" />
</template>
