import assert from 'node:assert/strict'
import test from 'node:test'
import { classNames } from './classNames.ts'

test('classNames пропускает пустые значения', () => {
  assert.equal(classNames('base', false, undefined, 'active', null), 'base active')
})
