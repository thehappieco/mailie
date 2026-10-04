<script setup lang="ts">
import { t } from '../ui/i18n'
</script>

<template>
  <div class="empty app-loading" role="status" aria-live="polite">
    <div>
      <div class="loading-spinner" aria-hidden="true" />
      <div class="big">{{ t('Finishing the connection…') }}</div>
      <div>{{ t('Mailie is confirming the authorization with the provider.') }}</div>
    </div>
  </div>
</template>
