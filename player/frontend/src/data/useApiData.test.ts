import assert from 'node:assert/strict'
import test from 'node:test'
import { pendingApiDataState } from './apiDataState.ts'
import type { ApiDataState } from './useApiData.ts'

test('pendingApiDataState keeps successful data while a new key loads', () => {
  const previous: ApiDataState<{ value: number }> = { status: 'data', data: { value: 42 }, error: null }
  assert.deepEqual(pendingApiDataState(previous), { status: 'loading', data: previous.data, error: null })
})

test('pendingApiDataState does not expose incomplete or failed data', () => {
  assert.deepEqual(pendingApiDataState(undefined), { status: 'loading', data: null, error: null })
  assert.deepEqual(
    pendingApiDataState({ status: 'error', data: { value: 42 }, error: new Error('failed') }),
    { status: 'loading', data: null, error: null },
  )
})
