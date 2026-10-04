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
