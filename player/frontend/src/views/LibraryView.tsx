import { useDeferredValue, useEffect, useState } from 'react'
import { apiFetch } from '../api/client.ts'
import { albumKey, groupCatalog, useApiData, useFavorites, useJobs, useToast } from '../data/index.ts'
import { usePlayer } from '../player/context.ts'
import { formatTime } from '../player/state.ts'
import { usePlayAction } from '../player/usePlayAction.ts'
import { Button, EmptyState, EntityCard, ErrorState, Field, SkeletonCards } from '../ui/index.ts'
import {
  filterAlbums, filterArtists, filterTracks, isLibraryTab, LIBRARY_TAB_KEY,
  libraryPlaceholder, type LibraryTab, type LibraryTrack,
} from './library/model.ts'
import { requestLibraryRescan } from './library/rescan.ts'

const tabs: ReadonlyArray<{ value: LibraryTab; label: string }> = [
  { value: 'tracks', label: 'Треки' }, { value: 'artists', label: 'Артисты' },
  { value: 'albums', label: 'Альбомы' }, { value: 'favorites', label: 'Избранное' },
]

export function LibraryView() {
  const [tab, setTabValue] = useState<LibraryTab>(() => {
    const stored = sessionStorage.getItem(LIBRARY_TAB_KEY)
    return isLibraryTab(stored) ? stored : 'tracks'
  })
  const [query, setQuery] = useState('')
  const deferredQuery = useDeferredValue(query)
  const { libraryVersion } = useJobs()
  const { showToast } = useToast()
  const library = useApiData(`library-${libraryVersion}`, (signal) => apiFetch<LibraryTrack[]>('/api/library', { signal }))
  const favorites = useFavorites()
  const player = usePlayer()
  const { act, addToLater, play } = usePlayAction()
  const [scanBusy, setScanBusy] = useState(false)
  const [scanStatus, setScanStatus] = useState('')

  useEffect(() => sessionStorage.removeItem(LIBRARY_TAB_KEY), [])

  async function rescan(full: boolean) {
    if (full && !window.confirm('Полное сканирование идёт долго (обход библиотеки, эмбеддинги, миксы). Не запускай без нужды. Продолжить?')) return
    setScanBusy(true)
    try {
      const message = await requestLibraryRescan(full)
      setScanStatus(message)
      showToast(message)
    } catch (cause) {
      const message = `Не удалось поставить обновление: ${cause instanceof Error ? cause.message : String(cause)}`
      setScanStatus(message)
      showToast(message, { variant: 'error' })
    } finally {
      setScanBusy(false)
    }
  }

  function setTab(next: LibraryTab) {
    setTabValue(next)
  }

  const playRequest = (request: Parameters<typeof player.play>[0]) => play(() => player.play(request))
  const startRadio = (trackId: number) => play(() => player.startRadio(trackId))

  if (!library.data || favorites.loading) return <div className="mt-6"><SkeletonCards count={4} /></div>
  if (library.status === 'error') return <ErrorState className="mt-6" description={library.error.message} onRetry={library.retry} title="Не удалось загрузить библиотеку" />
  if (favorites.error) return <ErrorState className="mt-6" description={favorites.error.message} onRetry={() => void favorites.reload()} title="Не удалось загрузить избранное" />

  const tracks = library.data
  const { artists, albums } = groupCatalog(tracks)
  const source = tab === 'favorites' ? tracks.filter(({ id }) => favorites.trackIds.has(id)) : tracks
  const shownTracks = filterTracks(source, deferredQuery).slice(0, 400)
  const shownArtists = filterArtists(artists, deferredQuery)
  const shownAlbums = filterAlbums(albums, deferredQuery)

  return (
    <div className="mt-6 grid gap-6">
      <p aria-live="polite" className="text-sm text-muted">
        {tracks.length} треков · {artists.length} артистов · {albums.length} альбомов · {favorites.trackIds.size} ♥
      </p>
      <div className="flex flex-wrap items-center gap-2">
        <Button className="focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent" disabled={scanBusy} onClick={() => void rescan(false)}>Обновить</Button>
        <Button className="focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent" disabled={scanBusy} onClick={() => void rescan(true)}>Полное сканирование</Button>
        <p aria-live="polite" className="text-sm text-muted">{scanStatus}</p>
      </div>
      <div aria-label="Разделы библиотеки" className="flex flex-wrap gap-2" role="tablist">
        {tabs.map(({ value, label }) => (
          <Button aria-controls="library-content" aria-selected={tab === value} key={value} onClick={() => setTab(value)} role="tab" variant={tab === value ? 'primary' : 'secondary'}>{label}</Button>
        ))}
      </div>
      <Field autoComplete="off" label="Поиск в библиотеке" onChange={(event) => setQuery(event.currentTarget.value)} placeholder={libraryPlaceholder(tab)} type="search" value={query} />
      <div aria-live="polite" id="library-content" role="tabpanel">
        {(tab === 'tracks' || tab === 'favorites') && (
          shownTracks.length ? (
            <div className="grid gap-3">
              {tab === 'favorites' && <Button aria-label="Слушать вперемешку: Избранное" className="justify-self-start" onClick={() => void playRequest({ track_ids: source.map(({ id }) => id), name: 'Избранное', shuffle: true })}>⇄ Слушать вперемешку</Button>}
              <ul className="grid gap-2" role="list">
              {shownTracks.map((track) => (
                <li className="grid items-center gap-2 rounded-xl border border-line bg-bg2 p-2 sm:grid-cols-[44px_minmax(0,1fr)_auto]" key={track.id}>
                  {track.artwork ? <img alt="" className="size-11 rounded-lg object-cover" decoding="async" height="44" loading="lazy" src={track.artwork} width="44" /> : <span aria-hidden="true" className="grid size-11 place-items-center rounded-lg bg-line-soft">♪</span>}
                  <button className="min-h-11 min-w-0 text-left outline-none hover:text-accent focus-visible:ring-2 focus-visible:ring-accent" onClick={() => void playRequest({ track_id: track.id, name: track.title })} type="button">
                    <span className="block truncate font-semibold">{favorites.trackIds.has(track.id) && '♥ '}{track.ready === false && '… '}{track.artist} — {track.title}</span>
                    <span className="text-xs text-muted">{track.album || 'Без альбома'} · {formatTime(track.duration)}</span>
                  </button>
                  <div className="flex flex-wrap gap-1 sm:justify-end">
                    <TrackAction label="Трек" onClick={() => playRequest({ track_id: track.id, name: track.title })} />
                    <TrackAction label={favorites.trackIds.has(track.id) ? 'Убрать ♥' : '♥'} onClick={() => act(() => favorites.toggleFavorite({ type: 'track', track_id: track.id }, { withLike: false }))} />
                    <TrackAction disabled={!track.album} label="Альбом" onClick={() => playRequest({ artist: track.artist, album: track.album })} />
                    <TrackAction ariaLabel={`Перемешать альбом: ${track.album || 'альбом'}`} disabled={!track.album} label="⇄ Альбом" onClick={() => playRequest({ artist: track.artist, album: track.album, shuffle: true })} />
                    <TrackAction disabled={!track.artist} label="Артист" onClick={() => playRequest({ artist: track.artist })} />
                    <TrackAction ariaLabel={`Перемешать артиста: ${track.artist || 'артист'}`} disabled={!track.artist} label="⇄ Артист" onClick={() => playRequest({ artist: track.artist, shuffle: true })} />
                    <TrackAction label="Потом" onClick={() => addToLater(track.id, 'В «Потом»')} />
                    <TrackAction label="Радио" onClick={() => startRadio(track.id)} />
                  </div>
                </li>
              ))}
              </ul>
            </div>
          ) : <LibraryEmpty favorites={tab === 'favorites'} query={query} />
        )}
        {tab === 'artists' && (shownArtists.length ? (
          <div className="grid grid-cols-[repeat(auto-fill,minmax(11rem,1fr))] gap-4">
            {shownArtists.map((artist) => <EntityCard artwork={artist.cover ?? undefined} key={artist.artist} kind="artist" meta={`${artist.tracks} треков`} onPlay={() => void playRequest({ artist: artist.artist })} onShuffle={() => void playRequest({ artist: artist.artist, shuffle: true })} subtitle="артист" title={artist.artist} />)}
          </div>
        ) : <LibraryEmpty query={query} />)}
        {tab === 'albums' && (shownAlbums.length ? (
          <div className="grid grid-cols-[repeat(auto-fill,minmax(11rem,1fr))] gap-4">
            {shownAlbums.map((album) => <EntityCard artwork={album.cover ?? undefined} key={albumKey(album.artist, album.album)} kind="album" meta={`${album.tracks} треков`} onPlay={() => void playRequest({ artist: album.artist, album: album.album })} onShuffle={() => void playRequest({ artist: album.artist, album: album.album, shuffle: true })} subtitle={album.artist} title={album.album} />)}
          </div>
        ) : <LibraryEmpty query={query} />)}
      </div>
    </div>
  )
}

function TrackAction({ label, onClick, disabled = false, ariaLabel }: { label: string; onClick: () => void | Promise<void>; disabled?: boolean; ariaLabel?: string }) {
  return <button aria-label={ariaLabel} className="min-h-11 rounded-lg border border-line px-2 text-xs outline-none hover:border-accent focus-visible:ring-2 focus-visible:ring-accent" disabled={disabled} onClick={() => void onClick()} type="button">{label}</button>
}

function LibraryEmpty({ favorites = false, query = '' }: { favorites?: boolean; query?: string }) {
  const title = query.trim() ? 'Ничего не найдено' : favorites ? 'Избранное пусто' : 'Пока пусто'
  const description = query.trim() ? 'Измени поисковый запрос.' : favorites ? 'Жми ♥ в плеере.' : 'Добавь музыку во вкладке «Загрузка».'
  return <EmptyState description={description} title={title} />
}
