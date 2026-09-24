/**
 * The address mapping between SRCOS and dsh.
 *
 * dsh resource addresses are `dsh-resource://<protocol>/…`, and the host names
 * the protocol (packages/client/resources/src/client/resources.ts, `protocolOf`).
 * SRCOS resource addresses are `srcos://<provider>/<scope>/<path>`, where the
 * host names the *provider* (docs/roadmap.md ADR-011/016).
 *
 * So the two grammars line up one level apart, and the mapping is a prefix
 * substitution in both directions:
 *
 *   srcos://file/workspace/out/x.txt?tool=demo
 *     ⇄ dsh-resource://srcos/file/workspace/out/x.txt?tool=demo
 *
 * That is the whole point of the "road D insurance": an adapter in either
 * direction is a string rewrite, never a translation. Keeping the SRCOS provider
 * segment (rather than dropping `file`) means a future SRCOS provider
 * (`artifact`, …) needs no change here.
 *
 * Nothing in this module touches the network or the DOM, so it is the part of
 * the plugin a Node test can exercise directly.
 */

/** The dsh scheme every resource address uses. */
export const DSH_SCHEME = 'dsh-resource'
/** dsh's protocol key for this plugin (the host of a `dsh-resource://` URL). */
export const PROTOCOL = 'srcos'
/** SRCOS's own scheme (README「资源查看器」). */
export const NATIVE_SCHEME = 'srcos'
/** The only SRCOS provider implemented today. */
export const NATIVE_PROVIDER = 'file'

const DSH_PREFIX = `${DSH_SCHEME}://${PROTOCOL}/`
const DSH_PREFIX_BARE = `${DSH_SCHEME}://${PROTOCOL}`
const NATIVE_PREFIX = `${NATIVE_SCHEME}://`

/** Is this a native SRCOS address (`srcos://…`)? */
export function isNative(address) {
  return typeof address === 'string' && address.startsWith(NATIVE_PREFIX)
}

/** Is this a dsh resource address for our protocol (`dsh-resource://srcos/…`)? */
export function isDsh(address) {
  return typeof address === 'string'
    && (address.startsWith(DSH_PREFIX) || address === DSH_PREFIX_BARE)
}

/** Is this an SRCOS resource address in either spelling? */
export function isSrcosAddress(address) {
  return isNative(address) || isDsh(address)
}

/**
 * The dsh spelling of an SRCOS address.
 * @param address - a `srcos://…` (or already-`dsh-resource://srcos/…`) address.
 * @returns the dsh address, or `null` when the input names no SRCOS resource.
 */
export function toDsh(address) {
  if (isDsh(address)) return address
  if (!isNative(address)) return null
  return DSH_PREFIX + address.slice(NATIVE_PREFIX.length)
}

/**
 * The SRCOS spelling of a dsh address.
 * @param address - a `dsh-resource://srcos/…` (or already-`srcos://…`) address.
 * @returns the native address, or `null` when the input belongs to another protocol.
 */
export function toNative(address) {
  if (isNative(address)) return address
  if (!isDsh(address)) return null
  if (address === DSH_PREFIX_BARE) return NATIVE_PREFIX
  return NATIVE_PREFIX + address.slice(DSH_PREFIX.length)
}

/**
 * Split an SRCOS address into the pieces the UI needs (a title, a breadcrumb, a
 * child address). Deliberately loose: it never validates the grammar, because
 * the SRCOS API is the authority on what an address means — this only reads the
 * shape for display.
 * @param address - an address in either spelling.
 * @returns `{ native, scope, path, tool, query }`, or `null` when unrecognised.
 */
export function split(address) {
  const native = toNative(address)
  if (native === null) return null
  const rest = native.slice(NATIVE_PREFIX.length)
  const q = rest.indexOf('?')
  const query = q >= 0 ? rest.slice(q + 1) : ''
  const bodyPath = q >= 0 ? rest.slice(0, q) : rest
  const segments = bodyPath.split('/')
  const provider = segments.shift() ?? ''
  const scope = segments.shift() ?? ''
  const path = segments.join('/')
  let tool = ''
  for (const pair of query.split('&')) {
    const [k, v] = pair.split('=')
    if (k === 'tool' && v) tool = decodeURIComponent(v)
  }
  return { native, provider, scope, path, tool, query }
}

/**
 * The last path segment of an address, for a tab title.
 * @param address - an address in either spelling.
 * @returns the decoded basename, or the scope when the address is a scope root.
 */
export function basenameOf(address) {
  const parts = split(address)
  if (parts === null) return String(address)
  const segments = parts.path.split('/').filter(Boolean)
  const last = segments.length > 0 ? segments[segments.length - 1] : parts.scope
  try {
    return decodeURIComponent(last)
  } catch {
    return last
  }
}

/**
 * The address of one entry inside a directory address, keeping the query (the
 * workspace's `?tool=` is part of the scope's identity, not of the path).
 * @param address - a directory address in either spelling.
 * @param name - one entry's name.
 * @returns the child address, in the same spelling as the input.
 */
export function childAddress(address, name) {
  const parts = split(address)
  if (parts === null) return address
  const path = parts.path === '' ? name : `${parts.path}/${name}`
  const native = `${NATIVE_PREFIX}${NATIVE_PROVIDER}/${parts.scope}/${path}`
  const withQuery = parts.tool === '' ? native : `${native}?tool=${encodeURIComponent(parts.tool)}`
  return isDsh(address) ? DSH_PREFIX + withQuery.slice(NATIVE_PREFIX.length) : withQuery
}
