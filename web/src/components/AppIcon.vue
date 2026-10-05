<script lang="ts">
export type IconName =
  | 'mail'
  | 'mail-open'
  | 'inbox'
  | 'server'
  | 'cloud'
  | 'send'
  | 'pencil'
  | 'trash'
  | 'archive'
  | 'alert'
  | 'layers'
  | 'star'
  | 'folder'
  | 'external'
  | 'user'
  | 'users'
  | 'lock'
  | 'menu'
  | 'chevron-down'
  | 'chevron-right'
  | 'close'
  | 'check'
  | 'clock'
  | 'eye'
  | 'eye-off'
  | 'shield'
  | 'info'
  | 'logout'
  | 'refresh'
  | 'plus'
  | 'search'
  | 'paperclip'
  | 'download'
  | 'arrow-left'
  | 'undo'
  | 'key'
  | 'copy'
  | 'reply'
  | 'reply-all'
  | 'forward'

// One path per icon on a 24-unit grid, stroked in currentColor at 1.8 with
// round caps and joins — the sibling console's drawing rules, so the two sets
// sit together. The shared shapes are the sibling's own paths.
const paths: Record<IconName, string> = {
  mail: 'M4 5h16a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1Zm-1 1.5 9 6.5 9-6.5',
  'mail-open': 'M3 10 12 4l9 6v9a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-9Zm0 0 9 6 9-6',
  inbox: 'M3 13h5l1.5 3h5l1.5-3h5M5.5 5h13L21 13v5a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-5l2.5-8Z',
  server: 'M5 4h14a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1Zm0 10h14a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1v-4a1 1 0 0 1 1-1Zm3-7h.01M8 17h.01',
  cloud: 'M6 18.5h10.8a4.3 4.3 0 0 0 .6-8.58 6.5 6.5 0 0 0-12.36 1.28A3.8 3.8 0 0 0 6 18.5Z',
  send: 'm3 3 19 9-19 9 4-9-4-9Zm4 9h15',
  pencil: 'm14 4 6 6M3 21l5-1L21 7l-5-5L3 15v6Z',
  trash: 'M4 7h16M9 7V4h6v3M6 7l1 14h10l1-14m-8 4v6m4-6v6',
  archive: 'M3 4h18v4H3zM5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8m-9 4h4',
  alert: 'M12 3 2 20h20L12 3Zm0 6v5m0 3h.01',
  layers: 'm12 3 9 5-9 5-9-5 9-5Zm-9 9 9 5 9-5m-18 4 9 5 9-5',
  star: 'm12 3 2.8 5.7 6.2.9-4.5 4.4 1.1 6.2-5.6-2.9-5.6 2.9 1.1-6.2L3 9.6l6.2-.9L12 3Z',
  folder: 'M3 6a1 1 0 0 1 1-1h5l2 2h9a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V6Z',
  external: 'M14 4h6v6m0-6-9 9m8 1v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1h5',
  user: 'M16 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM4 21v-1a8 8 0 0 1 16 0v1',
  users: 'M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2M13 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0Zm10 14v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75',
  lock: 'M6 11h12a1 1 0 0 1 1 1v8a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1v-8a1 1 0 0 1 1-1Zm2 0V7a4 4 0 0 1 8 0v4',
  menu: 'M4 6h16M4 12h16M4 18h16',
  'chevron-down': 'm6 9 6 6 6-6',
  'chevron-right': 'm9 6 6 6-6 6',
  close: 'm6 6 12 12M6 18 18 6',
  check: 'm5 12 4 4L19 6',
  clock: 'M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Zm-9-5v5l3 2',
  eye: 'M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12Zm13 0a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z',
  'eye-off': 'm3 3 18 18M10.6 5.1 12 5c6.5 0 10 7 10 7a18.5 18.5 0 0 1-3 3.8M6.4 6.4A19.4 19.4 0 0 0 2 12s3.5 7 10 7a10.8 10.8 0 0 0 5.6-1.6M9.9 9.9a3 3 0 0 0 4.2 4.2',
  shield: 'M12 2 3 6v6c0 5 9 10 9 10s9-5 9-10V6l-9-4Zm-4 10 3 3 5-6',
  info: 'M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Zm-9-1v6m0-10h.01',
  logout: 'M9 3H4v18h5m5-14 5 5-5 5m-6-5h13',
  refresh: 'M20 7a9 9 0 0 0-15-2L2 8m0-5v5h5m-3 9a9 9 0 0 0 15 2l3-3m0 5v-5h-5',
  plus: 'M12 5v14M5 12h14',
  search: 'M11 18a7 7 0 1 1 0-14 7 7 0 0 1 0 14Zm5-2 5 5',
  paperclip: 'm20 11.5-8.3 8.3a5 5 0 0 1-7.1-7.1l8.5-8.5a3.3 3.3 0 0 1 4.7 4.7l-8.5 8.5a1.7 1.7 0 0 1-2.4-2.4l7.8-7.8',
  download: 'M12 4v11m-5-5 5 5 5-5M5 20h14',
  'arrow-left': 'M19 12H5m6-6-6 6 6 6',
  undo: 'M9 14 4 9l5-5M4 9h11a5 5 0 0 1 0 10h-3',
  key: 'M20 9a5 5 0 1 1-10 0 5 5 0 0 1 10 0Zm-8.5 3.5L3 21m3-3 2.5 2.5M8.5 15.5l2 2M15 9h.01',
  copy: 'M9 9h10a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H9a1 1 0 0 1-1-1V10a1 1 0 0 1 1-1Zm-4 6H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1',
  reply: 'M9 6 3 12l6 6M3 12h11a7 7 0 0 1 7 7',
  'reply-all': 'M8 6l-6 6 6 6m5-12-6 6 6 6m-6-6h8a7 7 0 0 1 7 7',
  forward: 'M15 6l6 6-6 6m6-6H10a7 7 0 0 0-7 7',
}
</script>

<script setup lang="ts">
withDefaults(defineProps<{ name: IconName; size?: number | string }>(), { size: 22 })
</script>

<template>
  <svg
    class="app-icon"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.8"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
  >
    <path :d="paths[name]" />
  </svg>
</template>

<style scoped>
.app-icon {
  display: inline-block;
  flex-shrink: 0;
  vertical-align: middle;
}
</style>
