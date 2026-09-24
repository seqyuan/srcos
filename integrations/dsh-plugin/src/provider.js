/**
 * The `srcos` resource provider: one address, a stream of metadata frames.
 *
 * dsh's resource model asks a provider for an address's *identity and
 * freshness* — the `file` protocol yields a stat, and the tab type reads the
 * content through its own channel (packages/api/workspace-files/src/client/
 * provider.ts). This provider does the same: each frame carries the gateway's
 * `GET /api/resources?src=` answer, which has the size and mtime a viewer needs
 * to know whether to re-read, plus the `entries` a directory needs.
 *
 * The two rules of the contract, both load-bearing:
 *
 *   - **Failures are frames, not throws.** `{ ok: false, error }` marks the
 *     resource failed while keeping the last value; a throw inside the stream is
 *     a programming error the model lets surface. So every network or HTTP
 *     failure becomes a frame — including the address's own bad grammar, which
 *     is the one failure the gateway never sees.
 *   - **The stream stops when the signal aborts** (the last subscriber or pin
 *     released). Nothing here outlives its subscriber.
 *
 * Freshness: SRCOS has no change feed for files (only the log endpoint has SSE),
 * so this polls. The default interval is deliberately unhurried — a tab that is
 * not being looked at should not hammer the gateway, and every frame it does
 * send re-renders a body.
 */

import { toDsh, toNative } from './address.js'

/** The failure codes this provider declares (mirrored in client.d.ts for TS consumers). */
export const CODE_UNSUPPORTED_ADDRESS = 'srcos/unsupported-address'
export const CODE_UNREACHABLE = 'srcos/unreachable'
export const CODE_MISSING = 'srcos/missing'

/**
 * One failure frame. Shaped as dsh's `RemoteError` is identified — structurally,
 * with the marker and a stable code, because cross-bundle `instanceof` never
 * works (packages/typert/protocol/src/remote-error.ts).
 * @param code - one of this provider's declared codes.
 * @param message - human diagnostic.
 * @param details - structured payload (`{address, status?}`).
 * @returns the error object to place in a frame's `error` slot.
 */
export function srcosFailure(code, message, details) {
  const error = new Error(message)
  error.name = 'SrcosError'
  error.code = code
  error.details = details
  error.isDSHRemoteError = true
  return error
}

/**
 * Build the provider.
 * @param api - a client from `createSrcosApi`.
 * @param options - `pollMs` (default 5000), `sleep` (tests inject one).
 * @returns the provider to register with `ctx.resources.register`.
 */
export function createSrcosProvider(api, options = {}) {
  const pollMs = Number.isFinite(options.pollMs) && options.pollMs > 0 ? options.pollMs : 5000
  const sleep = options.sleep ?? defaultSleep

  /** Wait, but wake immediately when the signal aborts. */
  function defaultSleep(ms, signal) {
    return new Promise((resolve) => {
      if (signal.aborted) return resolve()
      const timer = setTimeout(() => {
        signal.removeEventListener('abort', onAbort)
        resolve()
      }, ms)
      function onAbort() {
        clearTimeout(timer)
        resolve()
      }
      signal.addEventListener('abort', onAbort, { once: true })
    })
  }

  return {
    protocol: 'srcos',

    async *open(address, ctx) {
      const signal = ctx?.signal
      const native = toNative(address)
      if (native === null) {
        yield {
          ok: false,
          error: srcosFailure(
            CODE_UNSUPPORTED_ADDRESS,
            `not an SRCOS resource address: ${String(address)} (expected ${toDsh('srcos://file/<scope>/<path>')})`,
            { address: String(address) },
          ),
        }
        return
      }

      // The freshness token of the last frame we sent. A frame is only worth
      // sending when this changes: an unchanged one would re-render the body for
      // nothing.
      let lastToken
      for (;;) {
        if (signal?.aborted) return
        try {
          const meta = await api.meta(native)
          if (signal?.aborted) return
          const token = `${meta.mtime ?? ''}|${meta.size ?? ''}|${meta.isDir ? 'd' : 'f'}`
          if (token !== lastToken) {
            lastToken = token
            yield {
              ok: true,
              value: {
                address: toDsh(native),
                native,
                scope: meta.scope,
                tool: meta.tool,
                sandboxPath: meta.sandboxPath,
                isDir: Boolean(meta.isDir),
                size: meta.size ?? 0,
                mtime: meta.mtime ?? '',
                mode: meta.mode ?? '',
                viewer: meta.viewer ?? { id: 'text', name: 'Text', kind: 'text' },
                entries: meta.entries ?? [],
              },
            }
          }
        } catch (cause) {
          if (signal?.aborted) return
          const status = cause?.status ?? 0
          yield {
            ok: false,
            error: srcosFailure(
              status === 404 ? CODE_MISSING : CODE_UNREACHABLE,
              cause?.message ?? String(cause),
              { address: native, status },
            ),
          }
          // Keep the stream open: the next poll may succeed (a gateway that was
          // restarting, a file that has not been created yet).
        }
        await sleep(pollMs, signal ?? new AbortController().signal)
      }
    },
  }
}
