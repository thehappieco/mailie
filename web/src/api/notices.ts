// The signed-in person's notices: what someone else did to a mailbox they
// read (removed it, or turned a team mailbox's sync off), kept until they
// dismiss it. A notice is a person's: the server refuses a key.

import { segment } from './endpoint'
import { checked, request } from './http'
import { isPersonNoticeList, type PersonNotice } from './types'

/** The person's notices, the newest first. */
export async function listNotices(token: string, signal?: AbortSignal): Promise<PersonNotice[]> {
  return checked(await request('/v1/me/notices', { token, signal }), isPersonNoticeList)
}

/** Dismisses one of the person's notices for good; one they no longer have is not_found. */
export async function dismissNotice(token: string, id: string): Promise<void> {
  await request<void>(`/v1/me/notices/${segment(id)}`, { token, method: 'DELETE' })
}
