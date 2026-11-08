<script setup lang="ts">
import { parse, stringify } from 'yaml'
import type { StepperItem, TableColumn, TableRow } from '@nuxt/ui'
import type { AppSummary, Cluster, LogLine, Run, ScanResult } from '~/utils/types'

const route = useRoute()
const me = useMe()
const notify = useNotify()
const UCheckbox = resolveComponent('UCheckbox')
const UBadge = resolveComponent('UBadge')

// --- state -------------------------------------------------------------------
const step = ref(0)
const clusters = ref<Cluster[]>([])
const cluster = ref<string>((route.query.cluster as string) || '')
const namespaces = ref<string[]>([])
const allNamespaces = ref<string[]>([])
const nsLoading = ref(false)
const selector = ref('')
const name = ref('')
const doc = ref<Record<string, any> | null>(null) // the Migration resource being built

const scanRun = ref<Run | null>(null)
const scanLines = ref<LogLine[]>([])
const scan = ref<ScanResult | null>(null)
const scanning = ref(false)
const excludeApps = ref<string[]>([])

const tool = ref<'none' | 'argocd' | 'flux'>('argocd')
const pattern = ref('app-of-apps')
const layout = ref('folder-per-env')
const repoURL = ref('')
const revision = ref('main')
const envs = ref<{ name: string, cluster: string, repoURL: string }[]>([{ name: 'prod', cluster: '', repoURL: '' }])
const secrets = ref('sealed-secrets')
const sealedScope = ref('strict')
const ageRecipients = ref('')
const esStoreKind = ref('ClusterSecretStore')
const esStoreName = ref('')
const adopt = ref(false)

const yamlText = ref('')
const yamlError = ref('')
const validating = ref(false)
const pushAfter = ref(false)
const commitMessage = ref('')
const creating = ref(false)

const operatorClusters = computed(() => clusters.value.filter(c => atLeast(c.role, 'operator')))
const clusterItems = computed(() => operatorClusters.value.map(c => ({ label: c.name, value: c.name, description: c.description })))
const envClusterItems = computed(() => [{ label: 'None (generate only)', value: '' }, ...clusters.value.map(c => ({ label: c.name, value: c.name }))])

const steps: StepperItem[] = [
  { title: 'Source', description: 'Cluster and scope', icon: 'i-lucide-server' },
  { title: 'Apps', description: 'What to migrate', icon: 'i-lucide-boxes' },
  { title: 'Target', description: 'GitOps tool and Git', icon: 'i-lucide-git-branch' },
  { title: 'Review', description: 'Check and generate', icon: 'i-lucide-rocket' }
]

// --- loading ------------------------------------------------------------------
onMounted(async () => {
  try {
    clusters.value = await api<Cluster[]>('/clusters')
  } catch (e) {
    notify.error('Loading clusters failed', e)
  }
  const from = route.query.from as string | undefined
  if (from) {
    await resumeFrom(from)
  } else if (!cluster.value && operatorClusters.value.length) {
    cluster.value = operatorClusters.value[0]!.name
  }
})

// resuming is set while a scan's configuration is loaded, so the cluster
// watcher keeps it instead of starting over.
let resuming = false
watch(cluster, async (c) => {
  if (!c) return
  if (!resuming) {
    // Another cluster: what was scanned and chosen belongs to the old one.
    namespaces.value = []
    scan.value = null
    scanRun.value = null
    scanLines.value = []
    excludeApps.value = []
    doc.value = null
  }
  allNamespaces.value = []
  nsLoading.value = true
  try {
    allNamespaces.value = await api<string[]>(`/clusters/${encodeURIComponent(c)}/namespaces`)
  } catch (e) {
    notify.error(`Listing the namespaces of ${c} failed`, e)
  } finally {
    nsLoading.value = false
  }
  if (!name.value || name.value === lastDefaultName.value) {
    name.value = defaultName(c)
    lastDefaultName.value = name.value
  }
})
const lastDefaultName = ref('')
function defaultName(c: string) {
  return (c.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 50) || 'migration') + '-migration'
}

