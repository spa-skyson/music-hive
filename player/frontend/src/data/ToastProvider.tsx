import { useEffect, useState, type ReactNode } from 'react'
import { Toast } from '../ui/index.ts'
import { ToastContext, type ToastOptions, type ToastVariant } from './toastContext.ts'

interface ActiveToast {
  id: number
  message: string
  variant: ToastVariant
  duration: number
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toast, setToast] = useState<ActiveToast | null>(null)

  function showToast(message: string, options: ToastOptions = {}) {
    setToast({
      id: Date.now(),
      message,
      variant: options.variant ?? 'status',
      duration: options.duration ?? 2_200,
    })
  }

  function dismissToast() {
    setToast(null)
  }

  useEffect(() => {
    if (!toast || toast.duration <= 0) return
    const timer = window.setTimeout(dismissToast, toast.duration)
    return () => window.clearTimeout(timer)
  }, [toast])

  return (
    <ToastContext.Provider value={{ showToast, dismissToast }}>
      {children}
      {toast && (
        <div className="fixed bottom-5 left-1/2 z-50 w-[min(calc(100%-2rem),28rem)] -translate-x-1/2" key={toast.id}>
          <Toast message={toast.message} onDismiss={dismissToast} variant={toast.variant} />
        </div>
      )}
    </ToastContext.Provider>
  )
}
