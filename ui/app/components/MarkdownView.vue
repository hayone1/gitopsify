<script setup lang="ts">
import DOMPurify from 'dompurify'
import { marked } from 'marked'

const props = defineProps<{ source: string }>()
const html = computed(() => DOMPurify.sanitize(marked.parse(props.source, { async: false, gfm: true }) as string))
</script>

<template>
  <!-- eslint-disable-next-line vue/no-v-html -->
  <div class="markdown text-sm leading-relaxed" v-html="html" />
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
