import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createSrcosProvider, CODE_MISSING, CODE_UNREACHABLE, CODE_UNSUPPORTED_ADDRESS } from '../src/provider.js'
import { SrcosError } from '../src/api.js'
import { readConfig } from '../src/config.js'

// A fake gateway: each call takes the next queued answer (the last one repeats),
// and the provider's polling is driven by an injected sleep, so the stream is
// deterministic and nothing here waits in real time.

function fakeApi(answers) {
  const calls = []
  return {
    calls,
    async meta(native) {
      calls.push(native)
      const next = answers.length > 1 ? answers.shift() : answers[0]
      if (next instanceof Error) throw next
      return next
    },
  }
}

const meta = (over = {}) => ({
  scope: 'workspace', tool: 'demo', sandboxPath: '/workspace/out', isDir: false,
  size: 10, mtime: 't1', mode: 'rw', viewer: { id: 'text', name: 'Text', kind: 'text' },
  ...over,
})

/**
 * A provider whose sleep aborts the stream after N polls. Driving the end of the
 * stream from the sleep — rather than from a frame count — is what makes "an
 * unchanged poll emits nothing" testable: a helper that waits for the Nth frame
 * would hang on a provider that is behaving correctly.
 */
function streamOf(api, abortAfterPolls) {
  const controller = new AbortController()
  let polls = 0
  const provider = createSrcosProvider(api, {
    sleep: async () => {
      polls += 1
      if (polls >= abortAfterPolls) controller.abort()
    },
  })
  return { provider, controller, polls: () => polls }
}

/** Collect every frame the stream yields, until it ends. */
async function drain(provider, address, controller) {
  const out = []
  for await (const frame of provider.open(address, { signal: controller.signal })) out.push(frame)
  return out
}

test('unchanged polls emit one frame, not one per poll', async () => {
  const api = fakeApi([meta()])
  const { provider, controller, polls } = streamOf(api, 3)
  const got = await drain(provider, 'dsh-resource://srcos/file/workspace/out?tool=demo', controller)

  assert.equal(got.length, 1, 'an unchanged resource must not re-emit')
  assert.ok(polls() >= 3, `the provider must keep polling (polled ${polls()})`)
  assert.equal(got[0].ok, true)
  assert.equal(got[0].value.native, 'srcos://file/workspace/out?tool=demo')
  assert.equal(got[0].value.address, 'dsh-resource://srcos/file/workspace/out?tool=demo')
  assert.equal(got[0].value.sandboxPath, '/workspace/out')
  assert.equal(got[0].value.viewer.kind, 'text')
})

test('a changed mtime yields a new frame', async () => {
  const api = fakeApi([meta(), meta({ mtime: 't2' })])
  const { provider, controller } = streamOf(api, 4)
  const got = await drain(provider, 'srcos://file/workspace/out', controller)
  assert.equal(got.length, 2)
  assert.equal(got[0].value.mtime, 't1')
  assert.equal(got[1].value.mtime, 't2')
})

test('a directory frame carries its entries', async () => {
  const api = fakeApi([meta({ isDir: true, entries: [{ name: 'a.txt', addr: 'srcos://file/home/a.txt', isDir: false }] })])
  const { provider, controller } = streamOf(api, 2)
  const [frame] = await drain(provider, 'srcos://file/home', controller)
  assert.equal(frame.value.isDir, true)
  assert.equal(frame.value.entries[0].addr, 'srcos://file/home/a.txt')
})

test('a 404 is a missing failure frame, and the stream stays open', async () => {
  const api = fakeApi([new SrcosError('not found: /home/a', 404, 'srcos://file/home/a'), meta()])
  const { provider, controller } = streamOf(api, 3)
  const got = await drain(provider, 'srcos://file/home/a', controller)
  assert.equal(got[0].ok, false)
  assert.equal(got[0].error.code, CODE_MISSING)
  assert.equal(got[0].error.details.status, 404)
  assert.equal(got[0].error.isDSHRemoteError, true)
  // A failure does not end the stream: the next poll may succeed.
  assert.equal(got[1].ok, true)
})

test('an unreachable gateway is a failure frame, not a throw', async () => {
  const api = fakeApi([new SrcosError('cannot reach SRCOS at http://gw:1', 0, 'srcos://file/home')])
  const { provider, controller } = streamOf(api, 2)
  const [frame] = await drain(provider, 'srcos://file/home', controller)
  assert.equal(frame.ok, false)
  assert.equal(frame.error.code, CODE_UNREACHABLE)
})

test('a foreign address fails once, with the code that says so', async () => {
  const api = fakeApi([meta()])
  const { provider, controller } = streamOf(api, 2)
  const got = await drain(provider, 'dsh-resource://file/session/s1/a.txt', controller)
  assert.equal(got.length, 1)
  assert.equal(got[0].ok, false)
  assert.equal(got[0].error.code, CODE_UNSUPPORTED_ADDRESS)
  // The gateway was never asked about an address that is not ours.
  assert.equal(api.calls.length, 0)
})

test('an aborted signal ends the stream', async () => {
  const api = fakeApi([meta()])
  const { provider, controller } = streamOf(api, 2)
  const seen = await drain(provider, 'srcos://file/home', controller)
  assert.equal(seen.length, 1)
})

test('readConfig prefers the injected global and falls back to storage', () => {
  const injected = { __SRCOS_DSH_CONFIG__: { baseUrl: 'http://gw:30152/', token: 'srcos_x' } }
  const cfg = readConfig(injected)
  assert.equal(cfg.baseUrl, 'http://gw:30152') // the trailing slash is trimmed
  assert.equal(cfg.token, 'srcos_x')
  assert.equal(cfg.pollMs, 5000)

  const stored = {
    localStorage: { getItem: (k) => (k === 'srcos-dsh' ? JSON.stringify({ baseUrl: 'http://b', pollMs: 100 }) : null) },
  }
  const fromStorage = readConfig(stored)
  assert.equal(fromStorage.baseUrl, 'http://b')
  assert.equal(fromStorage.pollMs, 100)

  // Nothing configured: empty, never a throw.
  assert.deepEqual(readConfig({ localStorage: { getItem: () => '{not json' } }), { baseUrl: '', token: '', pollMs: 5000 })
})
