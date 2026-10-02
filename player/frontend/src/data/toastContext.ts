import { createContext, useContext } from 'react'

export type ToastVariant = 'status' | 'error'

export interface ToastOptions {
  variant?: ToastVariant
  duration?: number
}

export interface ToastContextValue {
  showToast: (message: string, options?: ToastOptions) => void
  dismissToast: () => void
}

export const ToastContext = createContext<ToastContextValue | null>(null)

export function useToast(): ToastContextValue {
  const context = useContext(ToastContext)
  if (!context) throw new Error('useToast must be used within ToastProvider')
  return context
}
