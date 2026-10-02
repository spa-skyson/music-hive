import { useEffect, useState, type ReactNode } from 'react'
import { apiFetch, UNAUTHORIZED_EVENT } from '../api/client.ts'
import { AuthContext, type AuthUser } from './context.ts'

interface MeResponse {
  ok: boolean
  auth_enabled: boolean
  id?: number
  username?: string
  is_admin?: boolean
  is_owner?: boolean
}

interface LoginResponse {
  ok: boolean
  user?: { id?: number; username?: string; is_admin?: boolean; is_owner?: boolean }
}

function userFromMe(me: MeResponse): AuthUser | null {
  if (!me.ok) return null
  return { id: me.id, username: me.username, isOwner: me.is_owner }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [authEnabled, setAuthEnabled] = useState(false)
  const [isAdmin, setIsAdmin] = useState(false)
  const [ready, setReady] = useState(false)
  const [user, setUser] = useState<AuthUser | null>(null)

  async function refresh() {
    const me = await apiFetch<MeResponse>('/api/auth/me')
    setAuthEnabled(me.auth_enabled)
    setIsAdmin(Boolean(me.is_admin))
    setUser(userFromMe(me))
    setReady(true)
  }

  useEffect(() => {
    let cancelled = false
    apiFetch<MeResponse>('/api/auth/me')
      .then((me) => {
        if (cancelled) return
        setAuthEnabled(me.auth_enabled)
        setIsAdmin(Boolean(me.is_admin))
        setUser(userFromMe(me))
        setReady(true)
      })
      .catch(() => {
        if (!cancelled) {
          setAuthEnabled(true)
          setReady(true)
        }
      })
    return () => { cancelled = true }
  }, [])

  useEffect(() => {
    const handleUnauthorized = () => {
      setUser(null)
      setIsAdmin(false)
      setReady(true)
    }
    globalThis.addEventListener(UNAUTHORIZED_EVENT, handleUnauthorized)
    return () => globalThis.removeEventListener(UNAUTHORIZED_EVENT, handleUnauthorized)
  }, [])

  async function login(username: string, password: string) {
    const response = await apiFetch<LoginResponse>('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    })
    if (response.user) {
      setUser({ id: response.user.id, username: response.user.username, isOwner: response.user.is_owner })
      setIsAdmin(Boolean(response.user.is_admin))
      setReady(true)
      return
    }
    await refresh()
  }

  async function logout() {
    await apiFetch<unknown>('/api/auth/logout', { method: 'POST', body: '{}' })
    setUser(null)
    setIsAdmin(false)
  }

  return (
    <AuthContext.Provider value={{ authEnabled, isAdmin, ready, user, login, logout }}>
      {children}
    </AuthContext.Provider>
  )
}
