import { routeHash, type View } from '../routing.ts'
import { useFavorites } from '../data/index.ts'
import { IconButton } from '../ui/index.ts'
import { usePlayer } from './context.ts'
import { usePlayAction } from './usePlayAction.ts'

export function MiniPlayer({ activeView }: { activeView: View }) {
  const player = usePlayer()
  const favorites = useFavorites()
  const { report } = usePlayAction()

  if (!player.sessionId || activeView === 'player') return null

  return (
    <aside aria-label="Мини-плеер" className="fixed inset-x-2 bottom-2 z-10 mx-auto flex max-w-3xl items-center gap-2 rounded-app border border-line bg-surface-strong p-2 shadow-app backdrop-blur sm:inset-x-4 sm:bottom-4">
      <a className="flex min-w-0 flex-1 items-center gap-3 rounded-xl outline-none focus-visible:ring-2 focus-visible:ring-accent" href={routeHash({ view: 'player' })}>
        <span className="grid size-12 shrink-0 place-items-center overflow-hidden rounded-lg border border-line bg-gradient-to-br from-accent/30 via-bg2 to-accent2/30 text-xl text-muted">
          {player.current?.artwork
            ? <img alt="" className="size-full object-cover" decoding="async" height="48" loading="lazy" src={player.current.artwork} width="48" />
            : <span aria-hidden="true">{(player.current?.title || '♪').slice(0, 1).toUpperCase()}</span>}
        </span>
        <span className="min-w-0">
          <strong className="block truncate">{player.current?.title || '—'}</strong>
          <span className="block truncate text-sm text-muted">{player.current?.artist || '—'}</span>
        </span>
      </a>
      <IconButton aria-label={`${player.playing ? 'Пауза' : 'Воспроизвести'} (Пробел)`} disabled={!player.current || player.busy} onClick={() => void player.togglePlay().catch(report)}>{player.playing ? '❚❚' : '▶'}</IconButton>
      <IconButton aria-label="Следующий трек (N)" disabled={!player.current || player.busy} onClick={() => void player.skip().catch(report)}>⏭</IconButton>
      <IconButton aria-label={`${player.current && favorites.trackIds.has(player.current.id) ? 'Убрать из любимых' : 'Добавить в любимые'} (L)`} disabled={!player.current || favorites.loading} onClick={() => player.current && void favorites.toggleFavorite({ track_id: player.current.id }).catch(report)} pressed={Boolean(player.current && favorites.trackIds.has(player.current.id))}>♥</IconButton>
    </aside>
  )
}
