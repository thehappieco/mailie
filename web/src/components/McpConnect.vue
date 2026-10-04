<script setup lang="ts">
// How to connect an AI assistant: the MCP server's URL, and the Claude Code
// command with a placeholder where the key goes. A real key is never drawn
// here; the creation dialog is the one place that can copy a command with the
// key in it, right after the key is made.
import { computed, onBeforeUnmount, ref } from 'vue'
import { announce } from '../ui/announce'
import { claudeCommand, keyPlaceholder, mcpEndpoint } from '../ui/apikeys'
import { copyText } from '../ui/clipboard'
import { t } from '../ui/i18n'
import AppIcon from './AppIcon.vue'

const endpoint = mcpEndpoint(location.origin)
const command = computed(() => claudeCommand(endpoint, keyPlaceholder()))
const copied = ref<'' | 'url' | 'command'>('')
const copyFailed = ref(false)
let copiedTimer: ReturnType<typeof setTimeout> | undefined

async function copy(what: 'url' | 'command') {
  const ok = await copyText(what === 'url' ? endpoint : command.value)
  copyFailed.value = !ok
  copied.value = ok ? what : ''
  clearTimeout(copiedTimer)
  if (!ok) return
  announce(what === 'url' ? t('URL copied.') : t('Command copied.'))
  copiedTimer = setTimeout(() => { copied.value = '' }, 4_000)
}
onBeforeUnmount(() => clearTimeout(copiedTimer))
</script>

<template>
  <section class="connect-panel" aria-labelledby="mcp-connect-title">
    <div class="connect-head">
      <span class="connect-icon"><AppIcon name="layers" :size="20" /></span>
      <div class="grow">
        <h2 id="mcp-connect-title">{{ t('Connect an AI assistant') }}</h2>
        <p>{{ t('Tools that speak MCP, such as Claude Code, reach your mail through Mailie’s MCP server with a key you create here.') }}</p>
      </div>
    </div>

    <div class="connect-field">
      <h3>{{ t('MCP server URL') }}</h3>
      <div class="copy-row">
        <code class="notranslate" translate="no">{{ endpoint }}</code>
        <button class="ghost small" type="button" @click="copy('url')"><AppIcon :name="copied === 'url' ? 'check' : 'copy'" :size="16" />{{ copied === 'url' ? t('Copied') : t('Copy URL') }}</button>
      </div>
    </div>

    <div class="connect-field">
      <h3>Claude Code</h3>
      <p>{{ t('Run this in a terminal, with your key where it says {placeholder}.', { placeholder: keyPlaceholder() }) }}</p>
      <div class="copy-row">
        <code class="notranslate command" translate="no">{{ command }}</code>
        <button class="ghost small" type="button" @click="copy('command')"><AppIcon :name="copied === 'command' ? 'check' : 'copy'" :size="16" />{{ copied === 'command' ? t('Copied') : t('Copy command') }}</button>
      </div>
    </div>

    <p>{{ t('Other MCP clients: give them the URL above and the header Authorization: Bearer followed by your key.') }}</p>
    <p v-if="copyFailed" class="alert" role="alert">{{ t('Your browser did not let Mailie copy. Select the text and copy it yourself.') }}</p>
    <p class="note">{{ t('Connectors on claude.ai are not supported yet: they sign in with OAuth, which Mailie does not offer yet. Tools that send a key in a header, such as Claude Code, work now.') }}</p>
  </section>
</template>

<style scoped>
.connect-panel { display: grid; gap: 16px; padding: 22px; background: var(--bg-raised); border: 1px solid var(--console-border); border-radius: 12px; min-width: 0; }
.connect-panel p { margin: 0; font-size: 13px; line-height: 1.55; color: var(--text-dim); }
.connect-head { display: flex; align-items: flex-start; gap: 14px; }
.connect-head h2 { margin: 2px 0 6px; font-size: 16px; font-weight: 650; }
.connect-icon { display: grid; place-items: center; flex: none; width: 40px; height: 40px; border-radius: 12px; background: var(--accent-dim); color: var(--accent); }
.connect-field { display: grid; gap: 8px; min-width: 0; }
.connect-field h3 { margin: 0; font-size: 13px; font-weight: 600; }
.copy-row { display: flex; align-items: center; gap: 10px; min-width: 0; }
.copy-row code { flex: 1; min-width: 0; padding: 10px 12px; border: 1px solid var(--console-border); border-radius: 8px; background: var(--bg-input); color: var(--text); font-family: var(--mono); font-size: 12.5px; line-height: 1.5; overflow-wrap: anywhere; user-select: all; }
.copy-row button { display: inline-flex; align-items: center; gap: 6px; flex: none; }
.connect-panel .note { margin: 0; }
@media (max-width: 600px) {
  .connect-panel { padding: 17px; }
  .copy-row { flex-direction: column; align-items: stretch; }
  .copy-row button { justify-content: center; }
}
</style>
