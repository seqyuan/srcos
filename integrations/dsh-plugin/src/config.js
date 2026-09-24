/**
 * Where the plugin's configuration comes from.
 *
 * dsh hands a client plugin its configuration as a page global
 * (`__DSH_<NAME>_CONFIG__` — see ui-sidebar-documentpreview's `apply`), which the
 * host fills from the profile's settings. When a profile declares none, a JSON
 * blob in `localStorage` under {@link CONFIG_STORAGE_KEY} is accepted as well, so
 * a user can try the plugin without editing the profile first.
 *
 * The token is *not* hidden from the page: it is the user's own read-scoped
 * credential, it lives in the browser for exactly as long as they use it, and it
 * is revocable from the gateway's `/tokens` page. What is deliberately *not*
 * done: putting it in a URL (query strings end up in history and proxy logs).
 */

/** The global dsh injects when the profile declares plugin config. */
export const CONFIG_GLOBAL = '__SRCOS_DSH_CONFIG__'
/** Where a user pastes configuration when dsh injects none. */
export const CONFIG_STORAGE_KEY = 'srcos-dsh'
/** The poll interval when the configuration names none. */
export const DEFAULT_POLL_MS = 5000

/**
 * Read the plugin's configuration.
 * @param scope - the global object (injectable for tests).
 * @returns `{baseUrl, token, pollMs}`; empty strings mean "not configured".
 */
export function readConfig(scope = globalThis) {
  const from = (value) => ({
    // A trailing slash would double in every path this is concatenated with.
    baseUrl: String(value?.baseUrl ?? '').replace(/\/+$/, ''),
    token: String(value?.token ?? ''),
    pollMs: Number(value?.pollMs) > 0 ? Number(value.pollMs) : DEFAULT_POLL_MS,
  })
  const injected = scope?.[CONFIG_GLOBAL]
  if (injected && typeof injected === 'object') return from(injected)
  try {
    const raw = scope?.localStorage?.getItem(CONFIG_STORAGE_KEY)
    if (raw) return from(JSON.parse(raw))
  } catch {
    // Unreadable storage, or malformed JSON in it, is the same as unconfigured:
    // the pane then says so instead of failing to load.
  }
  return from({})
}
