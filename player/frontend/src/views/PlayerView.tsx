import { memo, useEffect, useRef, useState } from 'react'
import { apiFetch } from '../api/client.ts'
import { albumKey, useApiData, useFavorites, useToast } from '../data/index.ts'
import { SESSION_KEY } from '../player/PlayerContext.tsx'
import { usePlayer, usePlayerProgress } from '../player/context.ts'
import { formatTime, type QueueItem, type Track } from '../player/state.ts'
import { usePlayAction } from '../player/usePlayAction.ts'
import { Button, EmptyState, ErrorState, IconButton } from '../ui/index.ts'

interface LyricsResponse {
  status?: string
  instrumental?: boolean
  plain_lyrics?: string
  synced_lyrics?: string
}

const maturityLabels: Record<string, string> = {
  discovering: 'изучаем вкус · слушай и скипай', forming: 'вкус формируется', ready: 'вкус готов',
}

const DISLIKES_KEY_PREFIX = `${SESSION_KEY}:dislikes:`

function dislikedTracks(sessionId: string | null): Set<number> {
  if (!sessionId) return new Set()
  try {
    const stored = JSON.parse(sessionStorage.getItem(`${DISLIKES_KEY_PREFIX}${sessionId}`) ?? '[]')
    return new Set(Array.isArray(stored) ? stored.filter((id): id is number => typeof id === 'number') : [])
  } catch {
    return new Set()
  }
}

function storeDislikedTracks(sessionId: string, tracks: Set<number>) {
  sessionStorage.setItem(`${DISLIKES_KEY_PREFIX}${sessionId}`, JSON.stringify([...tracks]))
}

