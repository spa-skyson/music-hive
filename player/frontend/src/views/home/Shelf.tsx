import { useId, useRef, type KeyboardEvent, type ReactNode } from 'react'
import { IconButton, Section } from '../../ui/index.ts'

interface ShelfProps {
  title: string
  hint?: string
  action?: ReactNode
  children: ReactNode
}

export function Shelf({ title, hint, action, children }: ShelfProps) {
  const railRef = useRef<HTMLDivElement>(null)
  const descriptionId = useId()

  function move(direction: -1 | 1) {
    const rail = railRef.current
    if (rail) rail.scrollBy({ left: direction * Math.max(280, rail.clientWidth * 0.8), behavior: 'smooth' })
  }

  function onKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    if (event.target !== event.currentTarget) return
    if (event.key === 'ArrowLeft' || event.key === 'PageUp') {
      event.preventDefault()
      move(-1)
    } else if (event.key === 'ArrowRight' || event.key === 'PageDown') {
      event.preventDefault()
      move(1)
    } else if (event.key === 'Home') {
      event.preventDefault()
      event.currentTarget.scrollTo({ left: 0, behavior: 'smooth' })
    } else if (event.key === 'End') {
      event.preventDefault()
      event.currentTarget.scrollTo({ left: event.currentTarget.scrollWidth, behavior: 'smooth' })
    }
  }

  return (
    <Section action={action} hint={hint} title={title}>
      <p className="sr-only" id={descriptionId}>Горизонтальная полка. Используйте стрелки, Page Up, Page Down, Home или End для прокрутки.</p>
      <div className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-2">
        <IconButton aria-label={`Прокрутить «${title}» назад`} onClick={() => move(-1)}>‹</IconButton>
        <div
          ref={railRef}
          aria-describedby={descriptionId}
          aria-label={title}
          className="flex snap-x snap-mandatory gap-4 overflow-x-auto overscroll-x-contain scroll-smooth rounded-app py-2 [scrollbar-width:thin] [&>*]:w-44 [&>*]:shrink-0 [&>*]:snap-start sm:[&>*]:w-52"
          onKeyDown={onKeyDown}
          role="region"
          tabIndex={0}
        >{children}</div>
        <IconButton aria-label={`Прокрутить «${title}» вперёд`} onClick={() => move(1)}>›</IconButton>
      </div>
    </Section>
  )
}
