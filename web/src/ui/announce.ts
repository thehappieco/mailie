// What a screen reader should hear when something changes out of sight.
//
// A role="status" element created together with its text is often not read:
// screen readers watch live regions that already exist and announce what
// changes inside them. So the console keeps one region mounted for its whole
// life (LiveRegion.vue, in App.vue and inside every modal dialog, since a modal
// makes everything outside it inert) and changes only its text.

import { nextTick, ref } from 'vue'

/** The text of every live region. Read by LiveRegion.vue; written only by announce(). */
export const announcement = ref('')

/**
 * announce says text politely. It clears the region first and writes on the
 * next tick, so the same sentence twice in a row is still read twice.
 */
export function announce(text: string): void {
  announcement.value = ''
  void nextTick(() => { announcement.value = text })
}
