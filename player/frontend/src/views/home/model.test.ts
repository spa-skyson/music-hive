import assert from 'node:assert/strict'
import test from 'node:test'
import { jobProgressLabel, mixMeta, WEEKDAY_RU } from './model.ts'

test('подпись микса учитывает специальные и неготовые полки', () => {
  assert.equal(mixMeta({ kind: 'later', title: '', tracks: 3 }), '3 в очереди')
  assert.equal(mixMeta({ kind: 'favorites', title: '', tracks: 0 }), 'жми ♥ на треке')
  assert.equal(mixMeta({ kind: 'daily', title: '', ready: false }), 'нажми «Обновить миксы»')
  assert.equal(mixMeta({ kind: 'daily', title: '', ready: true, tracks: 12 }), '12 треков')
  assert.equal(WEEKDAY_RU.weekday_wed, 'среда')
})

test('подпись прогресса предпочитает сообщение', () => {
  assert.equal(jobProgressLabel({ progress: { message: 'Собираем' } }), 'Собираем')
  assert.equal(jobProgressLabel({ result: { progress: { phase: 'embed', pct: 42 } } }), 'embed 42%')
})
