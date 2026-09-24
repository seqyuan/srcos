/**
 * The SRCOS REST client the plugin reads through.
 *
 * It talks to exactly four endpoints, all of them documented in the gateway's
 * README, and none of them dsh-specific:
 *
 *   GET /api/resources                     → the roots a user may browse
 *   GET /api/resources?src=<address>       → one resource's metadata (+ entries)
 *   GET /api/resources/raw?src=<address>   → its bytes (text, images, PDF)
 *   GET /api/resources/html?src=<address>  → user HTML, sandboxed (CSP)
 *
 * Authentication is an **agent token** (`Authorization: Bearer srcos_…`), the
 * same credential an MCP client uses: the plugin is a program, and a browser
 * session cookie belongs to a browser on the gateway's own origin, which this
 * plugin is not. The token is read-scoped and revocable from `/tokens`.
 *
 * `baseUrl` is required and must be explicit. It cannot be inferred from
 * location: when dsh itself is proxied *through* SRCOS, the page's origin serves
 * dsh's own `/api`, not the gateway's.
 *
 * No dsh import here either — the client is plain `fetch`, so a Node test can run
 * it against a real gateway.
 */

/** One failure from the SRCOS API, with the status that produced it. */
export class SrcosError extends Error {
  /**
   * @param message - human diagnostic (the gateway's `error` field when it sent one).
   * @param status - HTTP status, or 0 for a transport failure.
   * @param address - the resource address the call was about, when it had one.
   */
  constructor(message, status, address) {
    super(message)
    this.name = 'SrcosError'
    this.status = status
    this.address = address
  }
}

/**
 * Build the API client.
 * @param options - `baseUrl` (required), `token` (read scope), `fetchImpl` (tests).
 * @returns the client's four calls.
 */
export function createSrcosApi(options = {}) {
  const base = String(options.baseUrl ?? '').replace(/\/+$/, '')
  const token = options.token ?? ''
  const doFetch = options.fetchImpl ?? globalThis.fetch?.bind(globalThis)

  async function call(path, address) {
    if (base === '') {
      throw new SrcosError('no SRCOS gateway URL is configured', 0, address)
    }
    if (typeof doFetch !== 'function') {
      throw new SrcosError('no fetch implementation is available', 0, address)
    }
    const headers = {}
    if (token !== '') headers.Authorization = `Bearer ${token}`
    let res
    try {
      res = await doFetch(base + path, { headers })
    } catch (cause) {
      throw new SrcosError(`cannot reach SRCOS at ${base}: ${cause?.message ?? cause}`, 0, address)
    }
    if (!res.ok) {
      // The gateway answers JSON `{error}` on every refusal; a proxy in front of
      // it may not, so fall back to the status text.
      let message = `${res.status} ${res.statusText}`
      try {
        const body = await res.json()
        if (body && typeof body.error === 'string') message = body.error
      } catch {
        /* not JSON: keep the status line */
      }
      throw new SrcosError(message, res.status, address)
    }
    return res
  }

  return {
    /** The roots this token's user may browse: home, workspaces, storages. */
    async roots() {
      const res = await call('/api/resources')
      const body = await res.json()
      return body.scopes ?? []
    },

    /**
     * One resource's metadata.
     * @param native - a `srcos://…` address.
     * @returns the gateway's answer (scope, paths, size, mtime, viewer, entries).
     */
    async meta(native) {
      const res = await call(`/api/resources?src=${encodeURIComponent(native)}`, native)
      return res.json()
    },

    /**
     * A resource's bytes as text.
     * @param native - a `srcos://…` address.
     * @returns the body as a string (the gateway caps text reads on its side).
     */
    async text(native) {
      const res = await call(`/api/resources/raw?src=${encodeURIComponent(native)}`, native)
      return res.text()
    },

    /**
     * The URL a browser element can load directly (`<img>`, `<iframe>`).
     *
     * It carries no credential: the gateway refuses an unauthenticated read, so
     * this is only usable when the gateway is reachable with a session or a
     * proxy in front. The pane therefore prefers text reads and treats this as a
     * link, never as a silent fallback. The token is *not* put in the URL: a
     * query string ends up in history and proxy logs.
     * @param native - a `srcos://…` address.
     * @param kind - `raw` (default) or `html` (the sandboxed endpoint).
     * @returns the absolute URL.
     */
    url(native, kind = 'raw') {
      return `${base}/api/resources/${kind}?src=${encodeURIComponent(native)}`
    },
  }
}
