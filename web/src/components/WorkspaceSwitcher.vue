<script setup lang="ts">
// Which workspace the console shows: the person's own, or a team they are in.
// A native select, so a phone opens its own picker and a screen reader reads
// it as one. Shown once there is more than one to choose from; when the list
// could not be read, it says so where the choice would be, with a retry.
import { computed, useId } from 'vue'
import { loadWorkspaces, selectWorkspace, workspaces } from '../state/workspaces'
import { workspaceName, workspaceRoleLabel } from '../ui/access'
import { describe } from '../ui/errors'
import { t } from '../ui/i18n'

const id = useId()
const options = computed(() => workspaces.list.map(item => ({
  id: item.id,
  label: item.kind === 'team' ? t('{name} · {role}', { name: workspaceName(item), role: workspaceRoleLabel(item.role) }) : workspaceName(item),
})))
const shown = computed(() => workspaces.supported && workspaces.list.length > 1)

function choose(event: Event) {
  selectWorkspace((event.target as HTMLSelectElement).value)
}
</script>

<template>
  <div v-if="shown" class="workspace-switcher">
    <label :for="id">{{ t('Workspace') }}</label>
    <select :id="id" name="workspace" :value="workspaces.currentID" @change="choose">
      <option v-for="option in options" :key="option.id" :value="option.id" :selected="option.id === workspaces.currentID">{{ option.label }}</option>
    </select>
  </div>
  <div v-else-if="workspaces.failure" class="workspace-failure" role="alert">
    <span>{{ describe(workspaces.failure) }}</span>
    <button class="ghost small" type="button" :disabled="workspaces.loading" @click="loadWorkspaces">{{ t('Try again') }}</button>
  </div>
</template>

<style scoped>
.workspace-switcher { display: grid; gap: 6px; padding: 0 12px; }
.workspace-switcher label { font-size: 11px; font-weight: 600; color: var(--text-dim); text-transform: uppercase; letter-spacing: .4px; }
.workspace-switcher select { width: 100%; min-height: 40px; padding: 8px 10px; border: 1px solid var(--console-border); border-radius: 9px; background: var(--bg-input); color: var(--text); font: inherit; font-size: 13px; text-overflow: ellipsis; }
.workspace-switcher select:focus-visible { outline: 2px solid var(--console-accent); outline-offset: 2px; }
.workspace-failure { display: grid; gap: 8px; margin: 0 12px; padding: 10px 12px; border: 1px solid color-mix(in srgb, var(--danger) 40%, transparent); border-radius: 10px; color: var(--danger); font-size: 12px; line-height: 1.45; }
.workspace-failure button { justify-self: start; }
@media (max-width: 760px) { .workspace-switcher select { font-size: 16px; min-height: 44px; } }
</style>
