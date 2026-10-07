<script setup lang="ts">
// The open console's Account section: name, password and sessions, what the
// person allows this server to do with their mailboxes, and the API keys they
// created, in every workspace.
import AccountPanel from '../components/AccountPanel.vue'
import ActionsPermission from '../components/ActionsPermission.vue'
import MyKeys from '../components/MyKeys.vue'
import SyncPermission from '../components/SyncPermission.vue'
</script>

<template>
  <AccountPanel>
    <SyncPermission />
    <ActionsPermission />
    <MyKeys />
  </AccountPanel>
</template>
