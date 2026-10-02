import { createContext, useContext } from 'react'
import type { BackgroundJob } from './jobs.ts'

export interface JobsData {
  jobs: BackgroundJob[]
  error: Error | null
  libraryVersion: number
  retry: () => void
}

export const JobsContext = createContext<JobsData | null>(null)

export function useJobs(): JobsData {
  const value = useContext(JobsContext)
  if (!value) throw new Error('useJobs must be used inside JobsProvider')
  return value
}
