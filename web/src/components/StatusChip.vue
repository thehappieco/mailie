<script setup lang="ts">
import type { AccountState } from '../api/types'
import { stateLabel, stateTone } from '../ui/labels'

defineProps<{ state: AccountState }>()
</script>

<template>
  <span class="status-chip" :class="stateTone(state)"><i aria-hidden="true" />{{ stateLabel(state) }}</span>
</template>
