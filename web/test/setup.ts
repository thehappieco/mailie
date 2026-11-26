// Every core spec runs with the test edition configured (test/edition.ts),
// as an edition's entry does before mounting. A spec that loads the modules
// again (freshModules in support.ts) configures the new ones the same way.
import { configureEdition } from '../src/edition'
import { testEdition } from './edition'

configureEdition(testEdition)
