import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  basenameOf, childAddress, isDsh, isNative, isSrcosAddress, split, toDsh, toNative,
} from '../src/address.js'

// The mapping is the "road D insurance" (ADR-016): an adapter in either
// direction must be a string rewrite, so these tests pin the exact strings.

test('a native address maps to the dsh spelling and back', () => {
  const native = 'srcos://file/workspace/out/x.txt?tool=demo'
  const dsh = 'dsh-resource://srcos/file/workspace/out/x.txt?tool=demo'
  assert.equal(toDsh(native), dsh)
  assert.equal(toNative(dsh), native)
})

test('the provider segment survives the rewrite', () => {
  // Keeping `file` means a future SRCOS provider needs no change here.
  assert.equal(toDsh('srcos://artifact/run1/metrics.csv'), 'dsh-resource://srcos/artifact/run1/metrics.csv')
})

test('both directions are idempotent', () => {
  const native = 'srcos://file/home/a.md'
  const dsh = 'dsh-resource://srcos/file/home/a.md'
  assert.equal(toDsh(dsh), dsh)
  assert.equal(toNative(native), native)
  assert.equal(toDsh(toDsh(native)), dsh)
  assert.equal(toNative(toNative(dsh)), native)
})

test('other protocols and other schemes are refused, not mangled', () => {
  assert.equal(toDsh('dsh-resource://file/session/s1/a.txt'), null)
  assert.equal(toNative('dsh-resource://file/session/s1/a.txt'), null)
  assert.equal(toDsh('sidebar://srcos'), null)
  assert.equal(toDsh('/etc/passwd'), null)
  assert.equal(toDsh(''), null)
  assert.equal(toDsh(undefined), null)
  assert.equal(toNative(42), null)
  // A bare protocol with no path is still ours.
  assert.equal(toNative('dsh-resource://srcos'), 'srcos://')
})

test('the claim predicate accepts exactly the two spellings', () => {
  for (const yes of ['srcos://file/home/a', 'dsh-resource://srcos/file/home/a']) {
    assert.equal(isSrcosAddress(yes), true, yes)
  }
  for (const no of ['dsh-resource://file/x', 'sidebar://srcos', '/home/a', '', undefined]) {
    assert.equal(isSrcosAddress(no), false, String(no))
  }
  assert.equal(isNative('srcos://x'), true)
  assert.equal(isDsh('dsh-resource://srcos/x'), true)
})

test('split reads the pieces the UI needs, without validating them', () => {
  assert.deepEqual(split('srcos://file/workspace/out/x.txt?tool=demo'), {
    native: 'srcos://file/workspace/out/x.txt?tool=demo',
    provider: 'file', scope: 'workspace', path: 'out/x.txt', tool: 'demo', query: 'tool=demo',
  })
  assert.deepEqual(split('dsh-resource://srcos/file/home')?.scope, 'home')
  assert.equal(split('dsh-resource://file/home'), null)
})

test('titles come from the last segment, decoded', () => {
  assert.equal(basenameOf('srcos://file/home/notes.md'), 'notes.md')
  assert.equal(basenameOf('srcos://file/home/a%20b.txt'), 'a b.txt')
  // A scope root has no path: the scope names it.
  assert.equal(basenameOf('srcos://file/home'), 'home')
  assert.equal(basenameOf('srcos://file/workspace?tool=demo'), 'workspace')
})

test('a child address keeps the spelling and the query', () => {
  assert.equal(childAddress('srcos://file/home', 'a.txt'), 'srcos://file/home/a.txt')
  assert.equal(childAddress('srcos://file/home/sub', 'a.txt'), 'srcos://file/home/sub/a.txt')
  // The workspace's ?tool= is part of the scope, so it must survive navigation.
  assert.equal(
    childAddress('dsh-resource://srcos/file/workspace/out?tool=demo', 'x.txt'),
    'dsh-resource://srcos/file/workspace/out/x.txt?tool=demo',
  )
})
