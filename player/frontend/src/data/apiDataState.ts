import type { ApiDataState } from './useApiData.ts'

export function pendingApiDataState<T>(state: ApiDataState<T> | undefined): ApiDataState<T> {
  return { status: 'loading', data: state?.status === 'data' ? state.data : null, error: null }
}
