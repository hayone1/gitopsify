<script setup lang="ts">
import { parse, stringify } from 'yaml'
import type { StepperItem } from '@nuxt/ui'
import type { Run, Tool, ToolCheck } from '~/utils/types'

const route = useRoute()
const notify = useNotify()

const step = ref(0)
const tools = ref<Tool[]>([])
const tool = ref((route.query.tool as string) || '')
const checking = ref(false)
const info = ref<ToolCheck | null>(null)
const partitions = ref<string[]>([])
const kinds = ref<string[]>([])
const formats = ref<string[]>([])
const adopt = ref(true)
const allowPartial = ref(false)
const repoURL = ref('')
const branch = ref('main')
const pushAfter = ref(false)
const commitMessage = ref('')
const yamlText = ref('')
const yamlError = ref('')
const validating = ref(false)
const creating = ref(false)

const steps: StepperItem[] = [
  { title: 'Tool', description: 'What to read', icon: 'i-lucide-blocks' },
  { title: 'Scope', description: 'Objects and output', icon: 'i-lucide-list-filter' },
  { title: 'Review', description: 'Check and export', icon: 'i-lucide-rocket' }
]

const operable = computed(() => tools.value.filter(t => atLeast(t.role, 'operator') && t.installed))
const toolItems = computed(() => operable.value.map(t => ({ label: t.name, value: t.name, description: t.title || t.provider })))
const desc = computed(() => info.value?.description)
const supportedFormats = computed(() => [
  ...(desc.value?.terraform ? [{ label: 'Terraform / OpenTofu', value: 'terraform' }] : []),
  ...(desc.value?.crossplane ? [{ label: 'Crossplane', value: 'crossplane' }] : [])
])

onMounted(async () => {
  try {
    tools.value = await api<Tool[]>('/tools')
  } catch (e) {
    notify.error('Loading tools failed', e)
  }
  if (!tool.value && operable.value.length) tool.value = operable.value[0]!.name
})

watch(tool, async (t) => {
  if (!t) return
  checking.value = true
  info.value = null
  try {
    info.value = await api<ToolCheck>(`/tools/${encodeURIComponent(t)}/check`)
    const d = info.value.description
    if (d) {
      kinds.value = d.kinds.filter(k => !k.optional).map(k => k.name)
      formats.value = supportedFormats.value.map(f => f.value)
    }
    partitions.value = []
  } catch (e) {
    notify.error('Checking the tool failed', e)
  } finally {
    checking.value = false
  }
}, { immediate: true })

async function toReview() {
  try {
    const t = await api<{ export: string }>('/export/template', { query: { tool: tool.value } })
    const doc = parse(t.export) as Record<string, any>
    const sp = doc.spec || (doc.spec = {})
    sp.select = { ...(sp.select || {}) }
    if (partitions.value.length) sp.select.partitions = [...partitions.value]
    sp.select.kinds = [...kinds.value]
    sp.output = { ...(sp.output || {}), formats: [...formats.value], adopt: adopt.value }
    delete sp.instances
    yamlText.value = stringify(doc)
    step.value = 2
    await validate()
  } catch (e) {
    notify.error('Preparing the export failed', e)
  }
}

async function validate() {
  validating.value = true
  try {
    const r = await api<{ valid: boolean, error?: string }>('/migration/validate', { method: 'POST', body: { type: 'export', tool: tool.value, export: yamlText.value } })
    yamlError.value = r.valid ? '' : (r.error || 'invalid')
  } catch (e) {
    yamlError.value = e instanceof Error ? e.message : String(e)
  } finally {
    validating.value = false
  }
}

async function start() {
  creating.value = true
  try {
    const run = await api<Run>('/runs', {
      method: 'POST',
      body: {
        type: 'export',
        tool: tool.value,
        export: yamlText.value,
        allowPartial: allowPartial.value,
        ...(repoURL.value && { repo: { url: repoURL.value, branch: branch.value } }),
        ...(pushAfter.value && repoURL.value && { publish: { message: commitMessage.value } })
      }
    })
    notify.ok('Export started')
    await navigateTo(`/runs/${run.id}`)
  } catch (e) {
    notify.error('Starting the export failed', e)
  } finally {
    creating.value = false
  }
}

const canNext = computed(() => step.value === 0 ? !!info.value?.check : kinds.value.length > 0 && formats.value.length > 0)
</script>