// resumeFrom continues from a finished scan: its configuration and apps.
async function resumeFrom(id: string) {
  resuming = true
  try {
    const r = await api<Run>(`/runs/${id}`)
    scanRun.value = r
    cluster.value = r.cluster || ''
    await nextTick()
    doc.value = r.spec as Record<string, any>
    const sp = doc.value.spec || {}
    name.value = doc.value.metadata?.name || defaultName(r.cluster || '')
    namespaces.value = sp.scope?.namespaces || []
    selector.value = sp.scope?.labelSelector || ''
    excludeApps.value = sp.scope?.excludeApps || []
    readTarget(sp)
    scan.value = await api<ScanResult>(`/runs/${id}/scan`)
    step.value = 1
  } catch (e) {
    notify.error('Loading the scan failed', e)
  } finally {
    // After the cluster watcher ran.
    setTimeout(() => { resuming = false }, 0)
  }
}

function readTarget(sp: Record<string, any>) {
  if (sp.output?.tool) tool.value = sp.output.tool
  if (sp.output?.argocd?.pattern) pattern.value = sp.output.argocd.pattern
  if (sp.output?.layout) layout.value = sp.output.layout
  if (sp.output?.repo?.url) repoURL.value = sp.output.repo.url
  if (sp.output?.repo?.revision) revision.value = sp.output.repo.revision
  if (sp.secrets?.strategy) secrets.value = sp.secrets.strategy
  if (Array.isArray(sp.environments) && sp.environments.length && !(sp.environments.length === 1 && sp.environments[0].name === 'default')) {
    envs.value = sp.environments.map((e: any) => ({ name: e.name, cluster: e.cluster?.context || '', repoURL: e.repoURL || '' }))
  }
  adopt.value = !!sp.adopt?.enabled
}

async function template(): Promise<Record<string, any>> {
  const t = await api<{ migration: string }>('/migration/template', { query: { cluster: cluster.value } })
  return parse(t.migration)
}

// --- building the Migration resource --------------------------------------------
function build(base: Record<string, any>): Record<string, any> {
  // A plain copy: structuredClone can't copy Vue's reactive proxies.
  const d = JSON.parse(JSON.stringify(base))
  d.metadata = { ...(d.metadata || {}), name: name.value }
  const sp = d.spec || (d.spec = {})
  sp.scope = sp.scope || {}
  if (namespaces.value.length) sp.scope.namespaces = [...namespaces.value]
  else delete sp.scope.namespaces
  if (selector.value) sp.scope.labelSelector = selector.value
  else delete sp.scope.labelSelector
  if (excludeApps.value.length) sp.scope.excludeApps = [...excludeApps.value].sort()
  else delete sp.scope.excludeApps
  return d
}

function applyTarget(d: Record<string, any>) {
  const sp = d.spec
  sp.output = sp.output || {}
  sp.output.tool = tool.value
  sp.output.layout = layout.value
  sp.output.repo = { ...(sp.output.repo || {}), url: repoURL.value || undefined, revision: revision.value || undefined }
  if (tool.value === 'argocd') sp.output.argocd = { ...(sp.output.argocd || {}), pattern: pattern.value }
  sp.environments = envs.value.filter(e => e.name).map(e => ({
    name: e.name,
    ...(e.cluster && { cluster: { context: e.cluster } }),
    ...(layout.value === 'repo-per-env' && e.repoURL && { repoURL: e.repoURL })
  }))
  sp.secrets = sp.secrets || {}
  sp.secrets.strategy = secrets.value
  if (secrets.value === 'sealed-secrets') sp.secrets.sealedSecrets = { ...(sp.secrets.sealedSecrets || {}), scope: sealedScope.value }
  if (secrets.value === 'sops') sp.secrets.sops = { ...(sp.secrets.sops || {}), age: ageRecipients.value.split(/[\s,]+/).filter(Boolean) }
  if (secrets.value === 'external-secrets') sp.secrets.externalSecrets = { ...(sp.secrets.externalSecrets || {}), storeKind: esStoreKind.value, storeName: esStoreName.value }
  sp.adopt = { ...(sp.adopt || {}), enabled: adopt.value }
  return d
}

// --- step 1: scan --------------------------------------------------------------
async function startScan() {
  if (!cluster.value) return
  scanning.value = true
  scan.value = null
  scanLines.value = []
  try {
    doc.value = build(doc.value && scanRun.value?.cluster === cluster.value ? doc.value : await template())
    const run = await api<Run>('/runs', { method: 'POST', body: { type: 'scan', cluster: cluster.value, migration: stringify(doc.value) } })
    scanRun.value = run
    follow(run.id)
  } catch (e) {
    scanning.value = false
    notify.error('Starting the scan failed', e)
  }
}

