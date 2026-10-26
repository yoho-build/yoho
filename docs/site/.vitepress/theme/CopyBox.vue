<script setup>
// Copyable text that wraps, so a reader sees all of it at once (no sideways scroll).
import { ref } from 'vue'

const props = defineProps({
  text: { type: String, required: true },
  label: { type: String, default: '' },
  mono: { type: Boolean, default: false }
})
const state = ref('Copy')
const box = ref(null)

async function copy() {
  try {
    await navigator.clipboard.writeText(props.text)
    state.value = 'Copied'
  } catch {
    const r = document.createRange(); r.selectNodeContents(box.value)
    const s = getSelection(); s.removeAllRanges(); s.addRange(r)
    state.value = 'Press ⌘C'
  }
  setTimeout(() => (state.value = 'Copy'), 1800)
}
</script>

<template>
  <div class="yh-copy" :class="{ mono }">
    <div class="yh-copy-head">
      <span>{{ label }}</span>
      <button type="button" @click="copy">{{ state }}</button>
    </div>
    <p ref="box" class="yh-copy-text">{{ text }}</p>
  </div>
</template>
