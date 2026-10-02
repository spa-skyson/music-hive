import { classNames } from './classNames.ts'
import { Button } from './controls.tsx'

interface StateProps {
  title: string
  description?: string
  className?: string
}

export function EmptyState({ title, description, className }: StateProps) {
  return (
    <div className={classNames('rounded-app border border-dashed border-line p-8 text-center', className)}>
      <p className="font-semibold">{title}</p>
      {description && <p className="mt-2 text-sm text-muted">{description}</p>}
    </div>
  )
}

interface ErrorStateProps extends StateProps {
  onRetry: () => void
}

export function ErrorState({ title = 'Не удалось загрузить', description, onRetry, className }: ErrorStateProps) {
  return (
    <div className={classNames('flex flex-wrap items-center justify-between gap-4 rounded-app border border-red-400/50 bg-red-950/20 p-4', className)} role="alert">
      <div>
        <p className="font-semibold">{title}</p>
        {description && <p className="mt-1 text-sm text-muted">{description}</p>}
      </div>
      <Button onClick={onRetry}>Повторить</Button>
    </div>
  )
}

export function SkeletonCards({ count = 3, label = 'Загрузка' }: { count?: number; label?: string }) {
  return (
    <div aria-label={label} className="grid grid-cols-[repeat(auto-fit,minmax(11rem,1fr))] gap-4" role="status">
      {Array.from({ length: count }, (_, index) => (
        <div aria-hidden="true" className="min-h-48 animate-pulse rounded-app border border-line bg-gradient-to-br from-line-soft via-surface to-line-soft" key={index} />
      ))}
    </div>
  )
}
