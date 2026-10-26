<script setup lang="ts">
// The person's notices, above every section of the console (state/notices.ts):
// a mailbox they read that someone else removed, or a team mailbox whose sync
// someone else turned off, deleting its index, or paused, keeping it — in
// whichever workspace it happened, since they concern the person, not the
// workspace shown. Each stays until they dismiss it, a recovery or the
// pause's end withdraws it or the server's journal lets it go (90 days at
// most), and is said once through the live region when it is first read. Read when the console starts; the event stream says when
// there is a new one.
import { onMounted } from 'vue'
import { dismissPersonNotice, loadNotices, notices } from '../state/notices'
import { describe } from '../ui/errors'
import { stamp } from '../ui/format'
import { t } from '../ui/i18n'
import { personNoticeText } from '../ui/personNotices'
import AppIcon from './AppIcon.vue'

onMounted(() => { if (!notices.loaded && !notices.loading) void loadNotices() })
</script>

<template>
  <ul v-if="notices.list.length" class="person-notices" :aria-label="t('Notices')">
    <li v-for="notice in notices.list" :key="notice.id" class="note person-notice">
      <AppIcon :name="notice.kind === 'sync_off' ? 'eye-off' : notice.kind === 'paused' ? 'pause' : 'trash'" :size="17" />
      <span class="grow">
        <span class="person-notice-text">{{ personNoticeText(notice) }}</span>
        <small>{{ stamp(notice.at) }}</small>
        <span v-if="notices.problem?.id === notice.id" class="person-notice-problem" role="alert">{{ describe(notices.problem.failure) }}</span>
      </span>
      <button class="icon-btn" type="button" :disabled="notices.dismissing === notice.id" :aria-label="t('Dismiss')" @click="dismissPersonNotice(notice.id)"><AppIcon name="close" :size="16" /></button>
    </li>
  </ul>
</template>

<style scoped>
.person-notices { list-style: none; margin: 0; padding: 0; display: grid; gap: 8px; }
.person-notice { display: flex; align-items: flex-start; gap: 10px; margin: 0; border-left-color: var(--warn); }
.person-notice > .app-icon { flex: none; margin-top: 1px; color: var(--warn-text); }
.person-notice-text { display: block; overflow-wrap: anywhere; line-height: 1.5; color: var(--text); }
.person-notice small { display: block; margin-top: 3px; font-size: 11px; color: var(--text-dim); }
.person-notice-problem { display: block; margin-top: 4px; color: var(--danger); }
.person-notice .icon-btn { min-width: 32px; min-height: 32px; padding: 4px; margin: -6px -6px -6px 0; color: inherit; }
</style>
