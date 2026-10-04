// Every core spec runs with the open edition configured, as src/main.ts does
// before mounting. A spec that loads the modules again (freshModules in
// support.ts) configures the new ones the same way.
import { configureEdition } from '../src/edition'
import { openEdition } from '../src/open/edition'

configureEdition(openEdition)
