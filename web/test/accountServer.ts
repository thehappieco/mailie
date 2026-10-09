// An in-memory server half of the key scheme's ceremonies
// (docs/key-scheme.md section 12, internal/auth/accountkeys.go), for the
// specs of the console's half: it stores what a browser sends, checks only
// what the real server checks (shapes, tickets, the secrets' equality where
// the real one compares hashes), and records every request so a spec can say
// what was, and was not, sent. Not a spec itself.
import { toBase64URL } from '@thehappieco/kit/bytes'
import type { User } from '../src/api/types'
import { json, failure, now, serve } from './support'

export const DEFAULT_KDF = { alg: 'argon2id', m: 65536, t: 3, p: 1 }

/** A salt made from a label: 16 bytes, base64url, as the server answers one. */
export function saltOf(label: string): string {
  const bytes = new Uint8Array(16)
  new TextEncoder().encode(label).forEach((byte, i) => { bytes[i % 16] = (bytes[i % 16]! * 31 + byte) & 0xff })
  return toBase64URL(bytes)
}

const normalise = (email: string) => email.trim().toLowerCase()
/** An address's target: the salt the server's salt key gives it. */
export const targetOf = (email: string) => saltOf(`target|${normalise(email)}`)

let serial = 0
const random = (n = 32) => toBase64URL(crypto.getRandomValues(new Uint8Array(n)))

/** A person as the server stores them. legacyPassword: someone who signed up before the key scheme. */
export interface Stored {
  id: string
  email: string
  name: string
  sealID: string
  publicKey?: string
  salt: string
  kdf: typeof DEFAULT_KDF
  authKey: string
  passwordWrap: string
  recoveryWrap: string
  recoveryProof: string
  legacyPassword?: string
}

/**
 * A ticket: a password change's and a re-derivation's are bound to a session
 * (token) and to the auth key that earned it, a recovery's to the recovery
 * proof that opened it (proof); the upgrade's enrolment to neither.
 */
interface Ticket { purpose: 'password' | 'rederive' | 'recover' | 'enrol'; userID: string; token?: string; proof?: string; salt: string }
interface Session { userID: string; authenticatedAt: number; ended?: boolean }

export interface Call { path: string; body: Record<string, unknown>; token: string }