export function PlayerView() {
  const player = usePlayer()
  const { showToast } = useToast()
  const { addToLater, report } = usePlayAction()
  const favorites = useFavorites()
  const [, refreshRating] = useState(0)
  const previousEnded = useRef(false)
  const currentId = player.current?.id

  useEffect(() => {
    if (player.ended && !previousEnded.current) showToast('Конец плейлиста')
    previousEnded.current = player.ended
  }, [player.ended, showToast])

  async function dislike() {
    if (!player.current || !player.sessionId) {
      showToast('Сначала запусти микс или трек', { variant: 'error' })
      return
    }
    try {
      const trackId = player.current.id
      const sessionRatings = dislikedTracks(player.sessionId)
      const result = await player.rateDetails('dislike')
      if ((result.rating ?? 'dislike') === 'dislike') {
        sessionRatings.add(trackId)
        storeDislikedTracks(player.sessionId, sessionRatings)
      }
      refreshRating((revision) => revision + 1)
      showToast(result.ignored ? 'Уже дизлайк' : 'Дизлайк')
    } catch (cause) { report(cause) }
  }

  function addLater() {
    if (!player.current) return showToast('Сейчас ничего не играет', { variant: 'error' })
    return addToLater(player.current.id, 'Добавлено в «Потом»')
  }

  const favorite = currentId !== undefined && favorites.trackIds.has(currentId)
  const rating = currentId !== undefined && dislikedTracks(player.sessionId).has(currentId) ? 'dislike' : null
  const artistFavorite = Boolean(player.current?.artist && favorites.artistNames.has(player.current.artist))
  const albumFavorite = Boolean(player.current?.artist && player.current?.album && favorites.albumKeys.has(albumKey(player.current.artist, player.current.album)))
  return (
    <div className="mt-6 grid gap-8">
      {player.error && <p className="rounded-xl border border-red-400/50 bg-red-950/20 p-4 text-red-200" role="alert">{player.error}</p>}
      <section className="grid gap-8 lg:grid-cols-[minmax(16rem,28rem)_1fr]">
        <div className="aspect-square overflow-hidden rounded-app border border-line bg-gradient-to-br from-accent/30 via-bg2 to-accent2/30 shadow-app">
          {player.current?.artwork
            ? <img alt="" className="size-full object-cover" decoding="async" height="640" loading="lazy" src={player.current.artwork} width="640" />
            : <div aria-hidden="true" className="grid size-full place-items-center text-8xl text-muted">{(player.current?.title || '♪').slice(0, 1).toUpperCase()}</div>}
        </div>
        <div className="flex min-w-0 flex-col justify-center">
          <p className="text-sm font-bold uppercase tracking-widest text-accent">{player.mode || 'ничего не играет'}</p>
          <h2 className="mt-2 truncate font-display text-3xl font-bold sm:text-5xl">{player.current?.title || 'Выбери микс'}</h2>
          <p className="mt-3 text-lg text-muted">{player.current ? [player.current.artist, player.current.album].filter(Boolean).join(' · ') : 'или запусти радио на главной'}</p>
          <p className="mt-2 text-sm text-muted">{maturityLabels[player.maturity ?? ''] ?? 'твой локальный микс'}</p>

          <PlayerSeek currentDuration={player.current?.duration} disabled={!player.current} />

          <div className="mt-6 flex flex-wrap items-center justify-center gap-3">
            <IconButton aria-label="Добавить в «Потом»" disabled={!player.current || player.busy} onClick={() => void addLater()}>◷</IconButton>
            <IconButton aria-label="Не нравится" disabled={!player.current || player.busy || rating === 'dislike'} onClick={() => void dislike()} pressed={rating === 'dislike'}>✕</IconButton>
            <IconButton aria-label={player.playing ? 'Пауза' : 'Воспроизвести'} className="size-16 border-accent bg-accent text-2xl text-bg" disabled={!player.current || player.busy} onClick={() => void player.togglePlay().catch(report)}>{player.playing ? '❚❚' : '▶'}</IconButton>
            <IconButton aria-label="Следующий трек" disabled={!player.current || player.busy} onClick={() => void player.skip().catch(report)}>⏭</IconButton>
            <IconButton aria-label={favorite ? 'Убрать из любимых песен' : 'Добавить в любимые песни'} disabled={!player.current || favorites.loading} onClick={() => currentId !== undefined && void favorites.toggleFavorite({ track_id: currentId }).catch(report)} pressed={favorite}>♥</IconButton>
          </div>

          <div className="mt-5 flex items-center justify-center gap-3">
            <IconButton aria-label={player.muted ? 'Включить звук' : 'Выключить звук'} onClick={player.toggleMute} pressed={player.muted}>{player.muted || player.volume === 0 ? '🔇' : '🔊'}</IconButton>
            <label className="sr-only" htmlFor="player-volume">Громкость</label>
            <input className="min-h-11 w-full max-w-52 accent-accent" id="player-volume" max="100" min="0" onChange={(event) => player.setVolume(Number(event.target.value))} step="5" type="range" value={player.volume} />
            <output className="w-12 text-sm text-muted" htmlFor="player-volume">{player.volume}%</output>
          </div>

          <div className="mt-5 flex flex-wrap justify-center gap-2">
            {player.fixed && <Button aria-label="Перемешать текущий плейлист" disabled={player.busy} onClick={() => void player.shuffle().catch(report)} variant="quiet">⇄ Перемешать</Button>}
            <Button className="min-h-11" disabled={!player.current?.artist} onClick={() => player.current?.artist && void favorites.toggleFavorite({ type: 'artist', artist: player.current.artist, track_id: player.current.id }).catch(report)} variant="quiet">{artistFavorite ? '♥' : '♡'} артист</Button>
            <Button className="min-h-11" disabled={!player.current?.album} onClick={() => player.current?.album && void favorites.toggleFavorite({ type: 'album', artist: player.current.artist, album: player.current.album, track_id: player.current.id }).catch(report)} variant="quiet">{albumFavorite ? '♥' : '♡'} альбом</Button>
          </div>
        </div>
      </section>

      <details className="rounded-app border border-line bg-bg2/60 p-4">
        <summary className="min-h-11 cursor-pointer py-2 font-semibold">Текст песни</summary>
        <Lyrics trackId={currentId} />
      </details>

      <section aria-labelledby="playlist-label">
        <div className="mb-4 flex items-baseline justify-between"><h2 className="font-display text-2xl font-bold" id="playlist-label">{player.fixed ? 'Треки плейлиста' : 'Далее в радио'}</h2><span className="text-sm text-muted">{player.fixed ? player.tracks.length : player.queue.length || ''}</span></div>
        {player.fixed ? <Playlist current={player.current} currentIndex={player.currentIndex} disabled={player.busy} onSelect={(track, index) => void player.selectTrack(track, index).catch(report)} tracks={player.tracks} /> : <Queue queue={player.queue} />}
      </section>
    </div>
  )
}

