/**
 * The dsh client face of the plugin: one resource protocol provider, one tab
 * type, and the body that renders it.
 *
 * Everything this file touches is dsh's published client API, and each use is
 * pinned to the file it was read from, because none of it is stable yet (dsh is
 * a developer preview and says so):
 *
 *   - `ctx.resources.register({protocol, open})`
 *       packages/client/resources/src/client/contract.ts
 *   - `ctx.sidebarRightTabs.register(<SidebarRightTabDefinition>)`
 *       packages/client/ui-sidebar-right/src/client/tab-registry.ts
 *   - `ctx.slots.register({name, key, inject}, Component)` under
 *       `sidebar.right.pane.tab`
 *       packages/client/ui-sidebar-right/src/client/contract/slots.ts
 *   - a body reads `props.useTabInfo().tab.contentId` and
 *     `props.useResource('srcos')(address)`
 *       packages/client/ui-sidebar-documentpreview/src/client/TextPreview.tsx
 *
 * Two deliberate choices:
 *
 *   - **The provider is registered even without configuration.** An unconfigured
 *     plugin then fails *visibly* (every address reports "no gateway URL is
 *     configured") instead of silently doing nothing, which is the difference
 *     between a bug report and a mystery.
 *   - **The credential is an SRCOS agent token, not a browser session.** A dsh
 *     page is not the gateway's origin, so a session cookie would not be sent —
 *     and a program's credential is a token (ADR-019). It is read-scoped, and
 *     revocable from the gateway's `/tokens` page.
 */

import { createElement, useEffect, useState } from 'react'
import { readConfig } from './config.js'
import { basenameOf, childAddress, isSrcosAddress, toDsh } from './address.js'
import { createSrcosApi } from './api.js'
import { createSrcosProvider } from './provider.js'

/** This implementation's identity, and the key its body registers under. */
export const SRCOX_TAB_ID = '@seqyuan/srcos-dsh'
/** The tab kind (what `openTab` names and the page address carries). */
export const SRCOX_KIND = 'srcos'

/** Required browser services: the resource model, the tab registry, the slots. */
export const inject = ['resources', 'sidebarRightTabs', 'slots']

/**
 * The tab type's definition.
 *
 * It claims both spellings — the dsh form the resource model resolves, and the
 * native `srcos://` form a user may paste — at the `extension` band, which is
 * what a type from outside the product is.
 * @returns the definition to register.
 */
export function srcosDefinition() {
  return {
    id: SRCOX_TAB_ID,
    kind: SRCOX_KIND,
    priority: 'extension',
    patterns: ['dsh-resource://srcos/**', 'srcos://**'],
    canOpen: (address) => isSrcosAddress(address),
    title: (address) => basenameOf(address),
    guide: [{
      id: 'srcos-roots',
      order: 100,
      title: () => 'SRCOS 资源',
      description: () => '浏览 SRCOS 网关的 home、工作区和共享数据',
    }],
  }
}

/**
 * The plugin body.
 * @param ctx - the client root context.
 */
export function apply(ctx) {
  const config = readConfig()
  const api = createSrcosApi(config)
  const provider = createSrcosProvider(api, { pollMs: config.pollMs })

  ctx.effect(() => ctx.resources.register(provider), 'srcos-dsh: resource provider')
  ctx.effect(() => ctx.sidebarRightTabs.register(srcosDefinition()), 'srcos-dsh: tab type')
  ctx.effect(() => ctx.slots.register(
    { name: 'sidebar.right.pane.tab', key: SRCOX_TAB_ID, inject: { api } },
    SrcosPane,
  ), 'srcos-dsh: pane body')
}

/**
 * The pane: a directory listing, a text preview, or the reason neither is
 * showing. The roots browser is the same component in page mode — the guide's
 * capsule opens the type with no address, which is what makes the plugin
 * reachable without typing an address first.
 * @param props - composed slot props (`useTabInfo`, `useResource`, and the injected `api`).
 * @returns the rendered pane.
 */
