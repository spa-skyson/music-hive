import assert from 'node:assert/strict'
import test from 'node:test'
import { completedTransitions, LIBRARY_JOB_KINDS, nextPollDelay, type BackgroundJob } from './jobs.ts'

function job(id: number, status: string): BackgroundJob {
  return { id, kind: 'scan', status, created_at: '', updated_at: '' }
}

test('completedTransitions reports only known jobs becoming terminal', () => {
  const previous = new Map([[1, 'running'], [2, 'pending'], [3, 'done']])
  assert.deepEqual(completedTransitions(previous, [job(1, 'done'), job(2, 'failed'), job(3, 'done'), job(4, 'done')]), [
    { job: job(1, 'done'), status: 'done' },
    { job: job(2, 'failed'), status: 'failed' },
  ])
})

test('nextPollDelay backs off from three to thirty seconds', () => {
  assert.deepEqual([-1, 0, 1, 2, 3, 4].map(nextPollDelay), [3_000, 3_000, 6_000, 12_000, 30_000, 30_000])
})

test('library refreshes after scans and mix generation', () => {
  assert.equal(LIBRARY_JOB_KINDS.has('full_rescan'), true)
  assert.equal(LIBRARY_JOB_KINDS.has('mix_pack'), true)
})
