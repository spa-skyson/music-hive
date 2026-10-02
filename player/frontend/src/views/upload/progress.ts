import type { BackgroundJob } from '../../data/jobs.ts'

export function jobProgress(job: BackgroundJob): { pct: number | null; detail: string } {
  const progress = job.progress ?? job.result?.progress ?? {}
  const pct = Number(progress.pct)
  const details = [progress.phase, progress.message ?? progress.detail].filter(Boolean)
  return {
    pct: Number.isFinite(pct) ? pct : null,
    detail: details.join(' · '),
  }
}