export function SrcosPane(props) {
  const { tab } = props.useTabInfo()
  const address = tab.contentId
  const browse = !isSrcosAddress(address)
  const snapshot = props.useResource('srcos')(browse ? '' : toDsh(address))
  const api = props.api
  const [roots, setRoots] = useState(null)
  const [text, setText] = useState(null)
  const [note, setNote] = useState('')

  // Page mode (opened from the guide): list the roots this token may browse.
  useEffect(() => {
    if (!browse) return
    let alive = true
    api.roots()
      .then((scopes) => { if (alive) setRoots(scopes) })
      .catch((cause) => { if (alive) setNote(cause.message ?? String(cause)) })
    return () => { alive = false }
  }, [browse, api])

  const meta = snapshot.value
  const viewerKind = meta?.viewer?.kind ?? ''
  const readable = meta !== undefined && !meta.isDir
    && (viewerKind === 'text' || viewerKind === 'markdown' || viewerKind === 'table')
  // Reload when the gateway reports the file changed — that is what the
  // metadata frame is *for*.
  const stamp = `${meta?.mtime ?? ''}|${meta?.size ?? ''}`

  useEffect(() => {
    if (!readable) { setText(null); return }
    let alive = true
    api.text(meta.native)
      .then((body) => { if (alive) { setText(body); setNote('') } })
      .catch((cause) => { if (alive) { setText(null); setNote(cause.message ?? String(cause)) } })
    return () => { alive = false }
  }, [readable, meta?.native, stamp, api])

  const open = (child) => props.openResource(child)

  if (browse) {
    if (roots === null) return h('div', { className: 'srcos-note' }, note || '载入 SRCOS 资源…')
    return h('div', { className: 'srcos-pane' }, [
      note !== '' && h('div', { className: 'srcos-note srcos-bad', key: 'note' }, note),
      ...roots.map((scope) => h('button', {
        key: `${scope.kind}:${scope.scope}:${scope.tool ?? ''}`,
        className: 'srcos-row',
        onClick: () => open(scopeAddress(scope)),
      }, [
        h('span', { className: 'srcos-icon', key: 'i' }, scope.kind === 'storage' ? '▤' : '⌂'),
        h('span', { className: 'srcos-name', key: 'n' }, scope.name),
        h('span', { className: 'srcos-meta', key: 'm' }, scope.mode ?? ''),
      ])),
    ])
  }

  if (snapshot.status === 'failed') {
    return h('div', { className: 'srcos-pane' }, [
      h('div', { className: 'srcos-note srcos-bad', key: 'e' }, snapshot.failure?.message ?? '读取失败'),
    ])
  }
  if (meta === undefined) return h('div', { className: 'srcos-note' }, '载入中…')

  if (meta.isDir) {
    return h('div', { className: 'srcos-pane' }, [
      h('div', { className: 'srcos-path', key: 'p' }, meta.sandboxPath),
      ...(meta.entries ?? []).map((entry) => h('button', {
        key: entry.addr ?? entry.name,
        className: 'srcos-row',
        onClick: () => open(toDsh(entry.addr ?? childAddress(address, entry.name))),
      }, [
        h('span', { className: 'srcos-icon', key: 'i' }, entry.isDir ? '▸' : '·'),
        h('span', { className: 'srcos-name', key: 'n' }, entry.name),
        h('span', { className: 'srcos-meta', key: 'm' }, entry.isDir ? '' : humanBytes(entry.size)),
      ])),
    ])
  }

  if (!readable) {
    return h('div', { className: 'srcos-pane' }, [
      h('div', { className: 'srcos-path', key: 'p' }, meta.sandboxPath),
      h('div', { className: 'srcos-note', key: 'n' },
        `这个文件用 ${meta.viewer?.name ?? viewerKind} 查看；在 SRCOS 的 /view 页面打开它，或下载原始文件。`),
      h('a', { className: 'srcos-link', key: 'a', href: api.url(meta.native), target: '_blank', rel: 'noreferrer' }, '原始文件'),
    ])
  }

  return h('div', { className: 'srcos-pane' }, [
    h('div', { className: 'srcos-path', key: 'p' },
      `${meta.sandboxPath} · ${humanBytes(meta.size)}${meta.mode ? ` · ${meta.mode}` : ''}${meta.viewer?.name ? ` · ${meta.viewer.name}` : ''}`),
    note !== '' && h('div', { className: 'srcos-note srcos-bad', key: 'n' }, note),
    h('pre', { className: 'srcos-text', key: 't' }, text ?? '载入中…'),
  ])
}

/** The native address of one browsable root. */
function scopeAddress(scope) {
  return toDsh(`srcos://file/${scope.scope}${scope.tool ? `?tool=${encodeURIComponent(scope.tool)}` : ''}`)
}

/** A byte count for a listing line. */
function humanBytes(n) {
  const size = Number(n) || 0
  if (size >= 1 << 20) return `${(size / (1 << 20)).toFixed(1)} MB`
  if (size >= 1 << 10) return `${(size / (1 << 10)).toFixed(1)} KB`
  return `${size} B`
}

/** `createElement` under a short name: this file is bundled without JSX. */
function h(tag, props, children) {
  return createElement(tag, props, children)
}
