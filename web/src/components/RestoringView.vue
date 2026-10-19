<script setup lang="ts">
// What the page shows while it restores a remembered session (the session's
// phase 'restoring'), and, when the server could not be asked, the way on:
// trying again, or signing in again.
import { describe } from '../ui/errors'
import { t } from '../ui/i18n'
import { forgetRemembered, restore, session } from '../state/session'
</script>

<template>
  <div class="empty app-loading" role="status" aria-live="polite">
    <div>
      <template v-if="!session.restoreFailed"><div class="loading-spinner" aria-hidden="true" /><div class="big">{{ t('Restoring your session…') }}</div></template>
      <template v-else>
        <p class="alert" role="alert">{{ describe({ op: 'restore', code: 'unavailable' }) }}</p>
        <div class="restore-actions"><button class="primary" type="button" @click="restore">{{ t('Try again') }}</button><button class="ghost" type="button" @click="forgetRemembered">{{ t('Sign in again') }}</button></div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.restore-actions { display: flex; flex-wrap: wrap; justify-content: center; gap: 10px; }
.app-loading .alert { max-width: 420px; }
</style>
