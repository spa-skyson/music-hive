import assert from 'node:assert/strict'
import test from 'node:test'
import {
  accumulateListened,
  formatTime,
  initialPlaybackState,
  reducePlaybackPayload,
  shouldPostProgress,
} from './state.ts'

test('formatTime formats valid durations and guards invalid values', () => {
  assert.equal(formatTime(65.9), '1:05')
  assert.equal(formatTime(Number.NaN), '0:00')
  assert.equal(formatTime(-1), '0:00')
})

test('accumulateListened only counts forward movement', () => {
  assert.equal(accumulateListened(10, 5, 8), 13)
  assert.equal(accumulateListened(10, 8, 3), 10)
})

test('progress is sent only for a current track after four seconds', () => {
  assert.equal(shouldPostProgress(1_000, 5_000, true), false)
  assert.equal(shouldPostProgress(1_000, 5_001, true), true)
  assert.equal(shouldPostProgress(1_000, 9_000, false), false)
})

test('server payload replaces fixed playlist or radio queue', () => {
  const fixed = reducePlaybackPayload(initialPlaybackState, {
    session_id: 'one',
    fixed: true,
    current: { id: 1, title: 'First' },
    tracks: [{ id: 1 }, { id: 2 }],
    index: 0,
  })
  assert.deepEqual(fixed.tracks.map(({ id }) => id), [1, 2])
  assert.deepEqual(fixed.queue, [])

  const radio = reducePlaybackPayload(fixed, {
    fixed: false,
    next: { id: 3 },
    queue: [{ id: 4, explanation: 'next' }],
  })
  assert.equal(radio.current?.id, 3)
  assert.deepEqual(radio.queue.map(({ id }) => id), [4])
  assert.equal(radio.fixed, false)
})

test('tracks null from a radio payload preserves its queue', () => {
  const radio = reducePlaybackPayload(initialPlaybackState, {
    fixed: false,
    tracks: null,
    queue: [{ track_id: 7, title: 'Queued' }],
  })

  assert.deepEqual(radio.tracks, [])
  assert.deepEqual(radio.queue.map(({ id }) => id), [7])
})

test('event payload with queue preserves a fixed playlist without fixed flag', () => {
  const fixed = reducePlaybackPayload(initialPlaybackState, {
    fixed: true,
    tracks: [{ id: 1 }, { id: 2 }],
  })

  const rated = reducePlaybackPayload(fixed, {
    queue: [{ track_id: 3, title: 'Queued after dislike' }],
    rating: 'dislike',
  })

  // Event payloads update the queue without clearing the playlist (app.js:839-841).
  assert.deepEqual(rated.tracks.map(({ id }) => id), [1, 2])
  assert.deepEqual(rated.queue.map(({ id }) => id), [3])
})

test('shuffle payload reorders a fixed playlist without changing its current track', () => {
  const current = { id: 2, title: 'Playing' }
  const state = { ...initialPlaybackState, sessionId: 'one', fixed: true, current, tracks: [{ id: 1 }, current, { id: 3 }], currentIndex: 1 }

  const shuffled = reducePlaybackPayload(state, {
    session_id: 'one',
    fixed: true,
    current,
    tracks: [current, { id: 3 }, { id: 1 }],
    index: 0,
  })

  assert.equal(shuffled.current, current)
  assert.equal(shuffled.currentIndex, 0)
  assert.deepEqual(shuffled.tracks.map(({ id }) => id), [2, 3, 1])
})
