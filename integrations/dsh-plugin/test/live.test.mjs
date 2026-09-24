/**
 * The live test: the plugin's client and provider against a real gateway.
 *
 * It is skipped unless `SRCOS_BASE_URL` and `SRCOS_TOKEN` are set, so `npm test`
 * stays hermetic (no gateway needed, no credential in the repository). Run it
 * with a gateway started from the repository root:
 *
 *   SRCOS_BASE_URL=http://127.0.0.1:30152 SRCOS_TOKEN=srcos_… npm test
 *
 * This is the half of the plugin that *can* be verified without dsh: everything
 * from the address to the frames is real HTTP against the real API.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createSrcosApi } from '../src/api.js'
import { createSrcosProvider } from '../src/provider.js'
import { toDsh } from '../src/address.js'

const base = process.env.SRCOS_BASE_URL ?? ''
const token = process.env.SRCOS_TOKEN ?? ''
const enabled = base !== '' && token !== ''

test('live: an unauthorized token is refused with its status', { skip: !enabled }, async () => {
  const api = createSrcosApi({ baseUrl: base, token: 'srcos_aaaaaaa.bbbb' })
  await assert.rejects(() => api.roots(), (err) => err.status === 401)
})

test('live: roots, a directory, a text file, and a missing resource', { skip: !enabled }, async () => {
  const api = createSrcosApi({ baseUrl: base, token })

  const roots = await api.roots()
  assert.ok(Array.isArray(roots) && roots.length > 0, 'no browsable roots')
  const home = roots.find((r) => r.scope === 'home') ?? roots[0]

  const dir = await api.meta(`srcos://file/${home.scope}`)
  assert.equal(dir.isDir, true)
  assert.ok(Array.isArray(dir.entries) && dir.entries.length > 0, 'the seeded home is empty')
  // Every entry carries its own address: the client never composes one.
  for (const entry of dir.entries) {
    assert.match(entry.addr, /^srcos:\/\/file\//)
  }

  const file = dir.entries.find((e) => !e.isDir && /\.(md|txt)$/.test(e.name))
  assert.ok(file, `no text file in ${home.scope}: ${dir.entries.map((e) => e.name).join(', ')}`)
  const meta = await api.meta(file.addr)
  assert.equal(meta.isDir, false)
  assert.ok(['text', 'markdown', 'table'].includes(meta.viewer.kind), `viewer=${meta.viewer.kind}`)
  const body = await api.text(file.addr)
  assert.ok(body.length > 0, 'the text read came back empty')

  // The provider, over the same two addresses: one ok frame each, then silence.
  const provider = createSrcosProvider(api, { pollMs: 30 })
  const controller = new AbortController()
  const frames = []
  let polls = 0
  setTimeout(() => controller.abort(), 200)
  for await (const frame of provider.open(toDsh(file.addr), { signal: controller.signal })) {
    frames.push(frame)
    polls += 1
    if (polls > 200) break
  }
  assert.equal(frames.length, 1, 'an unchanged file must emit exactly one frame')
  assert.equal(frames[0].ok, true)
  assert.equal(frames[0].value.native, file.addr)
  assert.equal(frames[0].value.sandboxPath, meta.sandboxPath)

  // A missing resource is a failure frame carrying the gateway's own message.
  const missing = createSrcosProvider(api, { pollMs: 30 })
  const gone = new AbortController()
  const failures = []
  const run = (async () => {
    for await (const frame of missing.open(toDsh(`srcos://file/${home.scope}/definitely-not-here`), { signal: gone.signal })) {
      failures.push(frame)
      gone.abort()
    }
  })()
  await run
  assert.equal(failures[0].ok, false)
  assert.match(failures[0].error.message, /not found/i)
})
