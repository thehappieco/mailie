<script setup lang="ts">
/**
 * The Mailie mark. The geometry lives in src/brand/mark.ts (provisional until
 * the company kit exists); this component only adds the classes and dash
 * attributes that let mark.css colour it (outline | tile | auto = theme-driven)
 * and choreograph the parts.
 */
import { nextTick, ref, useTemplateRef } from 'vue'
import { FRAME_INNER, FRAME_OUTER, MAILIE_PARTS, draw, type Part } from '../brand/mark'

const props = withDefaults(defineProps<{
  mode?: 'outline' | 'tile' | 'auto'
  /** Written as width/height attributes so the box is reserved before the stylesheet loads. */
  size?: number
  animate?: 'load' | 'hover' | 'none'
  /** Replay on pointerenter where hovering exists, on tap where it does not. */
  playOnHover?: boolean
  /** Accessible name; without one the mark is decorative. */
  label?: string
}>(), { mode: 'auto', size: 34, animate: 'none', playOnHover: false, label: undefined })

const playing = ref(props.animate === 'load')
const svg = useTemplateRef<SVGSVGElement>('svg')

/** Matching is wrapped: a browser without matchMedia simply animates. */
function reduced(): boolean {
  try { return window.matchMedia('(prefers-reduced-motion: reduce)').matches } catch { return false }
}
function hoverable(query: string): boolean {
  try { return window.matchMedia(query).matches } catch { return false }
}

/** Dash attributes only while playing: at rest the path carries no dash pattern and is the plain stroke. */
function dash(part: Part): { dasharray?: string; dashoffset?: string } {
  if (!playing.value || !part.draw || part.length === undefined || part.width === undefined) return {}
  return draw(part.length, part.width)
}

/** Start, or restart, the choreography. */
async function play(): Promise<void> {
  if (reduced()) return
  playing.value = true
  await nextTick()
  const element = svg.value
  // Rewind rather than cancel: cancelling shows the un-animated frame for an instant before the restart.
  if (element && typeof element.getAnimations === 'function') {
    for (const animation of element.getAnimations({ subtree: true })) { animation.currentTime = 0; animation.play() }
  }
}

function onPointerEnter(): void {
  if (props.animate === 'hover' && !playing.value) { void play(); return }
  if (props.playOnHover && hoverable('(hover: hover)')) void play()
}
function onClick(): void {
  if (props.playOnHover && hoverable('(pointer: coarse)')) void play()
}

defineExpose({ play })
</script>

<template>
  <svg
    ref="svg"
    class="bm"
    :class="{ 'bm--play': playing }"
    data-brand="mailie"
    :data-mode="mode"
    viewBox="0 0 90 90"
    :width="size"
    :height="size"
    :role="label ? 'img' : undefined"
    :aria-label="label"
    :aria-hidden="label ? undefined : 'true'"
    focusable="false"
    @pointerenter="onPointerEnter"
    @click="onClick"
  >
    <title v-if="label">{{ label }}</title>
    <path class="bm-frame" :d="FRAME_OUTER" />
    <path class="bm-interior" :d="FRAME_INNER" />
    <g v-for="part in MAILIE_PARTS" :key="part.id" :class="['bm-part-wrap', `bm-mailie-${part.id}-wrap`]">
      <path
        :class="['bm-part', `bm-mailie-${part.id}`, part.kind === 'fill' ? 'bm-fill' : 'bm-cut']"
        :d="part.d"
        :stroke-width="part.width"
        :stroke-linecap="part.linecap"
        :stroke-linejoin="part.linejoin"
        :stroke-dasharray="dash(part).dasharray"
        :stroke-dashoffset="dash(part).dashoffset"
      />
    </g>
  </svg>
</template>

<!-- Global on purpose: the colour system and keyframes are shared with the lockup. -->
<style src="../brand/mark.css"></style>
