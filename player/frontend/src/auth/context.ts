import { createContext, useContext } from 'react'

export interface AuthUser {
  id?: number
  username?: string
  isOwner?: boolean
}

export interface AuthContextValue {
  authEnabled: boolean
  isAdmin: boolean
  ready: boolean
  user: AuthUser | null
  login: (username: string, password: string) => Promise<void>
  logout: () => Promise<void>
}

export const AuthContext = createContext<AuthContextValue | null>(null)

export function useAuth(): AuthContextValue {
  const context = useContext(AuthContext)
  if (!context) throw new Error('useAuth must be used within AuthProvider')
  return context
}
