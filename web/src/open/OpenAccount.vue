<script setup lang="ts">
// The open console's Account section: name, password and sessions, and what
// the person allows this server to do with their mailboxes.
import AccountPanel from '../components/AccountPanel.vue'
import ActionsPermission from '../components/ActionsPermission.vue'
import SyncPermission from '../components/SyncPermission.vue'
</script>

<template>
  <AccountPanel>
    <SyncPermission />
    <ActionsPermission />
  </AccountPanel>
</template>
