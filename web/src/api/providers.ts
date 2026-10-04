import { checked, request } from './http'
import { isProviderList, type Provider } from './types'

/**
 * The providers this server can connect, and how, for this caller. The flows
 * already reflect where the console is served from: a hosted console is never
 * offered the loopback flow, so the list is used as given.
 */
export async function listProviders(token: string, signal?: AbortSignal): Promise<Provider[]> {
  return checked(await request('/v1/providers', { token, signal }), isProviderList)
}
