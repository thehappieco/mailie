<script setup lang="ts">
// What a section of the workspace shown says instead of what it holds, when
// nothing in that workspace can be used (ui/access.ts, workspaceUnusable): a
// team that closed, or one this server's edition does not offer yet. The
// server refuses every use of it whatever the console shows; this says why
// there is nothing here, and that nothing of it is lost.
import { computed } from 'vue'
import type { Workspace } from '../api/types'
import { unusableNotice, workspaceName, workspaceUnusable } from '../ui/access'
import AppIcon from './AppIcon.vue'

const props = defineProps<{ workspace: Workspace | undefined }>()

const notice = computed(() => {
  const why = workspaceUnusable(props.workspace)
  return why ? unusableNotice(why, workspaceName(props.workspace)) : null
})
</script>

<template>
  <div v-if="notice" class="empty-card unusable-workspace" data-unusable>
    <span class="empty-icon"><AppIcon name="lock" :size="30" /></span>
    <h3>{{ notice.title }}</h3>
    <p>{{ notice.text }}</p>
  </div>
</template>

<style scoped>
.unusable-workspace h3 { overflow-wrap: anywhere; }
.empty-icon { width: 56px; height: 56px; border-radius: 16px; display: grid; place-items: center; background: var(--bg-hover); color: var(--text-dim); }
</style>
