import { reactive } from 'vue'

/**
 * Where the event stream stands (state/live.ts): not running, connecting,
 * open, waiting to reconnect, or given up because the credential was refused.
 */
export type StreamPhase = 'off' | 'connecting' | 'open' | 'retrying' | 'stopped'

/**
 * Whether the daemon answers at all, and whether the event stream is open.
 *
 * Any HTTP answer, even an error, proves the server is there, and only a
 * failure to get one (network down, daemon stopped, timeout) says otherwise.
 * The event stream is the console's one standing connection: while it is
 * open the server is certainly there, and when it drops without an answer the
 * next attempt to reopen it says so.
 */
export const server = reactive<{ reachable: boolean; stream: StreamPhase }>({ reachable: true, stream: 'off' })

export function markReachable(reachable: boolean): void {
  if (server.reachable !== reachable) server.reachable = reachable
}

export function setStream(phase: StreamPhase): void {
  if (server.stream !== phase) server.stream = phase
}

/**
 * How far the daemon's clock is from this browser's, in milliseconds, as the
 * Date of its latest answer says (to the second, plus the trip): what turns
 * a time the server wrote, such as a session's step-up time, into one this
 * page can compare with now. Zero until an answer carries a Date.
 */
let serverOffsetMS = 0

/** noteServerDate takes the daemon's clock from an answer's Date header; one it cannot read changes nothing. */
export function noteServerDate(header: string | null, localNow = Date.now()): void {
  if (!header) return
  const at = Date.parse(header)
  if (Number.isFinite(at)) serverOffsetMS = at - localNow
}

/** serverNow is now by the daemon's clock, in milliseconds, as far as this page can tell. */
export function serverNow(localNow = Date.now()): number {
  return localNow + serverOffsetMS
}
