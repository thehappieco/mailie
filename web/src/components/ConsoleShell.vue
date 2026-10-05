<script lang="ts">
import type { Component } from 'vue'
import type { IconName } from './AppIcon.vue'

export interface ConsoleSection {
  id: string
  label: string
  /** The line under the header, saying what the section is for. */
  description: string
  icon: IconName
  component: Component
  /** A number beside the label, when there is one to show. */
  count?: number
}
</script>

<script setup lang="ts">
// The console's frame, which every edition's console is built on: a sidebar
// of the sections the edition declares as data, a header, and the sections
// themselves, mounted the first time they are visited and kept alive after
// that. Below 760 px the sidebar becomes a drawer. The edition decides which
// sections exist (a list that may change while the console is open: a
// section that goes away takes the person home), which one is the person's
// own account, and where the console opens once it knows the person's
// mailboxes; and what the frame is called (edition().shell): the open
// edition's is a console, another's may be an app. Anything else the edition
// keeps on screen across sections goes in the default slot.
//
// While the console is on screen it reads the mailboxes, the providers, and
// where the person stands on sync and on actions, and keeps the event stream
// open.
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { edition } from '../edition'
import { accounts, loadAccounts, loadProviders } from '../state/accounts'
import { actionsConsent, loadActionsConsent } from '../state/actionsConsent'
import { server } from '../state/connection'
import { startLive, stopLive, wakeLive } from '../state/live'
import { session, signOut } from '../state/session'
import { consent, loadConsent } from '../state/sync'
import { initials } from '../ui/format'
import { t } from '../ui/i18n'
import AppearanceMenu from './AppearanceMenu.vue'
import AppIcon from './AppIcon.vue'
import BrandLockup from './BrandLockup.vue'
import ConsoleDialog from './ConsoleDialog.vue'

const props = defineProps<{
  sections: ConsoleSection[]
  /** Where the console opens, and where it goes when the section shown goes away. */
  home: string
  /** The person's own section: the profile button in the sidebar and the header opens it. */
  account: string
  /** Once, when the first list of mailboxes arrives and nobody has picked a section yet: where to go instead of home. */
  landing?: () => string | undefined
}>()

const section = ref(props.home)
const visited = ref(new Set<string>([props.home]))
/** The person chose a section; the console stops choosing for them. */
let navigated = false
const mobileNav = ref(false)
const content = ref<HTMLElement>()
const signingOut = ref(false)
const current = computed(() => props.sections.find(item => item.id === section.value) ?? props.sections[0]!)
const accountLabel = computed(() => props.sections.find(item => item.id === props.account)?.label ?? '')
const profileName = computed(() => session.user?.name || session.user?.email || '')
/** What the edition calls what this frame holds: the breadcrumb's root. */
const rootLabel = computed(() => edition().shell.rootLabel())
/** The lockup's name: the product, and the edition's root beside it unless the root is the product itself. */
const brandLabel = computed(() => rootLabel.value === 'Mailie' ? 'Mailie' : `Mailie · ${rootLabel.value}`)
const navLabel = computed(() => edition().shell.navLabel())

function show(id: string) {
  navigated = true
  section.value = id
  mobileNav.value = false
  visited.value.add(id)
  void nextTick(() => content.value?.scrollTo({ top: 0 }))
}

async function leave() {
  if (signingOut.value) return
  signingOut.value = true
  mobileNav.value = false
  try { await signOut() } finally { signingOut.value = false }
}

function landed() {
  const target = navigated ? undefined : props.landing?.()
  if (target && section.value === props.home && props.sections.some(item => item.id === target)) {
    section.value = target
    visited.value.add(target)
  }
  navigated = true
}
watch(() => accounts.loaded, loaded => { if (loaded) landed() })
watch(() => props.sections.some(item => item.id === section.value), present => { if (!present) section.value = props.home })

// A network that comes back cuts the event stream's reconnection wait short.
function online() { wakeLive() }

onMounted(() => {
  if (accounts.loaded) landed()
  else void loadAccounts()
  if (!accounts.providersLoaded) void loadProviders()
  if (!consent.loaded) void loadConsent()
  if (!actionsConsent.loaded) void loadActionsConsent()
  startLive()
  window.addEventListener('online', online)
})
onBeforeUnmount(() => {
  window.removeEventListener('online', online)
  stopLive()
})
</script>

