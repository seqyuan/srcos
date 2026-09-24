/**
 * The types this plugin publishes.
 *
 * The `srcos` protocol's value is what `GET /api/resources?src=` answers, plus
 * the two spellings of the address. A TypeScript consumer (another dsh plugin
 * that wants to show an SRCOS resource) declares it by naming the protocol:
 * `useResource<'srcos'>(address)`.
 *
 * The failure codes are declared here because dsh's failure vocabulary is
 * merge-extensible and discrimination is always by `code` — a code that is not
 * declared would be a stringly-typed surprise for the consumer
 * (packages/typert/protocol/src/types.ts).
 */

import type {} from '@deepseek-ai/dsh-client-resources/client'
import type {} from '@deepseek-ai/dsh-typert-protocol'

/** One directory entry, as the gateway describes it. */
export interface SrcosEntry {
  readonly name: string
  /** The sandbox path (the contract spelling a tool receives). */
  readonly path: string
  /** The path inside the scope — what an address is built from. */
  readonly rel: string
  /** The entry's own `srcos://` address, so a caller never composes one. */
  readonly addr: string
  readonly isDir: boolean
  readonly size?: number
  readonly mtime?: string
}

/** The viewer the gateway claims for a resource. */
export interface SrcosViewer {
  readonly id: string
  readonly name: string
  readonly kind: string
}

/** One SRCOS resource: identity, freshness, and (for a directory) its children. */
export interface SrcosResourceValue {
  /** The dsh spelling of the address (`dsh-resource://srcos/…`). */
  readonly address: string
  /** The SRCOS spelling (`srcos://…`) — what the REST API takes as `src`. */
  readonly native: string
  readonly scope: string
  readonly tool?: string
  readonly sandboxPath: string
  readonly isDir: boolean
  readonly size: number
  readonly mtime: string
  readonly mode: string
  readonly viewer: SrcosViewer
  readonly entries: readonly SrcosEntry[]
}

declare module '@deepseek-ai/dsh-client-ui-slots' {
  interface ResourceProtocolMap {
    srcos: SrcosResourceValue
  }
}

declare module '@deepseek-ai/dsh-typert-protocol' {
  interface RemoteErrorDetailsMap {
    /** The address is not an SRCOS resource address; raised by the client provider. */
    'srcos/unsupported-address': { readonly address: string }
    /** The gateway could not be reached, or answered with an error status. */
    'srcos/unreachable': { readonly address: string; readonly status: number }
    /** The gateway answered 404: the resource is not there, or not readable. */
    'srcos/missing': { readonly address: string; readonly status: number }
  }
}
