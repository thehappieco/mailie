// What the caller's mailboxes take up in the index. Whose mailboxes they are,
// and whether the database's size is told, is decided by the server
// (internal/service); this file only asks.

import { checked, request } from './http'
import { isStorage, type Storage } from './types'

export async function getStorage(token: string, signal?: AbortSignal): Promise<Storage> {
  return checked(await request('/v1/me/storage', { token, signal }), isStorage)
}