function follow(id: string) {
  const es = new EventSource(`/api/v1/runs/${id}/events`)
  es.addEventListener('log', e => scanLines.value.push(JSON.parse((e as MessageEvent).data)))
  es.addEventListener('run', e => { scanRun.value = JSON.parse((e as MessageEvent).data) })
  es.addEventListener('end', async () => {
    es.close()
    scanning.value = false
    if (scanRun.value?.status === 'succeeded') {
      scan.value = await api<ScanResult>(`/runs/${id}/scan`)
      excludeApps.value = excludeApps.value.filter(a => scan.value!.apps.some(x => x.name === a))
      step.value = 1
    }
  })
  es.onerror = () => {
    // The browser reconnects by itself (and the server resumes after the
    // last line); give up only when it can't.
    if (es.readyState === EventSource.CLOSED) scanning.value = false
  }
}

// --- step 2: apps --------------------------------------------------------------
// In apply order: wave, then name.
const sortedApps = computed(() => [...(scan.value?.apps || [])].sort((a, b) => a.wave - b.wave || a.name.localeCompare(b.name)))
const included = computed(() => scan.value?.apps.filter(a => !excludeApps.value.includes(a.name)) || [])
function toggleApp(a: string, on: boolean) {
  excludeApps.value = on ? excludeApps.value.filter(x => x !== a) : [...excludeApps.value, a]
}
function setAll(on: boolean) {
  excludeApps.value = on ? [] : (scan.value?.apps.map(a => a.name) || [])
}
const appColumns: TableColumn<AppSummary>[] = [
  {
    id: 'select',
    header: () => h(UCheckbox, {
      modelValue: !excludeApps.value.length ? true : (included.value.length ? 'indeterminate' : false),
      'onUpdate:modelValue': (v: boolean | 'indeterminate') => setAll(!!v),
      'aria-label': 'All apps'
    }),
    cell: ({ row }: { row: TableRow<AppSummary> }) => h(UCheckbox, {
      modelValue: !excludeApps.value.includes(row.original.name),
      'onUpdate:modelValue': (v: boolean | 'indeterminate') => toggleApp(row.original.name, !!v),
      'aria-label': row.original.name
    })
  },
  { accessorKey: 'wave', header: 'Wave' },
  { accessorKey: 'name', header: 'App' },
  { accessorKey: 'namespace', header: 'Namespace' },
  { accessorKey: 'objects', header: 'Objects' },
  {
    id: 'what',
    header: 'What',
    cell: ({ row }: { row: TableRow<AppSummary> }) => row.original.helm
      ? h(UBadge, { variant: 'subtle', color: 'info' }, () => `Helm ${row.original.helm!.chart} ${row.original.helm!.version}`)
      : row.original.kinds.slice(0, 5).join(', ') + (row.original.kinds.length > 5 ? '…' : '')
  }
]

// --- step 4: review --------------------------------------------------------------
async function toReview() {
  try {
    if (!doc.value) doc.value = build(await template())
    doc.value = applyTarget(build(doc.value))
    yamlText.value = stringify(doc.value)
    step.value = 3
  } catch (e) {
    notify.error('Preparing the migration failed', e)
    return
  }
  await validate()
}

async function validate() {
  validating.value = true
  try {
    const r = await api<{ valid: boolean, error?: string }>('/migration/validate', { method: 'POST', body: { type: 'migrate', cluster: cluster.value, migration: yamlText.value } })
    yamlError.value = r.valid ? '' : (r.error || 'invalid')
  } catch (e) {
    yamlError.value = e instanceof Error ? e.message : String(e)
  } finally {
    validating.value = false
  }
}

// back leaves the review: edits to the YAML carry over to the form.
function back() {
  if (step.value === 3 && yamlText.value) {
    try {
      const d = parse(yamlText.value) as Record<string, any>
      if (d?.spec) {
        doc.value = d
        const sp = d.spec
        namespaces.value = sp.scope?.namespaces || []
        selector.value = sp.scope?.labelSelector || ''
        excludeApps.value = sp.scope?.excludeApps || []
        name.value = d.metadata?.name || name.value
        readTarget(sp)
      }
    } catch {
      // Unparsable YAML: the form keeps what it had.
    }
  }
  step.value--
}

