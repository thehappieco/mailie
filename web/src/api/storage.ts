// What the caller's mailboxes take up in the index. Whose mailboxes they are,
// and whether the database's size is told, is decided by the server
// (internal/service); this file only asks.

import { checked, request } from './http'
import { isStorage, type Storage } from './types'

/** With workspace, only that workspace's mailboxes (?workspace=). */
export async function getStorage(token: string, workspace = '', signal?: AbortSignal): Promise<Storage> {
  return checked(await request('/v1/me/storage', { token, signal, query: workspace ? { workspace } : undefined }), isStorage)
}
