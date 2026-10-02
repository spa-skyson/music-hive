import { classNames } from './classNames.ts'
import { IconButton } from './controls.tsx'

export type EntityKind = 'track' | 'album' | 'artist'

interface EntityCardProps {
  kind: EntityKind
  title: string
  subtitle?: string
  meta?: string
  artwork?: string
  favorite?: boolean
  onPlay: () => void
  onShuffle?: () => void
  onFavorite?: () => void
  className?: string
}

const kindLabels: Record<EntityKind, string> = {
  track: 'трек',
  album: 'альбом',
  artist: 'артиста',
}

export function EntityCard({
  kind, title, subtitle, meta, artwork, favorite = false, onPlay, onShuffle, onFavorite, className,
}: EntityCardProps) {
  const playLabel = `Слушать ${kindLabels[kind]} ${title}`
  return (
    <article className={classNames(
      'group relative isolate flex min-h-48 w-full min-w-44 max-w-64 flex-col justify-end overflow-hidden rounded-app border border-line bg-bg2 p-4 shadow-lg transition hover:-translate-y-1 hover:border-accent/60',
      className,
    )}>
      {artwork ? (
        <img
          alt=""
          className={classNames('absolute inset-0 -z-20 size-full object-cover brightness-[.55]', kind === 'artist' && 'rounded-full p-5')}
          decoding="async"
          height="320"
          loading="lazy"
          src={artwork}
          width="320"
        />
      ) : (
        <div aria-hidden="true" className="absolute inset-0 -z-20 grid place-items-center bg-gradient-to-br from-accent/35 to-bg text-6xl font-bold text-fg/20">
          {title.slice(0, 1).toLocaleUpperCase('ru')}
        </div>
      )}
      <div className="absolute inset-0 -z-10 bg-gradient-to-b from-transparent from-10% to-bg/95" />
      <button
        aria-label={playLabel}
        className="absolute inset-0 z-10 rounded-[inherit] border-0 bg-transparent focus-visible:outline-offset-[-3px]"
        onClick={onPlay}
        type="button"
      />
      <strong className="truncate text-lg">{title}</strong>
      {subtitle && <span className="truncate text-sm text-muted">{subtitle}</span>}
      {meta && <span className="mt-1 text-xs text-accent2">{meta}</span>}
      {onFavorite && (
        <IconButton
          aria-label={favorite ? `Убрать ${title} из избранного` : `Добавить ${title} в избранное`}
          className="absolute right-2 top-2 z-20 size-11 bg-bg/80"
          onClick={onFavorite}
          pressed={favorite}
        >♥</IconButton>
      )}
      {onShuffle && (
        <IconButton
          aria-label={`Слушать вперемешку: ${title}`}
          className={classNames('absolute top-2 z-20 size-11 bg-bg/80', onFavorite ? 'right-14' : 'right-2')}
          onClick={onShuffle}
        >⇄</IconButton>
      )}
    </article>
  )
}