// pushTarget is where the reviewed YAML pushes to, if anywhere.
const pushTarget = computed(() => {
  try {
    const sp = (parse(yamlText.value) as Record<string, any>)?.spec
    return (sp?.output?.repo?.url as string) || (sp?.environments || []).map((e: any) => e.repoURL).filter(Boolean).join(', ')
  } catch {
    return ''
  }
})

async function generate() {
  creating.value = true
  try {
    const run = await api<Run>('/runs', {
      method: 'POST',
      body: {
        type: 'migrate',
        cluster: cluster.value,
        migration: yamlText.value,
        ...(scanRun.value && { from: scanRun.value.id }),
        ...(pushAfter.value && pushTarget.value && { publish: { message: commitMessage.value } })
      }
    })
    notify.ok('Migration started')
    await navigateTo(`/runs/${run.id}`)
  } catch (e) {
    notify.error('Starting the migration failed', e)
  } finally {
    creating.value = false
  }
}

const canNext = computed(() => {
  if (step.value === 0) return !!scan.value
  if (step.value === 1) return included.value.length > 0
  if (step.value === 2) {
    return envs.value.some(e => e.name) && (tool.value === 'none' || !!repoURL.value || layout.value === 'repo-per-env') &&
      (secrets.value !== 'sealed-secrets' || !!envs.value[0]?.cluster)
  }
  return false
})

function next() {
  if (step.value === 2) return toReview()
  step.value++
}

const strategyItems = [
  { label: 'Sealed Secrets', value: 'sealed-secrets', description: 'Encrypted for the target cluster\'s controller' },
  { label: 'SOPS', value: 'sops', description: 'Encrypted with age, PGP or a cloud KMS' },
  { label: 'External Secrets', value: 'external-secrets', description: 'ExternalSecrets that read from your vault' },
  { label: 'Skip', value: 'skip', description: 'Leave Secrets out and list them in the report' }
]
const toolLabel = computed(() => toolItems.find(t => t.value === tool.value)?.label)
const strategyLabel = computed(() => strategyItems.find(s => s.value === secrets.value)?.label)
const toolItems = [
  { label: 'Argo CD', value: 'argocd', description: 'Applications or ApplicationSets and a pinned bootstrap' },
  { label: 'Flux', value: 'flux', description: 'Kustomizations, HelmReleases and a pinned bootstrap' },
  { label: 'Kustomize only', value: 'none', description: 'Plain apps and an apply script; wire them up yourself' }
]
</script>

