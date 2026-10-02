export interface JobProgress {
  pct?: number
  phase?: string
  message?: string
  detail?: string
}

export interface BackgroundJob {
  id: number
  kind: string
  status: string
  error?: string
  progress?: JobProgress
  result?: { progress?: JobProgress }
  created_at: string
  updated_at: string
}

export const LIBRARY_JOB_KINDS = new Set(['full_rescan', 'scan', 'embed', 'clusters', 'mix_pack'])

export interface JobTransition {
  job: BackgroundJob
  status: 'done' | 'failed'
}

export function completedTransitions(previous: ReadonlyMap<number, string>, jobs: readonly BackgroundJob[]): JobTransition[] {
  const transitions: JobTransition[] = []
  for (const job of jobs) {
    const before = previous.get(job.id)
    if (before && before !== job.status && (job.status === 'done' || job.status === 'failed')) {
      transitions.push({ job, status: job.status })
    }
  }
  return transitions
}

export function nextPollDelay(failures: number): number {
  return [3_000, 6_000, 12_000, 30_000][Math.min(Math.max(failures, 0), 3)]
}
