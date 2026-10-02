import assert from 'node:assert/strict'
import test from 'node:test'
import { playerHotkey } from './hotkeys.ts'

const plainTarget = { closest: () => null } as unknown as EventTarget
const editableTarget = { closest: () => ({}) } as unknown as EventTarget

test('maps player shortcuts without taking arrow keys', () => {
  assert.equal(playerHotkey({ code: 'Space', key: ' ', target: plainTarget }), 'toggle-play')
  assert.equal(playerHotkey({ code: 'KeyN', key: 'N', target: plainTarget }), 'skip')
  assert.equal(playerHotkey({ code: 'KeyL', key: 'l', target: plainTarget }), 'like')
  assert.equal(playerHotkey({ code: 'ArrowRight', key: 'ArrowRight', target: plainTarget }), null)
})

test('ignores shortcuts from editable and interactive targets', () => {
  assert.equal(playerHotkey({ code: 'Space', key: ' ', target: editableTarget }), null)
  assert.equal(playerHotkey({ code: 'KeyN', key: 'n', target: editableTarget }), null)
  assert.equal(playerHotkey({ code: 'KeyL', key: 'l', target: editableTarget }), null)
})