<template>
  <UDashboardPanel id="new-migration">
    <template #header>
      <UDashboardNavbar title="New migration">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
      </UDashboardNavbar>
    </template>

    <template #body>
      <UAlert v-if="me && !atLeast(me.role, 'operator')" color="warning" variant="subtle" icon="i-lucide-lock" title="You can't start migrations" description="Starting scans and migrations needs the operator role on a cluster." />
      <template v-else>
        <UStepper v-model="step" :items="steps" class="w-full" :linear="false" disabled />

        <!-- 1. Source -->
        <UCard v-if="step === 0">
          <div class="grid gap-4 md:grid-cols-2">
            <UFormField label="Source cluster" description="The cluster whose apps go into Git." required>
              <USelectMenu v-model="cluster" :items="clusterItems" value-key="value" class="w-full" placeholder="Pick a cluster" />
            </UFormField>
            <UFormField label="Name" description="Names the migration and its runs.">
              <UInput v-model="name" class="w-full" />
            </UFormField>
            <UFormField label="Namespaces" description="Empty means every namespace (system ones are left out by the built-in rules).">
              <USelectMenu v-model="namespaces" :items="allNamespaces" multiple :loading="nsLoading" class="w-full" placeholder="All namespaces" />
            </UFormField>
            <UFormField label="Label selector" description="Only objects with these labels, such as team=shop." hint="optional">
              <UInput v-model="selector" class="w-full" placeholder="app.kubernetes.io/part-of=shop" />
            </UFormField>
          </div>
          <div class="flex items-center gap-3 mt-6">
            <UButton icon="i-lucide-scan-search" label="Scan the cluster" :loading="scanning" :disabled="!cluster" @click="startScan" />
            <span class="text-sm text-(--ui-text-muted)">Reads the cluster (nothing is changed) and groups what it finds into apps.</span>
          </div>
          <div v-if="scanRun && (scanning || scanLines.length)" class="mt-4 space-y-2">
            <div class="flex items-center gap-2 text-sm">
              <RunStatus :status="scanRun.status" />
              <NuxtLink :to="`/runs/${scanRun.id}`" class="text-(--ui-text-muted) hover:underline font-mono text-xs">{{ scanRun.id }}</NuxtLink>
            </div>
            <UAlert v-if="scanRun.error" color="error" variant="subtle" :description="scanRun.error" />
            <LogView :lines="scanLines" :live="scanning" />
          </div>
        </UCard>

        <!-- 2. Apps -->
        <UCard v-else-if="step === 1 && scan">
          <template #header>
            <div class="flex flex-wrap items-center justify-between gap-2">
              <div>
                <h2 class="font-semibold">{{ included.length }} of {{ scan.apps.length }} apps selected</h2>
                <p class="text-sm text-(--ui-text-muted)">{{ scan.source }}. Apps are applied wave by wave; dependencies come first.</p>
              </div>
              <UButton variant="ghost" color="neutral" icon="i-lucide-refresh-cw" label="Scan again" @click="step = 0" />
            </div>
          </template>
          <UAlert v-if="scan.secretActions.length" color="info" variant="subtle" icon="i-lucide-key-round" class="mb-3"
            :title="`${scan.secretActions.length} Secrets need a decision`" description="The next step picks how Secrets reach Git safely." />
          <UTable :data="sortedApps" :columns="appColumns" class="max-h-[55vh]" sticky />
        </UCard>

        <!-- 3. Target -->
        <div v-else-if="step === 2" class="grid gap-4 xl:grid-cols-2">
          <UCard>
            <template #header><h2 class="font-semibold">GitOps tool</h2></template>
            <URadioGroup v-model="tool" :items="toolItems" variant="card" />
            <UFormField v-if="tool === 'argocd'" label="Argo CD pattern" class="mt-4">
              <USelect v-model="pattern" :items="[{ label: 'App of apps', value: 'app-of-apps' }, { label: 'ApplicationSet', value: 'applicationset' }, { label: 'Single Application', value: 'single' }]" class="w-full" />
            </UFormField>
            <USwitch v-model="adopt" class="mt-4" label="Take over in place" description="The target already runs these objects (the source cluster itself): sync without recreating anything." />
          </UCard>

          <UCard>
            <template #header><h2 class="font-semibold">Git repository</h2></template>
            <div class="grid gap-4">
              <UFormField label="Repository URL" :required="tool !== 'none' && layout !== 'repo-per-env'" description="Where the GitOps tool reads from, and where gitopsify can push.">
                <UInput v-model="repoURL" class="w-full" placeholder="https://github.com/acme/platform-gitops.git" />
              </UFormField>
              <div class="grid grid-cols-2 gap-4">
                <UFormField label="Branch">
                  <UInput v-model="revision" class="w-full" />
                </UFormField>
                <UFormField label="Layout">
                  <USelect v-model="layout" :items="[{ label: 'Folder per environment', value: 'folder-per-env' }, { label: 'Repository per environment', value: 'repo-per-env' }]" class="w-full" />
                </UFormField>
              </div>
            </div>
          </UCard>

          <UCard>
            <template #header>
              <div class="flex items-center justify-between">
                <h2 class="font-semibold">Environments</h2>
                <UButton size="xs" variant="ghost" icon="i-lucide-plus" label="Add" @click="envs.push({ name: '', cluster: '', repoURL: '' })" />
              </div>
            </template>
            <div class="space-y-3">
              <div v-for="(e, i) in envs" :key="i" class="grid gap-2 md:grid-cols-[1fr_1fr_auto] items-end">
                <UFormField :label="i === 0 ? 'Name' : undefined">
                  <UInput v-model="e.name" placeholder="prod" class="w-full" />
                </UFormField>
                <UFormField :label="i === 0 ? 'Target cluster' : undefined">
                  <USelect v-model="e.cluster" :items="envClusterItems" class="w-full" />
                </UFormField>
                <UButton icon="i-lucide-x" color="neutral" variant="ghost" :disabled="envs.length === 1" aria-label="Remove" @click="envs.splice(i, 1)" />
                <UFormField v-if="layout === 'repo-per-env'" label="Repository" class="md:col-span-3">
                  <UInput v-model="e.repoURL" class="w-full" placeholder="https://github.com/acme/prod-gitops.git" />
                </UFormField>
              </div>
            </div>
            <p class="text-xs text-(--ui-text-muted) mt-3">A target cluster lets gitopsify fetch its Sealed Secrets certificate; it's never changed.</p>
          </UCard>

          <UCard>
            <template #header><h2 class="font-semibold">Secrets</h2></template>
            <URadioGroup v-model="secrets" :items="strategyItems" variant="card" />
            <UAlert v-if="secrets === 'sealed-secrets' && !envs[0]?.cluster" color="warning" variant="subtle" icon="i-lucide-triangle-alert" class="mt-4"
              description="Sealing needs the controller's certificate: pick the first environment's target cluster, or choose another strategy." />
            <UFormField v-if="secrets === 'sealed-secrets'" label="Sealing scope" class="mt-4">
              <USelect v-model="sealedScope" :items="['strict', 'namespace-wide', 'cluster-wide']" class="w-full" />
            </UFormField>
            <UFormField v-if="secrets === 'sops'" label="age recipients" class="mt-4" required>
              <UTextarea v-model="ageRecipients" class="w-full font-mono" placeholder="age1..." />
            </UFormField>
            <div v-if="secrets === 'external-secrets'" class="grid grid-cols-2 gap-4 mt-4">
              <UFormField label="Store kind">
                <USelect v-model="esStoreKind" :items="['ClusterSecretStore', 'SecretStore']" class="w-full" />
              </UFormField>
              <UFormField label="Store name" required>
                <UInput v-model="esStoreName" class="w-full" placeholder="vault" />
              </UFormField>
            </div>
          </UCard>
        </div>

        <!-- 4. Review -->
        <div v-else-if="step === 3" class="grid gap-4 xl:grid-cols-[1fr_22rem]">
          <UCard>
            <template #header>
              <div class="flex items-center justify-between">
                <div>
                  <h2 class="font-semibold">gitopsify.yaml</h2>
                  <p class="text-sm text-(--ui-text-muted)">The whole migration. Edit anything; the generated repository keeps a copy for CI.</p>
                </div>
                <UButton size="sm" variant="outline" color="neutral" icon="i-lucide-check-check" label="Validate" :loading="validating" @click="validate" />
              </div>
            </template>
            <UTextarea v-model="yamlText" :rows="28" autoresize :maxrows="40" class="w-full font-mono" :ui="{ base: 'font-mono text-xs' }" @blur="validate" />
            <UAlert v-if="yamlError" color="error" variant="subtle" icon="i-lucide-circle-alert" class="mt-3" :description="yamlError" />
            <UAlert v-else-if="!validating" color="success" variant="subtle" icon="i-lucide-circle-check" class="mt-3" description="Valid." />
          </UCard>
          <UCard>
            <template #header><h2 class="font-semibold">Generate</h2></template>
            <ul class="text-sm space-y-2 mb-4">
              <li class="flex gap-2"><UIcon name="i-lucide-server" class="mt-0.5 shrink-0" /><span>From <b>{{ cluster }}</b>, {{ included.length || 'all' }} apps</span></li>
              <li class="flex gap-2"><UIcon name="i-lucide-git-branch" class="mt-0.5 shrink-0" /><span>{{ toolLabel }}, {{ envs.map(e => e.name).filter(Boolean).join(', ') }}</span></li>
              <li class="flex gap-2"><UIcon name="i-lucide-key-round" class="mt-0.5 shrink-0" /><span>Secrets: {{ strategyLabel }}</span></li>
            </ul>
            <USwitch v-model="pushAfter" :disabled="!pushTarget" label="Push when it's done" :description="pushTarget || 'Set a repository URL to push'" />
            <UFormField v-if="pushAfter" label="Commit message" class="mt-3">
              <UInput v-model="commitMessage" class="w-full" placeholder="gitopsify: migrate from the cluster" />
            </UFormField>
            <UButton block class="mt-4" size="lg" icon="i-lucide-rocket" label="Generate the repository" :loading="creating" :disabled="!!yamlError || validating" @click="generate" />
            <p class="text-xs text-(--ui-text-muted) mt-2">Writes files on the server; nothing is applied to any cluster.</p>
          </UCard>
        </div>

        <div class="flex justify-between">
          <UButton v-if="step > 0" color="neutral" variant="outline" icon="i-lucide-arrow-left" label="Back" @click="back" />
          <span v-else />
          <UButton v-if="step < 3" trailing-icon="i-lucide-arrow-right" :label="step === 2 ? 'Review' : 'Next'" :disabled="!canNext" @click="next" />
        </div>
      </template>
    </template>
  </UDashboardPanel>
</template>
