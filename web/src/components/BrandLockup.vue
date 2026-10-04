<script setup lang="ts">
/**
 * The brand lockup: the mark, the wordmark, and the edition's byline when it
 * has one, laid out the way the company site lays out its products — the
 * name's top on the mark's top edge and the byline's bottom on its bottom
 * edge. Without a byline the name sits on the mark's middle. The wordmark
 * takes the mark's own colour: the brand colour on a light theme, white on a
 * dark one, like the frame.
 *
 * Neither string is translated. They are names, identical in every language,
 * so they carry no catalogue key.
 */
import { edition } from '../edition'
import BrandMark from './BrandMark.vue'

const byline = edition().byline

withDefaults(defineProps<{
  /** Mark size in px; the text scales with it (34 in the console sidebar, 44 on the sign-in card). */
  size?: 30 | 34 | 44
  animate?: 'load' | 'hover' | 'none'
  playOnHover?: boolean
  /** Accessible name for the whole lockup; without one it is decorative. */
  label?: string
}>(), { size: 34, animate: 'load', playOnHover: true, label: undefined })
</script>

<template>
  <span class="lockup" :data-size="size" :role="label ? 'img' : undefined" :aria-label="label">
    <span class="lockup-mark"><BrandMark :size="size" mode="auto" :animate="animate" :play-on-hover="playOnHover" /></span>
    <span class="lockup-text" :class="{ solo: !byline }" aria-hidden="true">
      <span class="lockup-name">mailie</span>
      <span v-if="byline" class="lockup-by">{{ byline }}</span>
    </span>
  </span>
</template>

<style scoped>
.lockup { --lk: 34px; --lk-color: #B24B1E; display: inline-flex; align-items: center; gap: 0; color: var(--lk-color); }
.lockup[data-size="30"] { --lk: 30px; }
.lockup[data-size="44"] { --lk: 44px; }
:root[data-theme='dark'] .lockup { --lk-color: #ffffff; }
.lockup-mark { display: block; flex: 0 0 auto; }
/* The text block spans the mark's height, so the two edges line up. */
.lockup-text { display: flex; flex-direction: column; justify-content: space-between; align-self: stretch; margin-left: calc(var(--lk) * .2); animation: lockup-reveal .45s var(--ease) .25s both; }
.lockup-name { font-weight: 800; letter-spacing: -.03em; font-size: calc(var(--lk) * .6); line-height: 1; margin-top: calc(var(--lk) * -.085); white-space: nowrap; }
.lockup-text.solo { justify-content: center; }
.lockup-text.solo .lockup-name { margin-top: 0; }
.lockup-by { font-family: var(--mono); font-size: calc(var(--lk) * .28); line-height: 1; letter-spacing: .01em; word-spacing: -.12em; color: var(--text-dim); white-space: nowrap; }
/* The reveal wipes from the left. The final inset is negative so the frozen
   clip never cuts glyphs that sit outside the box: the name's negative top
   margin and the byline's descenders. */
@keyframes lockup-reveal { from { clip-path: inset(-30% 100% -30% -10%); } to { clip-path: inset(-30% -20% -30% -10%); } }
@media (prefers-reduced-motion: reduce) { .lockup-text { animation: none; } }
</style>
