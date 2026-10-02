import { classNames } from './classNames.ts'
import { IconButton } from './controls.tsx'

type ToastVariant = 'status' | 'error'

interface ToastProps {
  message: string
  variant?: ToastVariant
  onDismiss?: () => void
  className?: string
}

export function Toast({ message, variant = 'status', onDismiss, className }: ToastProps) {
  return (
    <div
      aria-live={variant === 'error' ? 'assertive' : 'polite'}
      className={classNames(
        'flex max-w-md items-center gap-3 rounded-full border border-line bg-surface-strong px-4 py-2 text-sm font-semibold shadow-app',
        variant === 'error' && 'border-red-400/60 text-red-200', className,
      )}
      role={variant === 'error' ? 'alert' : 'status'}
    >
      <span className="flex-1">{message}</span>
      {onDismiss && <IconButton aria-label="Закрыть уведомление" className="size-8" onClick={onDismiss}>×</IconButton>}
    </div>
  )
}
