import { forwardRef, useId, type ButtonHTMLAttributes, type InputHTMLAttributes, type ReactNode } from 'react'
import { classNames } from './classNames.ts'

type ButtonVariant = 'primary' | 'secondary' | 'quiet'

const buttonVariants: Record<ButtonVariant, string> = {
  primary: 'border-transparent bg-accent text-bg shadow-[0_10px_32px_var(--color-glow)] hover:brightness-110',
  secondary: 'border-line bg-white/4 text-fg hover:border-accent',
  quiet: 'border-transparent bg-transparent text-muted hover:bg-line-soft hover:text-fg',
}

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant
  pending?: boolean
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button({
  variant = 'secondary', pending = false, disabled, className, children, type = 'button', ...props
}, ref) {
  return (
    <button
      ref={ref}
      className={classNames(
        'inline-flex min-h-11 items-center justify-center gap-2 rounded-full border px-4 py-2 font-semibold transition enabled:active:scale-[.98] disabled:cursor-wait disabled:opacity-50',
        buttonVariants[variant], className,
      )}
      disabled={disabled || pending}
      type={type}
      {...props}
    >
      {pending ? 'Подождите…' : children}
    </button>
  )
})

interface IconButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  'aria-label': string
  pressed?: boolean
}

export const IconButton = forwardRef<HTMLButtonElement, IconButtonProps>(function IconButton({
  'aria-label': label, pressed, className, children, type = 'button', ...props
}, ref) {
  return (
    <button
      ref={ref}
      aria-label={label}
      aria-pressed={pressed}
      className={classNames(
        'grid size-11 shrink-0 place-items-center rounded-full border border-line bg-white/4 text-fg transition hover:border-accent hover:bg-line-soft disabled:opacity-50',
        pressed && 'border-accent bg-accent/20 text-accent', className,
      )}
      title={label}
      type={type}
      {...props}
    >
      <span aria-hidden="true">{children}</span>
    </button>
  )
})

interface FieldProps extends InputHTMLAttributes<HTMLInputElement> {
  label: string
  hint?: string
  error?: string
}

export const Field = forwardRef<HTMLInputElement, FieldProps>(function Field({
  label, hint, error, id, className, ...props
}, ref) {
  const generatedId = useId()
  const inputId = id ?? generatedId
  const hintId = hint ? `${inputId}-hint` : undefined
  const errorId = error ? `${inputId}-error` : undefined
  const describedBy = [props['aria-describedby'], hintId, errorId].filter(Boolean).join(' ') || undefined

  return (
    <div className="grid gap-1.5">
      <label className="text-sm font-semibold text-fg" htmlFor={inputId}>{label}</label>
      <input
        ref={ref}
        aria-describedby={describedBy}
        aria-invalid={error ? true : undefined}
        className={classNames(
          'min-h-11 w-full rounded-xl border border-line bg-bg2 px-3 text-fg placeholder:text-muted outline-none transition focus-visible:border-accent disabled:opacity-50',
          error && 'border-red-400', className,
        )}
        id={inputId}
        {...props}
      />
      {hint && <p className="text-xs text-muted" id={hintId}>{hint}</p>}
      {error && <p className="text-sm text-red-300" id={errorId} role="alert">{error}</p>}
    </div>
  )
})

interface SectionProps {
  title: string
  hint?: string
  action?: ReactNode
  children: ReactNode
  className?: string
}

export function Section({ title, hint, action, children, className }: SectionProps) {
  const headingId = useId()
  return (
    <section aria-labelledby={headingId} className={classNames('grid gap-4', className)}>
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <div>
          <h2 className="font-display text-2xl font-bold" id={headingId}>{title}</h2>
          {hint && <p className="mt-1 text-sm text-muted">{hint}</p>}
        </div>
        {action}
      </div>
      {children}
    </section>
  )
}