function PlayerSeek({ currentDuration, disabled }: { currentDuration?: number; disabled: boolean }) {
  const player = usePlayer()
  const progress = usePlayerProgress()
  const shownPosition = progress.seeking ? (progress.seekValue / 1_000) * progress.duration : progress.position

  return <>
    <label className="sr-only" htmlFor="player-seek">Прогресс</label>
    <input className="mt-8 min-h-11 w-full accent-accent" disabled={disabled || !progress.duration} id="player-seek" max="1000" min="0" onChange={(event) => player.previewSeek(Number(event.target.value))} onKeyDown={() => player.beginSeek()} onKeyUp={(event) => player.commitSeek(Number(event.currentTarget.value))} onMouseDown={player.beginSeek} onPointerUp={(event) => player.commitSeek(Number(event.currentTarget.value))} onTouchStart={player.beginSeek} step="1" type="range" value={progress.seekValue} />
    <div className="flex justify-between text-sm text-muted"><span>{formatTime(shownPosition)}</span><span>{formatTime(progress.duration || currentDuration || 0)}</span></div>
  </>
}

const Lyrics = memo(function Lyrics({ trackId }: { trackId?: number }) {
  const lyrics = useApiData<LyricsResponse>(trackId, (signal) => apiFetch(`/api/tracks/${trackId}/lyrics`, { signal }))
  if (lyrics.status === 'idle') return <pre className="mt-3 whitespace-pre-wrap font-sans text-muted">—</pre>
  if (lyrics.status === 'loading') return <p className="mt-3 text-muted" role="status">Загружаем текст…</p>
  if (lyrics.status === 'error') return <ErrorState className="mt-3" description={lyrics.error.message} onRetry={lyrics.retry} title="Не удалось загрузить текст" />
  const text = lyrics.data.status === 'absent' || lyrics.data.status === 'missing' ? 'Текста нет (music-hive lyrics)'
    : lyrics.data.instrumental ? '(instrumental)' : lyrics.data.plain_lyrics || lyrics.data.synced_lyrics || '—'
  return <pre className="mt-3 max-h-96 overflow-auto whitespace-pre-wrap font-sans leading-relaxed">{text}</pre>
})

const Playlist = memo(function Playlist({ tracks, current, currentIndex, disabled, onSelect }: { tracks: Track[]; current: Track | null; currentIndex: number; disabled: boolean; onSelect: (track: Track, index: number) => void }) {
  if (!tracks.length) return <EmptyState title="Плейлист пуст" description="Выбери трек или микс в библиотеке." />
  return <ol className="grid gap-2">{tracks.map((track, index) => {
    const active = track.current || index === currentIndex || track.id === current?.id
    return <li key={`${track.id}-${index}`}><button aria-current={active ? 'true' : undefined} className="grid min-h-14 w-full grid-cols-[2rem_1fr_auto] items-center gap-3 rounded-xl border border-transparent px-3 py-2 text-left hover:bg-line-soft aria-[current=true]:border-accent aria-[current=true]:bg-accent/10" disabled={disabled} onClick={() => onSelect(track, index)} type="button">
      <span className="text-sm text-muted">{index + 1}</span><span className="min-w-0"><strong className="block truncate">{track.title || `#${track.id}`}</strong><span className="block truncate text-sm text-muted">{[track.artist, track.album].filter(Boolean).join(' · ')}</span></span><span className="text-sm text-muted">{formatTime(track.duration || 0)}</span>
    </button></li>
  })}</ol>
})

const Queue = memo(function Queue({ queue }: { queue: QueueItem[] }) {
  if (!queue.length) return <EmptyState title="Очередь пуста" description="Запусти радио, чтобы подобрать следующие треки." />
  return <ol className="grid gap-2">{queue.map((track, index) => <li className="min-h-14 rounded-xl border border-line px-4 py-3" key={`${track.id}-${index}`}>
    <strong>{track.artist || '—'}</strong> — {track.title || `#${track.id}`}
    {track.explore && <span className="ml-2 rounded-full bg-accent/20 px-2 py-0.5 text-xs text-accent">far</span>}
    {track.new_boost && <span className="ml-2 rounded-full bg-accent2/20 px-2 py-0.5 text-xs text-accent2">new</span>}
    {track.explanation && <span className="mt-1 block text-sm text-muted">{track.explanation}</span>}
  </li>)}</ol>
})
