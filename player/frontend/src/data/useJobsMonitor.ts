import { useEffect, useEffectEvent, useRef, useState } from 'react'
import { apiFetch } from '../api/client.ts'
import { completedTransitions, LIBRARY_JOB_KINDS, nextPollDelay, type BackgroundJob } from './jobs.ts'
import { useToast } from './toastContext.ts'

interface JobsResponse { jobs?: BackgroundJob[] }

interface JobsMonitorOptions {
  enabled?: boolean
  onLibraryChanged?: () => void
}

export function useJobsMonitor({ enabled = true, onLibraryChanged }: JobsMonitorOptions = {}) {
  const { showToast } = useToast()
  const generationRef = useRef(0)
  const [attempt, setAttempt] = useState(0)
  const [jobs, setJobs] = useState<BackgroundJob[]>([])
  const [error, setError] = useState<Error | null>(null)
  const notify = useEffectEvent(showToast)
  const notifyLibraryChanged = useEffectEvent(() => onLibraryChanged?.())

  useEffect(() => {
    if (!enabled) return
    const generation = ++generationRef.current
    const known = new Map<number, string>()
    let timer: number | undefined
    let failures = 0
    let initial = true

    const poll = async () => {
      if (generation !== generationRef.current) return
      if (document.hidden) {
        document.addEventListener('visibilitychange', poll, { once: true })
        return
      }
      try {
        const response = await apiFetch<JobsResponse>('/api/jobs?limit=20')
        if (generation !== generationRef.current) return
        const nextJobs = Array.isArray(response.jobs) ? response.jobs : []
        if (!initial) {
          const transitions = completedTransitions(known, nextJobs)
          for (const { job, status } of transitions) {
            notify(status === 'done' ? `${job.kind} завершено` : `${job.kind}: ошибка`, status === 'failed' ? { variant: 'error' } : undefined)
          }
          if (transitions.some(({ job, status }) => status === 'done' && LIBRARY_JOB_KINDS.has(job.kind))) notifyLibraryChanged()
        }
        known.clear()
        for (const job of nextJobs) known.set(job.id, job.status)
        initial = false
        failures = 0
        setJobs(nextJobs)
        setError(null)
      } catch (cause) {
        if (generation !== generationRef.current) return
        failures = Math.min(failures + 1, 3)
        setError(cause instanceof Error ? cause : new Error('Не удалось загрузить задачи'))
      }
      if (generation === generationRef.current) timer = window.setTimeout(poll, nextPollDelay(failures))
    }

    void poll()
    return () => {
      generationRef.current += 1
      if (timer !== undefined) window.clearTimeout(timer)
      document.removeEventListener('visibilitychange', poll)
    }
  }, [attempt, enabled])

  return { jobs, error, retry: () => setAttempt((value) => value + 1) }
}