export function accountServer() {
  const people = new Map<string, Stored>()
  const sessions = new Map<string, Session>()
  const tickets = new Map<string, Ticket>()
  const invites = new Map<string, { email: string; sealID: string; used: boolean }>()
  const resets = new Map<string, string>()
  const calls: Call[] = []
  /**
   * The server's clock: skew is how many seconds it is behind this
   * browser's (negative: ahead), which every answer's Date says; and what it
   * answers next for a step-up time of a new session, which a spec moves to
   * make one old.
   */
  const clock = { skew: 0, authenticatedAt: () => serverNow() }
  const serverNow = () => now() - clock.skew

  const userOf = (p: Stored): User => ({
    id: p.id, email: p.email, name: p.name, role: 'member', created_at: 1_790_000_000, has_password: true, seal_id: p.sealID,
    ...(p.publicKey ? { public_key: p.publicKey } : {}),
  })
  const byID = (id: string) => [...people.values()].find(p => p.id === id)!
  function open(p: Stored) {
    const token = random()
    sessions.set(token, { userID: p.id, authenticatedAt: clock.authenticatedAt() })
    return { token, expires_at: now() + 14 * 86_400, authenticated_at: sessions.get(token)!.authenticatedAt, user: userOf(p) }
  }
  function endAll(userID: string) { for (const s of sessions.values()) if (s.userID === userID) s.ended = true }
  function ticket(t: Ticket): string { const id = random(); tickets.set(id, t); return id }
  function take(id: unknown, ...purposes: Ticket['purpose'][]): Ticket | undefined {
    const t = tickets.get(String(id))
    if (!t || !purposes.includes(t.purpose)) return undefined
    tickets.delete(String(id))
    return t
  }
  function enrolFrom(p: Stored, body: Record<string, unknown>, salt: string) {
    Object.assign(p, {
      publicKey: body.public_key as string, salt, kdf: body.kdf as typeof DEFAULT_KDF, authKey: body.auth_key as string,
      passwordWrap: body.password_wrap as string, recoveryWrap: body.recovery_wrap as string, recoveryProof: body.recovery_proof as string,
      legacyPassword: undefined,
    })
  }

  /**
   * What happens while an answer is on its way, by path: run once, after the
   * server answered and before the page has the answer.
   */
  const inFlight = new Map<string, () => unknown>()
  /** What the server answers instead, once, by path, before it does anything: a refusal (too many attempts, say). */
  const refuseNext = new Map<string, () => Response>()
  const fetch = serve(async request => {
    const refusal = refuseNext.get(request.path)
    refuseNext.delete(request.path)
    if (refusal) calls.push({ path: request.path, body: (request.body ?? {}) as Record<string, unknown>, token: request.token })
    const response = refusal ? refusal() : answer(request)
    response.headers.set('Date', new Date(serverNow() * 1000).toUTCString())
    const meanwhile = inFlight.get(request.path)
    inFlight.delete(request.path)
    await meanwhile?.()
    return response
  })
  function answer({ path, body: raw, token }: { path: string; body?: unknown; token: string }): Response {
    const body = (raw ?? {}) as Record<string, unknown>
    calls.push({ path, body, token })
    const session = sessions.get(token)
    const signedIn = session && !session.ended ? byID(session.userID) : undefined
    const person = people.get(normalise(String(body.email ?? '')))
    switch (path) {
      case '/v1/auth/challenge':
        if (person && !person.legacyPassword) return json({ salt: person.salt, kdf: person.kdf })
        return json({ salt: targetOf(String(body.email)), kdf: DEFAULT_KDF, ...(person?.legacyPassword ? { upgrade: true } : {}) })
      case '/v1/auth/login': {
        if (!person || person.legacyPassword || body.auth_key !== person.authKey) return failure('unauthorized', 401)
        const reply = open(person)
        const target = targetOf(person.email)
        const rederive = person.salt === target ? undefined : { salt: target, kdf: DEFAULT_KDF, ticket: ticket({ purpose: 'rederive', userID: person.id, token: reply.token, proof: person.authKey, salt: target }) }
        return json({ ...reply, password_wrap: person.passwordWrap, ...(rederive ? { rederive } : {}) })
      }
      case '/v1/auth/signup/open': {
        const invite = invites.get(String(body.invite))
        if (!invite || invite.used || invite.email !== normalise(String(body.email))) return failure('not_authorized', 403)
        return json({ salt: targetOf(invite.email), kdf: DEFAULT_KDF, seal_id: invite.sealID })
      }
      case '/v1/auth/signup': {
        const invite = invites.get(String(body.invite))
        if (!invite || invite.used || invite.email !== normalise(String(body.email))) return failure('not_authorized', 403)
        if (body.seal_id !== invite.sealID) return failure('conflict', 409)
        invite.used = true
        const p: Stored = { id: `usr_${String(++serial).padStart(16, '0')}`, email: invite.email, name: String(body.name), sealID: invite.sealID, salt: '', kdf: DEFAULT_KDF, authKey: '', passwordWrap: '', recoveryWrap: '', recoveryProof: '' }
        enrolFrom(p, body, targetOf(invite.email))
        people.set(p.email, p)
        return json(open(p), 201)
      }
      case '/v1/auth/reset/open': {
        const email = resets.get(String(body.reset))
        if (!email || email !== normalise(String(body.email))) return failure('not_authorized', 403)
        return json({ salt: targetOf(email), kdf: DEFAULT_KDF, seal_id: people.get(email)!.sealID })
      }
      case '/v1/auth/reset': {
        const email = resets.get(String(body.reset))
        if (!email || email !== normalise(String(body.email))) return failure('not_authorized', 403)
        resets.delete(String(body.reset))
        const p = people.get(email)!
        enrolFrom(p, body, targetOf(email))
        endAll(p.id)
        return json(open(p))
      }
      case '/v1/auth/recover/open':
        if (!person || person.legacyPassword || body.recovery_proof !== person.recoveryProof) return failure('unauthorized', 401)
        return json({
          seal_id: person.sealID, public_key: person.publicKey, recovery_wrap: person.recoveryWrap, salt: targetOf(person.email), kdf: DEFAULT_KDF,
          ticket: ticket({ purpose: 'recover', userID: person.id, proof: person.recoveryProof, salt: targetOf(person.email) }),
        })
      case '/v1/auth/recover/finish': {
        // Only with the proof that opened the recovery, and the ticket stays its own otherwise.
        if (tickets.get(String(body.ticket))?.proof !== body.current_recovery_proof) return failure('not_authorized', 403)
        const t = take(body.ticket, 'recover')
        if (!t) return failure('not_authorized', 403)
        const p = byID(t.userID)
        Object.assign(p, { salt: t.salt, authKey: body.auth_key, passwordWrap: body.password_wrap, recoveryWrap: body.recovery_wrap, recoveryProof: body.recovery_proof })
        endAll(p.id)
        return new Response(null, { status: 204 })
      }
      case '/v1/auth/upgrade/login':
        if (!person?.legacyPassword || body.password !== person.legacyPassword) return failure('unauthorized', 401)
        return json({ ticket: ticket({ purpose: 'enrol', userID: person.id, salt: targetOf(person.email) }), seal_id: person.sealID, salt: targetOf(person.email), kdf: DEFAULT_KDF })
      case '/v1/auth/upgrade/enrol': {
        const t = take(body.ticket, 'enrol')
        if (!t) return failure('not_authorized', 403)
        const p = byID(t.userID)
        enrolFrom(p, body, t.salt)
        endAll(p.id)
        return json(open(p))
      }
    }
    if (!signedIn) return failure('unauthorized', 401)
    switch (path) {
      case '/v1/auth/me':
        return json({ user: userOf(signedIn), session: { id: 'ses_0000000000000001', created_at: now(), expires_at: now() + 86_400, authenticated_at: session!.authenticatedAt } })
      case '/v1/auth/logout':
        session!.ended = true
        return new Response(null, { status: 204 })
      case '/v1/auth/password/begin':
        if (body.current_auth_key !== signedIn.authKey) return failure('not_authorized', 403)
        return json({ password_wrap: signedIn.passwordWrap, salt: targetOf(signedIn.email), kdf: DEFAULT_KDF, ticket: ticket({ purpose: 'password', userID: signedIn.id, token, proof: signedIn.authKey, salt: targetOf(signedIn.email) }) })
      case '/v1/auth/password/finish': {
        // The ticket finishes only with the auth key that earned it, and stays its own otherwise.
        const held = tickets.get(String(body.ticket))
        if (!held || held.token !== token || held.proof !== body.current_auth_key) return failure('not_authorized', 403)
        const t = take(body.ticket, 'password', 'rederive')
        if (!t) return failure('not_authorized', 403)
        Object.assign(signedIn, { salt: t.salt, authKey: body.auth_key, passwordWrap: body.password_wrap })
        if (t.purpose === 'rederive') return new Response(null, { status: 204 })
        const stepUp = session!.authenticatedAt
        endAll(signedIn.id)
        const reply = open(signedIn)
        sessions.get(reply.token)!.authenticatedAt = stepUp
        return json({ ...reply, authenticated_at: stepUp })
      }
      case '/v1/auth/stepup':
        if (body.auth_key !== signedIn.authKey) return failure('not_authorized', 403)
        session!.authenticatedAt = serverNow()
        return json({ authenticated_at: session!.authenticatedAt })
      case '/v1/auth/recovery':
        // The current auth key, in this request: the session alone sets no secret.
        if (body.current_auth_key !== signedIn.authKey) return failure('not_authorized', 403)
        Object.assign(signedIn, { recoveryWrap: body.recovery_wrap, recoveryProof: body.recovery_proof })
        for (const [id, t] of tickets) if (t.purpose === 'recover' && t.userID === signedIn.id) tickets.delete(id)
        return new Response(null, { status: 204 })
    }
    return failure('not_found', 404)
  }

  return {
    people, sessions, calls, fetch, clock, inFlight, refuseNext,
    /** An invitation for an address, whose seal id the server draws now. */
    invite(email: string): string { const code = random(); invites.set(code, { email: normalise(email), sealID: crypto.randomUUID(), used: false }); return code },
    /** A reset link for a person. */
    reset(email: string): string { const code = random(); resets.set(code, normalise(email)); return code },
    /** The paths asked, in order. */
    paths: () => calls.map(call => call.path),
  }
}
