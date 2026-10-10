// What this browser does with a mailbox's keys (docs/key-scheme.md sections
// 8 and 9): make a mailbox's key pair, seal its private key to a person (a
// grant), and open the person's own grant again. No state and no request:
// state/grants.ts and state/mailboxKeys.ts ask the server, and call these in
// between.
//
// A mailbox's private key never leaves the page but sealed in grants. Every
// one made or opened here is the caller's to zero, as soon as its grants are
// sealed; one mailbox key is open at a time. What goes to the server is the
// public half, the namespace and the grants, all base64url. Every refusal is
// a CeremonyError with a code the console translates: a public key or a seal
// id the server served outside its spelling, a recipient key of low order, a
// grant that does not open, and one that opens to a key other than the
// mailbox's public one, are all security: nothing is sealed or sent.

import { fromBase64URL, toBase64URL, type Bytes } from '@thehappieco/kit/bytes'
import { generateKeyPair, type PrivateKey } from '@thehappieco/kit/hpke'
import { CeremonyError } from './errors'
import { isNamespace, isSealID, MAX_EPOCH, MIN_EPOCH, newNamespace, openGrant, sealGrant } from './mailie'

const KEY_LEN = 32
const GRANT_LEN = 88

/**
 * A mailbox's key pair made in this browser, at one epoch: the public half
 * and the namespace, as the server stores them, and the private key, which
 * the caller seals to people and then zeroes.
 */
export interface MailboxKeyMaterial {
  /** base64url of the 32-byte public key. */
  publicKey: string
  namespace: string
  epoch: number
  privateKey: Bytes
}

/** Someone a grant is sealed to, as the server serves them: their seal id, which binds it, and their account public key. */
export interface GrantRecipient { seal_id?: string; public_key?: string }

/** A mailbox's key pair at one epoch as the server holds it: what a grant opens to. */
export interface MailboxPublicKey { epoch?: number; public_key?: string; namespace?: string }

const epochOf = (epoch: unknown): epoch is number => Number.isInteger(epoch) && (epoch as number) >= MIN_EPOCH && (epoch as number) <= MAX_EPOCH

/** A value the server served, decoded in its one spelling; anything else is a security error. */
function bytesOf(text: string | undefined, n: number): Bytes {
  try { return fromBase64URL(text ?? '', n) } catch { throw new CeremonyError('security') }
}

/**
 * newMailboxKey makes a mailbox's key pair: an X25519 pair (the kit's hpke),
 * a fresh namespace for a mailbox's first key (section 8), or the mailbox's
 * own for its next one (section 12.12), at the epoch given.
 */
export async function newMailboxKey(namespace: string = newNamespace(), epoch: number = MIN_EPOCH): Promise<MailboxKeyMaterial> {
  if (!isNamespace(namespace) || !epochOf(epoch)) throw new CeremonyError('security')
  const pair = await generateKeyPair()
  return { publicKey: toBase64URL(pair.publicKey), namespace, epoch, privateKey: pair.privateKey }
}

/**
 * sealTo seals a mailbox's private key to one person (section 9.1), at the
 * mailbox's namespace and epoch, under the seal id and to the public key the
 * server served for them, both checked first. A recipient key of low order,
 * whose grant whoever handed it out could open, is refused. The mailbox key
 * stays the caller's to zero.
 */
export async function sealTo(recipient: GrantRecipient, namespace: string, epoch: number, mailboxKey: Bytes): Promise<string> {
  if (!isSealID(recipient.seal_id) || !isNamespace(namespace) || !epochOf(epoch)) throw new CeremonyError('security')
  const publicKey = bytesOf(recipient.public_key, KEY_LEN)
  try {
    return toBase64URL(await sealGrant(publicKey, namespace, recipient.seal_id, epoch, mailboxKey))
  } catch {
    throw new CeremonyError('security')
  }
}

/**
 * sealAll seals a mailbox key just made to everyone given, in order, and
 * zeroes its private key whatever happens: the grants are all it is for.
 */
export async function sealAll(made: MailboxKeyMaterial, recipients: readonly GrantRecipient[]): Promise<string[]> {
  try {
    const grants: string[] = []
    for (const recipient of recipients) grants.push(await sealTo(recipient, made.namespace, made.epoch, made.privateKey))
    return grants
  } finally {
    made.privateKey.fill(0)
  }
}

/**
 * openOwnGrant opens the person's own grant of a mailbox with their account
 * key, bound to their seal id, at the mailbox key's namespace and epoch, and
 * checks the key it holds against the public key the server holds for the
 * mailbox at that epoch (section 9.2): a grant that does not open, or opens
 * to another key, is a security error. The mailbox key it answers is the
 * caller's to zero.
 */
export async function openOwnGrant(account: PrivateKey, sealID: string, key: MailboxPublicKey, grant: string | undefined): Promise<Bytes> {
  if (!isSealID(sealID) || !isNamespace(key.namespace) || !epochOf(key.epoch)) throw new CeremonyError('security')
  const mailboxPublicKey = bytesOf(key.public_key, KEY_LEN)
  const sealed = bytesOf(grant, GRANT_LEN)
  try {
    return await openGrant(account, key.namespace, sealID, key.epoch, mailboxPublicKey, sealed)
  } catch {
    throw new CeremonyError('security')
  }
}

/** What a person's link sends with the request that creates the mailbox (section 12.11): the first key, and their own grant at epoch 1. */
export interface LinkKey { public_key: string; namespace: string; grant: string }

/**
 * linkKey makes the key of a mailbox about to be linked, a fresh pair and
 * namespace on every attempt, and seals its private key to the linker's own
 * account public key under their seal id at epoch 1: no account private key
 * is needed. A person without an account key has nothing to seal it to:
 * not_enrolled, before anything is made or sent.
 */
export async function linkKey(linker: GrantRecipient): Promise<LinkKey> {
  if (!linker.seal_id || !linker.public_key) throw new CeremonyError('not_enrolled')
  const made = await newMailboxKey()
  const [grant] = await sealAll(made, [linker])
  return { public_key: made.publicKey, namespace: made.namespace, grant: grant! }
}
