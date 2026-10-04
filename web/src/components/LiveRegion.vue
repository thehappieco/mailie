<script setup lang="ts">
// Mounted once for the page and once inside each modal dialog; never behind a
// v-if, or it would be created together with the text it is meant to announce.
import { announcement } from '../ui/announce'
</script>

<template>
  <div class="visually-hidden" role="status" aria-live="polite" aria-atomic="true">{{ announcement }}</div>
</template>
