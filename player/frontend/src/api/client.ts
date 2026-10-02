import type { paths } from './schema.d.ts'

export const UNAUTHORIZED_EVENT = 'music-hive:unauthorized'

export type ApiPath = keyof paths | `/api/${string}`

export async function apiFetch<T>(path: ApiPath, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers)
  if (!(options.body instanceof FormData)) headers.set('Content-Type', 'application/json')

  const response = await fetch(path, {
    credentials: 'same-origin',
    ...options,
    headers,
  })

  if (response.status === 401 && !path.startsWith('/api/auth/')) {
    globalThis.dispatchEvent(new Event(UNAUTHORIZED_EVENT))
    throw new Error('unauthorized')
  }

  if (!response.ok) {
    let message = response.statusText
    try {
      const text = await response.text()
      try {
        const body = JSON.parse(text) as { error?: string | { message?: string } }
        message = typeof body.error === 'string' ? body.error : (body.error?.message ?? text) || message
      } catch {
        message = text || message
      }
    } catch {
      // Keep the HTTP status text when the body cannot be read.
    }
    throw Object.assign(new Error(message), { status: response.status })
  }

  if (!response.headers.get('content-type')?.includes('json')) return null as T
  return response.json() as Promise<T>
}
