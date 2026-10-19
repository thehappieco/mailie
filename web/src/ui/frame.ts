// The core's own frame owns the page's scrolling: the document never scrolls,
// and #app is fixed to the visual viewport and scrolls inside it, so a phone's
// keyboard shrinks the page instead of covering the field being typed in
// (ui/styles.css, under FIXED_FRAME_CLASS on <html>). App.vue asks for it, for
// every phase it draws. A host that draws a frame of its own around the
// core's panels leaves it off, and its document scrolls as any page does.

import { onBeforeMount, onBeforeUnmount } from 'vue'

/** The class on <html> under which ui/styles.css gives the page the core's own frame. */
export const FIXED_FRAME_CLASS = 'fixed-frame'

/**
 * Gives the page the core's own frame while the calling component is mounted:
 * FIXED_FRAME_CLASS on <html> from before its first render until it is
 * unmounted. Call it from a component's setup.
 */
export function useFixedFrame(): void {
  onBeforeMount(() => { document.documentElement.classList.add(FIXED_FRAME_CLASS) })
  onBeforeUnmount(() => { document.documentElement.classList.remove(FIXED_FRAME_CLASS) })
}
