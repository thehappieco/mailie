<script setup lang="ts">
// The page's one step-up prompt (state/stepUp.ts): the password dialog, over
// whatever dialog is open, while a flow waits for a step-up. Given, the flow
// goes on; closed, it stops.
import { stepUpGiven, stepUpPrompt, stepUpRefused } from '../state/stepUp'
import StepUpDialog from './StepUpDialog.vue'
</script>

<template>
  <StepUpDialog v-if="stepUpPrompt.open" @done="stepUpGiven" @close="stepUpRefused" />
</template>
