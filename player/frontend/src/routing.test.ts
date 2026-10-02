import assert from 'node:assert/strict'
import test from 'node:test'
import { parseHash, routeHash } from './routing.ts'

test('all application routes round-trip', () => {
  for (const view of ['home', 'player', 'library', 'profile', 'upload', 'users'] as const) {
    assert.deepEqual(parseHash(routeHash({ view })), { view })
  }
})

test('unknown and empty hashes fall back to home', () => {
  assert.deepEqual(parseHash(''), { view: 'home' })
  assert.deepEqual(parseHash('#/unknown'), { view: 'home' })
})
