// The open console: what a self-hosted server serves at its own origin.
import { createApp } from 'vue'

import App from './App.vue'
// The palette (its tokens), then the stylesheet, which reads them as the components do.
import './ui/palette.css'
import './ui/styles.css'
import { configureEdition } from './edition'
import { openEdition } from './open/edition'
import { initializeLocale } from './ui/i18n'
import { initializeTheme } from './ui/preferences'

configureEdition(openEdition)
initializeLocale()
initializeTheme()

createApp(App).mount('#app')