<template>
  <a class="skip-link" href="#console-content" @click.prevent="content?.focus()">{{ t('Skip to content') }}</a>
  <div class="console console-app">
    <aside class="console-sidebar" :aria-label="navLabel">
      <div class="console-brand"><BrandLockup :size="34" animate="load" :label="brandLabel" /></div>
      <nav class="console-nav" :aria-label="t('Sections')">
        <button v-for="item in sections" :key="item.id" type="button" class="console-nav-item"
          :class="{ active: section === item.id }" :aria-current="section === item.id ? 'page' : undefined"
          @click="show(item.id)">
          <AppIcon :name="item.icon" :size="20" /><span>{{ item.label }}</span>
          <span v-if="item.count !== undefined" class="nav-count">{{ item.count }}</span>
        </button>
      </nav>
      <div class="console-profile">
        <button type="button" class="profile-trigger" :aria-label="accountLabel" @click="show(account)"><span class="profile-initials" aria-hidden="true">{{ initials(profileName) }}</span><span class="profile-text"><strong>{{ profileName }}</strong><small>{{ session.user?.email }}</small></span><AppIcon name="chevron-right" :size="15" /></button>
        <button class="profile-signout" type="button" :disabled="signingOut" @click="leave"><AppIcon name="logout" :size="17" /><span>{{ t('Sign out') }}</span></button>
      </div>
    </aside>

    <div class="console-main">
      <header class="console-header">
        <button class="icon-btn mobile-menu" type="button" :aria-label="t('Open menu')" aria-haspopup="dialog" :aria-expanded="mobileNav" @click="mobileNav = true"><AppIcon name="menu" :size="23" /></button>
        <div class="grow"><div class="console-breadcrumb">{{ rootLabel }} <span>/</span> {{ current.label }}</div><h1>{{ current.label }}</h1></div>
        <AppearanceMenu />
        <button class="mobile-profile" type="button" :aria-label="accountLabel" @click="show(account)"><span aria-hidden="true">{{ initials(profileName) }}</span></button>
      </header>

      <main id="console-content" ref="content" class="console-content" tabindex="-1" :aria-label="current.label">
        <div class="console-section-intro"><p>{{ current.description }}</p><span class="connection-label" :class="{ connected: server.reachable }" role="status"><i aria-hidden="true" />{{ server.reachable ? t('Server connected') : t('No connection to the server') }}</span></div>
        <template v-for="item in sections" :key="item.id">
          <component :is="item.component" v-if="visited.has(item.id)" v-show="section === item.id" />
        </template>
      </main>
    </div>

    <slot />

    <ConsoleDialog v-if="mobileNav" title="Mailie" drawer @close="mobileNav = false">
      <div class="mobile-navigation">
        <nav class="console-nav" :aria-label="t('Sections')"><button v-for="item in sections" :key="item.id" type="button" class="console-nav-item" :class="{ active: section === item.id }" :aria-current="section === item.id ? 'page' : undefined" @click="show(item.id)"><AppIcon :name="item.icon" :size="21" /><span>{{ item.label }}</span><span v-if="item.count !== undefined" class="nav-count">{{ item.count }}</span></button></nav>
        <div class="mobile-account-area">
          <button type="button" class="profile-trigger" @click="show(account)"><span class="profile-initials" aria-hidden="true">{{ initials(profileName) }}</span><span class="profile-text"><strong>{{ profileName }}</strong><small>{{ session.user?.email }}</small></span></button>
          <button class="console-nav-item" type="button" :disabled="signingOut" @click="leave"><AppIcon name="logout" :size="20" />{{ t('Sign out') }}</button>
        </div>
      </div>
    </ConsoleDialog>
  </div>
</template>

