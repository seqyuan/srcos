/**
 * The packaging contract, pinned without dsh.
 *
 * dsh's client module system reads a package's `exports["./client"]` **as bytes**,
 * serves them at `/plugins/<id>/client.js`, and requires the file to register
 * itself with `window.__ModuleLoader__.load({id, factory})` whose factory returns
 * a module exposing `apply` and `inject`
 * (packages/client/modules/src/client/index.ts, `clientExportOf` + `bootInjections`).
 *
 * None of that needs dsh installed to check: this test loads the built bundle
 * into a stand-in loader and asserts the face dsh will look for. It is what makes
 * `make dsh-plugin` meaningful on a machine without a dsh profile.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, existsSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const root = join(here, '..')
const pkg = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'))

test('the manifest declares the client role the way dsh reads it', () => {
  assert.equal(pkg.dsh?.manifestVersion, 1)
  assert.equal(pkg.dsh?.client?.platform, 'web')
  // `inject` is informational (the real requirement is the Cordis service list
  // exported by the bundle), but it must name the packages this plugin leans on.
  for (const name of ['@deepseek-ai/dsh-client-resources', '@deepseek-ai/dsh-client-ui-sidebar-right']) {
    assert.ok(pkg.dsh.client.inject.includes(name), `dsh.client.inject is missing ${name}`)
  }
  // A local/private package would be skipped by the roster.
  assert.notEqual(pkg.private, true)
})

test('exports["./client"] resolves to the built bundle', () => {
  const entry = pkg.exports?.['./client']
  // dsh accepts a string or an object with a string `default`.
  const file = typeof entry === 'string' ? entry : entry?.default
  assert.equal(typeof file, 'string', 'exports["./client"] must carry a string default')
  assert.ok(existsSync(join(root, 'lib/client.js')), 'lib/client.js is missing: run `npm run build`')
  assert.equal(file, './lib/client.js')
})

test('the built bundle registers with the loader and exposes apply + inject', () => {
  // Rebuild, so the assertion is about the current source, not a stale file.
  execFileSync(process.execPath, [join(root, 'build.mjs')], { stdio: 'ignore' })
  const source = readFileSync(join(root, 'lib/client.js'), 'utf8')
  assert.ok(source.startsWith('window.__ModuleLoader__.load('), 'the bundle must self-register')

  let registration
  globalThis.window = { __ModuleLoader__: { load: (r) => { registration = r } } }
  // eslint-disable-next-line no-eval
  eval(source)
  assert.equal(registration.id, pkg.name)

  const face = registration.factory((specifier) => {
    if (specifier === 'react') {
      return { createElement: () => {}, useEffect: () => {}, useState: () => {} }
    }
    throw new Error(`unexpected external request: ${specifier}`)
  })
  assert.equal(typeof face.apply, 'function')
  assert.deepEqual(face.inject, ['resources', 'sidebarRightTabs', 'slots'])
  assert.equal(typeof face.SrcosPane, 'function')

  // The protocol claim: exactly the two spellings, and nothing else.
  const definition = face.srcosDefinition()
  assert.equal(definition.id, pkg.name)
  assert.equal(definition.kind, 'srcos')
  assert.equal(definition.priority, 'extension')
  assert.equal(definition.canOpen('dsh-resource://srcos/file/home/a.txt'), true)
  assert.equal(definition.canOpen('srcos://file/home/a.txt'), true)
  assert.equal(definition.canOpen('dsh-resource://file/session/s1/a.txt'), false)
  assert.equal(definition.title('srcos://file/home/notes.md'), 'notes.md')
  assert.ok(Array.isArray(definition.guide) && definition.guide.length > 0, 'no guide entry: the plugin would be unreachable')

  // `apply` must register through the context it is given, not by reaching into
  // globals — that is what the inject list is for.
  const registered = { providers: [], tabs: [], slots: [] }
  const ctx = {
    effect: (fn) => { fn(); return () => {} },
    resources: { register: (p) => { registered.providers.push(p); return () => {} } },
    sidebarRightTabs: { register: (d) => { registered.tabs.push(d); return () => {} } },
    slots: { register: (spec, component) => { registered.slots.push({ spec, component }); return () => {} } },
  }
  face.apply(ctx)
  assert.equal(registered.providers[0].protocol, 'srcos')
  assert.equal(typeof registered.providers[0].open, 'function')
  assert.equal(registered.tabs[0].kind, 'srcos')
  assert.equal(registered.slots[0].spec.name, 'sidebar.right.pane.tab')
  assert.equal(registered.slots[0].spec.key, pkg.name)
  assert.equal(registered.slots[0].component, face.SrcosPane)
})
