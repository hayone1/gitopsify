<script setup lang="ts">
import DOMPurify from 'dompurify'
import { marked } from 'marked'

const props = defineProps<{ source: string }>()
const emit = defineEmits<{ open: [path: string] }>()

// Reports quote what clusters and tools hold (chart names, object names,
// messages): text only. No styles, forms or images (the page's CSP stops
// scripts, not a fake sign-in overlay); external links open elsewhere, and
// links to other files of the run open them here.
if (!(globalThis as { gitopsifyPurifyHook?: boolean }).gitopsifyPurifyHook) {
  (globalThis as { gitopsifyPurifyHook?: boolean }).gitopsifyPurifyHook = true
  DOMPurify.addHook('afterSanitizeAttributes', (node) => {
    if (node.tagName !== 'A') return
    const href = node.getAttribute('href') || ''
    if (/^(https?:|mailto:)/i.test(href)) {
      node.setAttribute('target', '_blank')
      node.setAttribute('rel', 'noopener noreferrer')
    } else if (!href.startsWith('#')) {
      node.setAttribute('data-path', href)
      node.setAttribute('href', '#')
    }
  })
}
const html = computed(() => DOMPurify.sanitize(marked.parse(props.source, { async: false, gfm: true }) as string, {
  FORBID_TAGS: ['style', 'form', 'input', 'button', 'textarea', 'select', 'img', 'video', 'audio', 'svg', 'math'],
  FORBID_ATTR: ['style']
}))

function onClick(e: MouseEvent) {
  const a = (e.target as HTMLElement).closest('a[data-path]')
  if (!a) return
  e.preventDefault()
  emit('open', a.getAttribute('data-path') || '')
}
</script>

<template>
  <!-- eslint-disable-next-line vue/no-v-html -->
  <div class="markdown text-sm leading-relaxed" @click="onClick" v-html="html" />
</template>

<style>
.markdown h1 { font-size: 1.5rem; font-weight: 600; margin: 1rem 0 .5rem; }
.markdown h2 { font-size: 1.2rem; font-weight: 600; margin: 1.25rem 0 .5rem; }
.markdown h3 { font-weight: 600; margin: 1rem 0 .4rem; }
.markdown p, .markdown ul, .markdown ol, .markdown table, .markdown pre { margin: .5rem 0; }
.markdown ul { list-style: disc; padding-left: 1.4rem; }
.markdown ol { list-style: decimal; padding-left: 1.4rem; }
.markdown code { font-size: .85em; background: var(--ui-bg-muted); padding: .1rem .3rem; }
.markdown pre { background: var(--ui-bg-muted); padding: .75rem; overflow: auto; }
.markdown pre code { background: none; padding: 0; }
.markdown table { border-collapse: collapse; display: block; overflow: auto; }
.markdown th, .markdown td { border: 1px solid var(--ui-border); padding: .3rem .6rem; text-align: left; }
.markdown a { color: var(--ui-primary); text-decoration: underline; }
</style>