<style scoped>
.console-app { height: 100%; display: grid; grid-template-columns: 236px minmax(0, 1fr); color: var(--text); background: var(--bg); overflow: hidden; }
.console-sidebar { min-width: 0; display: flex; flex-direction: column; gap: 26px; padding: 28px 16px 18px; background: var(--bg-panel); border-right: 1px solid var(--console-border); overflow: auto; }
.console-brand { display: flex; align-items: center; gap: 10px; padding: 0 12px; }
.console-nav { display: flex; flex-direction: column; gap: 5px; }
.console-nav-item { display: flex; gap: 12px; align-items: center; width: 100%; min-height: 44px; padding: 10px 12px; border-radius: 9px; color: var(--text-dim); font-size: 13px; font-weight: 550; text-align: left; white-space: nowrap; }
.console-nav-item:hover:not(:disabled) { background: var(--bg-hover); color: var(--text); }
.console-nav-item.active { background: var(--console-tint); color: var(--console-accent); font-weight: 650; }
.console-nav-item:focus-visible, .profile-trigger:focus-visible, .profile-signout:focus-visible, .mobile-profile:focus-visible { outline: 2px solid var(--console-accent); outline-offset: 2px; }
.nav-count { margin-left: auto; padding: 1px 6px; font-size: 11px; border-radius: 5px; background: var(--bg-panel); color: var(--text-dim); font-weight: 550; }
.console-profile { margin-top: auto; padding: 14px 0 0; border-top: 1px solid var(--console-border); display: grid; gap: 8px; }
.profile-signout { display: flex; align-items: center; gap: 10px; min-height: 36px; padding: 8px 4px; color: var(--text-dim); font-size: 12px; text-align: left; border-radius: 8px; }
.profile-signout:hover:not(:disabled) { color: var(--text); }
.console-main { display: flex; flex-direction: column; min-height: 0; min-width: 0; }
.console-header { display: flex; align-items: center; gap: 12px; min-height: 96px; padding: 20px 36px; border-bottom: 1px solid var(--console-border); background: var(--bg-panel); flex-shrink: 0; }
.console-breadcrumb { font-size: 11px; color: var(--text-dim); overflow: hidden; white-space: nowrap; text-overflow: ellipsis; }
.console-breadcrumb span { margin: 0 7px; color: var(--text-faint); }
.console-header h1 { margin: 7px 0 0; font-size: 25px; font-weight: 650; letter-spacing: -.7px; line-height: 1.15; }
.mobile-menu, .mobile-profile { display: none; }
.mobile-profile { width: 36px; height: 36px; min-width: 36px; aspect-ratio: 1; flex-shrink: 0; padding: 0; border: 0; overflow: hidden; border-radius: 50%; background: var(--accent-dim); color: var(--accent); font-weight: 600; font-size: 13px; }
.profile-trigger { display: flex; gap: 10px; align-items: center; min-width: 0; flex: 1; text-align: left; padding: 7px 4px; border-radius: 10px; color: var(--text-dim); }
.profile-trigger:hover { background: var(--bg-hover); }
.profile-initials { width: 35px; height: 35px; border-radius: 50%; flex: none; display: grid; place-items: center; background: var(--accent-dim); color: var(--accent); font-size: 13px; font-weight: 600; }
.profile-text { min-width: 0; flex: 1; }
.profile-text strong, .profile-text small { display: block; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.profile-text strong { font-size: 12px; font-weight: 600; color: var(--text); }
.profile-text small { font-size: 11px; color: var(--text-dim); margin-top: 3px; }
.mobile-navigation { display: flex; flex-direction: column; gap: 22px; min-height: 70dvh; }
.mobile-navigation .console-nav-item { min-height: 48px; }
.mobile-account-area { margin-top: auto; border-top: 1px solid var(--line); padding-top: 14px; display: grid; gap: 12px; }
.mobile-account-area .profile-trigger { padding: 10px; }
.console-content { padding: 28px 36px 48px; min-height: 0; overflow-y: auto; overscroll-behavior: contain; scrollbar-gutter: stable; }
.console-content:focus { outline: none; }
.console-content > :not(:first-child) { margin-top: 20px; }
.console-section-intro { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.console-section-intro p { color: var(--text-dim); font-size: 13px; margin: 0; line-height: 1.6; }
.connection-label { display: flex; align-items: center; gap: 6px; color: var(--text-dim); font-size: 11px; flex-shrink: 0; margin-top: 3px; }
.connection-label i { width: 6px; height: 6px; border-radius: 50%; background: var(--danger); flex-shrink: 0; }
.connection-label.connected i { background: var(--ok); }
.console-app :deep(.console-panel) { border-color: var(--console-border); padding: 24px; gap: 16px; }
.console-app :deep(.console-panel h2) { font-size: 17px; font-weight: 600; }
.console-app :deep(.console-panel p) { line-height: 1.65; margin: 0; }
.console-app :deep(.console-panel input:not([type='checkbox']):not([type='radio'])), .console-app :deep(.console-panel select) {
  min-height: 42px; border: 1px solid var(--console-border); border-radius: 8px; background: var(--bg-input); color: var(--text); font: inherit; font-size: 13px;
}
.console-app :deep(input::placeholder) { color: var(--text-faint); opacity: 1; }
.console-app :deep(input:-webkit-autofill) { -webkit-text-fill-color: var(--text); -webkit-box-shadow: 0 0 0 1000px var(--bg-input) inset; caret-color: var(--text); }
.console-app :deep(input:focus-visible), .console-app :deep(select:focus-visible) { outline: 2px solid var(--console-accent); outline-offset: 2px; }
.console-app :deep(.console-panel button) { min-height: 36px; }
@media (max-width: 1020px) {
  .console-app { grid-template-columns: 208px minmax(0, 1fr); }
  .console-sidebar { padding: 22px 12px 16px; }
  .console-header { padding: 20px 24px; }
  .console-content { padding: 24px; }
  .connection-label { display: none; }
}
@media (max-width: 760px) {
  .console-app { display: flex; }
  .console-sidebar { display: none; }
  .console-main { flex: 1; }
  .console-header { min-height: 78px; padding: 14px; gap: 10px; }
  .console-header h1 { font-size: 20px; margin-top: 5px; }
  .console-breadcrumb { font-size: 10px; }
  .console-breadcrumb > span { margin: 0 4px; }
  .mobile-menu { display: grid; place-items: center; flex: none; }
  .mobile-profile { display: grid; place-items: center; flex: none; }
  .console-header :deep(.appearance-trigger) { padding: 8px; }
  .console-content { padding: 18px 14px calc(28px + env(safe-area-inset-bottom)); scrollbar-gutter: auto; }
  .console-section-intro p { font-size: 12px; }
  .console-app :deep(.console-panel) { padding: 18px 16px; }
  .console-app :deep(.console-panel input:not([type='checkbox']):not([type='radio'])), .console-app :deep(.console-panel select) { font-size: 16px; }
}
</style>
