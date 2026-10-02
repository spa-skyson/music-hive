import assert from 'node:assert/strict'
import test from 'node:test'
import type { BackgroundJob } from '../../data/jobs.ts'
import { jobProgress } from './progress.ts'

const job = (partial: Partial<BackgroundJob>): BackgroundJob => ({
  id: 1, kind: 'scan', status: 'running', created_at: '', updated_at: '', ...partial,
})

test('jobProgress reads direct progress and combines details', () => {
  assert.deepEqual(jobProgress(job({ progress: { pct: 42.5, phase: 'scan', message: 'album' } })), {
    pct: 42.5, detail: 'scan · album',
  })
})

test('jobProgress falls back to result progress and rejects invalid percent', () => {
  assert.deepEqual(jobProgress(job({ result: { progress: { pct: Number.NaN, detail: 'waiting' } } })), {
    pct: null, detail: 'waiting',
  })
})
