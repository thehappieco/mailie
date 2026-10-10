<script setup lang="ts">
// The page's one step-up prompt (state/stepUp.ts): the password dialog, over
// whatever dialog is open, while a flow waits for a step-up. Given, the flow
// goes on; closed, it stops. An edition with a step-up of its own
// (Edition.stepUp) is asked instead, by state/stepUp.ts, and nothing is drawn
// here.
import { edition } from '../edition'
import { stepUpGiven, stepUpPrompt, stepUpRefused } from '../state/stepUp'
import StepUpDialog from './StepUpDialog.vue'
</script>

<template>
  <StepUpDialog v-if="stepUpPrompt.open && !edition().stepUp" @done="stepUpGiven" @close="stepUpRefused" />
</template>
