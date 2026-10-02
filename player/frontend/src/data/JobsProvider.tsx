import { useState, type ReactNode } from 'react'
import { JobsContext } from './jobsContext.ts'
import { useJobsMonitor } from './useJobsMonitor.ts'

export function JobsProvider({ children, enabled = true }: { children: ReactNode; enabled?: boolean }) {
  const [libraryVersion, setLibraryVersion] = useState(0)
  const monitor = useJobsMonitor({ enabled, onLibraryChanged: () => setLibraryVersion((value) => value + 1) })
  return <JobsContext.Provider value={{ ...monitor, libraryVersion }}>{children}</JobsContext.Provider>
}
