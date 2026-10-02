import { useEffect, useEffectEvent, useState } from 'react'
import { pendingApiDataState } from './apiDataState.ts'

export type ApiDataState<T> =
  | { status: 'idle'; data: null; error: null }
  | { status: 'loading'; data: T | null; error: null }
  | { status: 'error'; data: T | null; error: Error }
  | { status: 'data'; data: T; error: null }

export type ApiDataResult<T> = ApiDataState<T> & { retry: () => void }

const idle = { status: 'idle', data: null, error: null } as const

export function useApiData<T>(key: string | number | null | undefined, load: (signal: AbortSignal) => Promise<T>): ApiDataResult<T> {
  const loadData = useEffectEvent(load)
  const [attempt, setAttempt] = useState(0)
  const requestKey = key === null || key === undefined ? null : `${String(key)}\0${attempt}`
  const [result, setResult] = useState<{ requestKey: string; state: ApiDataState<T> } | null>(null)
  useEffect(() => {
    if (requestKey === null) return

    const controller = new AbortController()
    loadData(controller.signal).then(
      (data) => {
        if (!controller.signal.aborted) setResult({ requestKey, state: { status: 'data', data, error: null } })
      },
      (cause: unknown) => {
        if (!controller.signal.aborted) {
          setResult({ requestKey, state: {
            status: 'error', data: null,
            error: cause instanceof Error ? cause : new Error('Не удалось загрузить'),
          } })
        }
      },
    )
    return () => controller.abort()
  }, [requestKey])

  const visibleState = requestKey === null
    ? idle
    : result?.requestKey !== requestKey
      ? pendingApiDataState(result?.state)
      : result.state
  return { ...visibleState, retry: () => setAttempt((value) => value + 1) }
}
