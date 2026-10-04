// The add-account form, before it becomes a request.

import type { AddAccountRequest, ProviderID, SMTPSecurity } from '../api/types'

export interface AccountDraft {
  email: string
  displayName: string
  password: string
  imapHost: string
  imapPort: number
  smtpHost: string
  smtpPort: number
  smtpTLS: SMTPSecurity
  loginUser: string
}

/** IMAP is always implicit TLS on 993: the daemon offers no STARTTLS for IMAP and never plain text. */
export const IMAP_PORT = 993
export const SMTP_PORTS: Record<SMTPSecurity, number> = { implicit: 465, starttls: 587 }

export function emptyDraft(): AccountDraft {
  return { email: '', displayName: '', password: '', imapHost: '', imapPort: IMAP_PORT, smtpHost: '', smtpPort: SMTP_PORTS.implicit, smtpTLS: 'implicit', loginUser: '' }
}

/** Apple's own page on creating an app-specific password, which iCloud needs in place of the Apple Account password. */
export const ICLOUD_PASSWORD_HELP = 'https://support.apple.com/102654'

/** The providers the server signs in to with a password it tests before saving anything; the others use OAuth. */
export function signsInWithPassword(provider: ProviderID | ''): boolean {
  return provider === 'imap' || provider === 'icloud'
}

/**
 * Whether the address is one of Apple's own. A custom domain on iCloud+ cannot
 * be told apart by its name: for those the person chooses iCloud themselves.
 */
export function isICloudAddress(email: string): boolean {
  const domain = email.trim().toLowerCase().split('@').at(-1) ?? ''
  return email.includes('@') && (domain === 'icloud.com' || domain === 'me.com' || domain === 'mac.com')
}

/**
 * The conventional server names for an address, offered until the person
 * types their own. None for Apple's addresses: Apple's servers are not named
 * after the domain (imap.mail.me.com), and the form points those at the iCloud
 * option instead.
 */
export function guessHosts(email: string): { imap: string; smtp: string } {
  const domain = email.trim().toLowerCase().split('@')[1] ?? ''
  if (!/^[a-z0-9.-]+\.[a-z]{2,}$/.test(domain) || isICloudAddress(email)) return { imap: '', smtp: '' }
  return { imap: `imap.${domain}`, smtp: `smtp.${domain}` }
}

/** Switching the SMTP security moves the port along with it, unless the person chose a port of their own. */
export function portForSecurity(current: number, from: SMTPSecurity, to: SMTPSecurity): number {
  return current === SMTP_PORTS[from] || !current ? SMTP_PORTS[to] : current
}

/**
 * The request body. Only fields the person filled are sent: the daemon refuses
 * unknown fields and treats absent ones as "use the default".
 */
export function draftRequest(provider: ProviderID, draft: AccountDraft): AddAccountRequest {
  const body: AddAccountRequest = { email: draft.email.trim(), provider }
  const displayName = draft.displayName.trim()
  if (displayName) body.display_name = displayName
  if (!signsInWithPassword(provider)) return body
  body.password = draft.password
  const loginUser = draft.loginUser.trim()
  if (loginUser) body.login_user = loginUser
  // Apple's servers and ports are the server's to fill in, and it refuses them
  // for iCloud (an "icloud" account pointing elsewhere would be a lie). The
  // login is the address itself, or for an iCloud+ custom domain the
  // account's iCloud address, which is all Apple accepts as a sign-in.
  if (provider === 'icloud') return body
  body.imap_host = draft.imapHost.trim()
  body.imap_port = Number(draft.imapPort) || IMAP_PORT
  body.smtp_host = draft.smtpHost.trim()
  body.smtp_port = Number(draft.smtpPort) || SMTP_PORTS[draft.smtpTLS]
  body.smtp_tls = draft.smtpTLS
  return body
}