<template>
  <UDashboardPanel id="new-export">
    <template #header>
      <UDashboardNavbar title="New export">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
      </UDashboardNavbar>
    </template>
    <template #body>
      <UAlert v-if="tools.length && !operable.length" color="warning" variant="subtle" icon="i-lucide-lock" title="Nothing to export"
        description="Exporting needs the operator role on a tool whose provider is installed." />
      <template v-else>
        <UStepper v-model="step" :items="steps" class="w-full" :linear="false" disabled />

        <UCard v-if="step === 0">
          <UFormField label="Tool" description="A registered instance; its connection is configured on the server." required>
            <USelectMenu v-model="tool" :items="toolItems" value-key="value" class="w-full md:w-96" placeholder="Pick a tool" />
          </UFormField>
          <div class="mt-4">
            <div v-if="checking" class="flex items-center gap-2 text-sm text-(--ui-text-muted)"><UIcon name="i-lucide-loader-circle" class="animate-spin" /> Connecting…</div>
            <UAlert v-else-if="info?.error" color="error" variant="subtle" icon="i-lucide-unplug" :description="info.error" />
            <div v-else-if="info?.check" class="space-y-2 text-sm">
              <p class="flex items-center gap-2 text-(--ui-success)"><UIcon name="i-lucide-plug-zap" />{{ info.check.target }}<template v-if="info.check.identity"> as {{ info.check.identity }}</template></p>
              <p class="text-(--ui-text-muted)">{{ info.check.partitions.length }} {{ desc?.partition.plural }} · provider {{ desc?.name }} {{ desc?.version }}</p>
              <UAlert v-for="(w, i) in info.check.warnings || []" :key="i" color="warning" variant="subtle" :description="w" />
            </div>
          </div>
        </UCard>

        <div v-else-if="step === 1 && info?.check && desc" class="grid gap-4 xl:grid-cols-2">
          <UCard>
            <template #header><h2 class="font-semibold capitalize">{{ desc.partition.plural }}</h2></template>
            <p class="text-sm text-(--ui-text-muted) mb-2">None checked exports them all. Each gets its own Terraform workspace or Kustomization.</p>
            <UCheckboxGroup v-model="partitions" :items="info.check.partitions.map(p => ({ label: p.name, value: p.name, description: p.id !== p.name ? p.id : undefined }))" />
          </UCard>
          <UCard>
            <template #header><h2 class="font-semibold">Kinds</h2></template>
            <UCheckboxGroup v-model="kinds" :items="desc.kinds.map(k => ({ label: k.plural || k.name, value: k.name, description: k.description, disabled: info?.check?.kinds?.some(c => c.kind === k.name && !c.ok) }))" />
          </UCard>
          <UCard>
            <template #header><h2 class="font-semibold">Output</h2></template>
            <UCheckboxGroup v-model="formats" :items="supportedFormats" orientation="horizontal" />
            <USwitch v-model="adopt" class="mt-4" label="Adopt what exists" description="Terraform imports and Crossplane observes first: the first plan or sync changes nothing." />
            <USwitch v-model="allowPartial" class="mt-3" label="Export even if something can't be read" description="Otherwise a partial read fails the export." />
          </UCard>
          <UCard>
            <template #header><h2 class="font-semibold">Git</h2></template>
            <div class="grid gap-3">
              <UFormField label="Repository URL" hint="optional">
                <UInput v-model="repoURL" class="w-full" placeholder="https://github.com/acme/grafana-iac.git" />
              </UFormField>
              <UFormField label="Branch">
                <UInput v-model="branch" class="w-full" />
              </UFormField>
            </div>
          </UCard>
        </div>

        <div v-else-if="step === 2" class="grid gap-4 xl:grid-cols-[1fr_22rem]">
          <UCard>
            <template #header>
              <div class="flex items-center justify-between">
                <div>
                  <h2 class="font-semibold">Export resource</h2>
                  <p class="text-sm text-(--ui-text-muted)">The export the server runs; the output keeps a copy for CI (gitopsify export --no-input).</p>
                </div>
                <UButton size="sm" variant="outline" color="neutral" icon="i-lucide-check-check" label="Validate" :loading="validating" @click="validate" />
              </div>
            </template>
            <UTextarea v-model="yamlText" :rows="24" autoresize :maxrows="36" class="w-full" :ui="{ base: 'font-mono text-xs' }" @blur="validate" />
            <UAlert v-if="yamlError" color="error" variant="subtle" icon="i-lucide-circle-alert" class="mt-3" :description="yamlError" />
            <UAlert v-else-if="!validating" color="success" variant="subtle" icon="i-lucide-circle-check" class="mt-3" description="Valid." />
          </UCard>
          <UCard>
            <template #header><h2 class="font-semibold">Export</h2></template>
            <ul class="text-sm space-y-2 mb-4">
              <li class="flex gap-2"><UIcon name="i-lucide-blocks" class="mt-0.5 shrink-0" /><span>{{ tool }}: {{ partitions.length || 'all' }} {{ desc?.partition.plural }}</span></li>
              <li class="flex gap-2"><UIcon name="i-lucide-shapes" class="mt-0.5 shrink-0" /><span>{{ kinds.length }} kinds → {{ formats.join(' and ') }}</span></li>
            </ul>
            <USwitch v-model="pushAfter" :disabled="!repoURL" label="Push when it's done" :description="repoURL || 'Set a repository URL to push'" />
            <UFormField v-if="pushAfter" label="Commit message" class="mt-3">
              <UInput v-model="commitMessage" class="w-full" />
            </UFormField>
            <UButton block class="mt-4" size="lg" icon="i-lucide-rocket" label="Export" :loading="creating" :disabled="!!yamlError || validating" @click="start" />
            <p class="text-xs text-(--ui-text-muted) mt-2">Reads the tool; nothing in it is changed. Secret values stay out of the output.</p>
          </UCard>
        </div>

        <div class="flex justify-between">
          <UButton v-if="step > 0" color="neutral" variant="outline" icon="i-lucide-arrow-left" label="Back" @click="step--" />
          <span v-else />
          <UButton v-if="step < 2" trailing-icon="i-lucide-arrow-right" :label="step === 1 ? 'Review' : 'Next'" :disabled="!canNext" @click="step === 1 ? toReview() : step++" />
        </div>
      </template>
    </template>
  </UDashboardPanel>
</template>
