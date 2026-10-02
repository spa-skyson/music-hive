import { useEffect, useRef, useState, type ReactNode } from 'react'
import { apiFetch } from '../api/client.ts'
import { albumKey, groupCatalog, useApiData, useFavorites, useJobs, useToast, type BackgroundJob } from '../data/index.ts'
import { usePlayer } from '../player/context.ts'
import { formatTime, type Track } from '../player/state.ts'
import { usePlayAction } from '../player/usePlayAction.ts'
import { Button, EmptyState, EntityCard, ErrorState, SkeletonCards } from '../ui/index.ts'
import { Shelf } from './home/Shelf.tsx'
import { jobProgressLabel, mixMeta, WEEKDAY_RU, type MixCardData } from './home/model.ts'
import { LIBRARY_TAB_KEY, type LibraryTab, type LibraryTrack } from './library/model.ts'

interface MixesResponse { mixes?: MixCardData[]; today_weekday?: string }
interface RecommendEntity { artist: string; album?: string; tracks?: number; artwork?: string; explanation?: string }
interface RecommendTrack extends Track { explanation?: string }
interface Recommendations { empty?: boolean; explanation?: string; tracks?: RecommendTrack[]; artists?: RecommendEntity[]; albums?: RecommendEntity[] }
interface Tip { artist?: string; album?: string; explanation?: string; track_ids?: number[] }
interface TipsResponse { tips?: Tip[] }
interface JobStart { id?: number; job_id?: number }
interface ShareResponse { url?: string; url_bare?: string }

export function HomeView() {
  const { libraryVersion } = useJobs()
  const mixes = useApiData(`mixes-${libraryVersion}`, (signal) => apiFetch<MixesResponse>('/api/mixes', { signal }))
  const library = useApiData(`home-library-${libraryVersion}`, (signal) => apiFetch<LibraryTrack[]>('/api/library', { signal }))
  const recommendations = useApiData(`recommendations-${libraryVersion}`, (signal) => apiFetch<Recommendations>('/api/recommend/favorites', { signal }))
  const favorites = useFavorites()
  const player = usePlayer()
  const { showToast } = useToast()
  const { act, play } = usePlayAction()
  const polling = useRef(true)
  const [refreshing, setRefreshing] = useState(false)
  const [jobStatus, setJobStatus] = useState('')
  const [shareUrl, setShareUrl] = useState('')
  const [tips, setTips] = useState<{ status: 'idle' | 'loading' | 'error' | 'data'; items: Tip[]; error?: string }>({ status: 'idle', items: [] })

  useEffect(() => () => { polling.current = false }, [])

  const playMix = (kind: string, trackId?: number) => play(() => player.playMix(kind, trackId))
  const playFixed = (request: Parameters<typeof player.play>[0]) => play(() => player.play(request))

  async function refreshMixes() {
    setRefreshing(true)
    setJobStatus('Миксы генерируются в фоне')
    try {
      const started = await apiFetch<JobStart>('/api/jobs/mix_pack', { method: 'POST', body: '{}' })
      const id = started.id ?? started.job_id
      if (!id) throw new Error('Сервер не вернул номер задачи')
      showToast('Миксы поставлены в очередь')
      let failures = 0
      for (let attempt = 0; attempt <= 40 && polling.current; attempt += 1) {
        await delay([3_000, 6_000, 12_000, 30_000][failures])
        if (!polling.current) return
        if (document.hidden) await visibleAgain()
        try {
          const job = await apiFetch<BackgroundJob>(`/api/jobs/${id}`)
          failures = 0
          setJobStatus(jobProgressLabel(job))
          if (job.status === 'done') {
            setJobStatus('Готово')
            showToast('Миксы обновлены')
            mixes.retry()
            return
          }
          if (job.status === 'failed') throw new Error(job.error || 'Не удалось обновить миксы')
        } catch (cause) {
          failures = Math.min(failures + 1, 3)
          if (cause instanceof Error && cause.message !== 'Failed to fetch' && !/network/i.test(cause.message)) throw cause
        }
      }
      mixes.retry()
      showToast('Проверь полки — возможно уже готово')
    } catch (cause) {
      showToast(cause instanceof Error ? cause.message : 'Не удалось обновить миксы', { variant: 'error' })
    } finally {
      setRefreshing(false)
      window.setTimeout(() => setJobStatus(''), 1_500)
    }
  }

  async function shareRadio() {
    try {
      const response = await apiFetch<ShareResponse>('/api/share/radio', { method: 'POST', body: JSON.stringify({ name: 'music-hive radio' }) })
      const url = response.url ?? response.url_bare
      if (!url) throw new Error('Сервер не вернул ссылку')
      setShareUrl(url)
      try { await navigator.clipboard.writeText(url); showToast('Ссылка скопирована') } catch { showToast('Ссылка создана') }
    } catch (cause) {
      showToast(cause instanceof Error ? cause.message : 'Не удалось создать ссылку', { variant: 'error' })
    }
  }

  async function showTips(kind: 'new' | 'old') {
    setTips({ status: 'loading', items: [] })
    try {
      const path = kind === 'new' ? '/api/discover/albums' : '/api/discover/resurfaced'
      const response = await apiFetch<TipsResponse>(path)
      setTips({ status: 'data', items: response.tips ?? [] })
    } catch (cause) {
      setTips({ status: 'error', items: [], error: cause instanceof Error ? cause.message : 'Не удалось загрузить открытия' })
    }
  }

  const loadError = mixes.status === 'error' ? mixes : library.status === 'error' ? library : recommendations.status === 'error' ? recommendations : null
  if (loadError) return <ErrorState className="mt-6" description={loadError.error.message} onRetry={loadError.retry} title="Не удалось загрузить главную" />
  if (!mixes.data || !library.data || !recommendations.data || favorites.loading) return <div className="mt-6"><SkeletonCards count={5} label="Загрузка главной" /></div>
  if (favorites.error) return <ErrorState className="mt-6" description={favorites.error.message} onRetry={() => void favorites.reload()} title="Не удалось загрузить избранное" />

  const { artists, albums } = groupCatalog(library.data)
  const allMixes = mixes.data.mixes ?? []
  const primaryMixes = allMixes.filter(({ kind }) => !kind.startsWith('weekday_'))
  const weekdayMixes = allMixes.filter(({ kind }) => kind.startsWith('weekday_'))
  const favoriteTracks = favorites.tracks.map((row) => row.track ?? { id: row.track_id, title: row.title, artist: row.artist, duration: row.duration })
  const recs = recommendations.data

  return (
    <div className="mt-6 grid gap-10">
      <section className="grid gap-5 rounded-app border border-line bg-gradient-to-br from-accent/15 to-bg2 p-6 sm:grid-cols-[1fr_auto]">
        <div><p className="text-sm font-semibold uppercase tracking-wider text-accent">Твоя музыка · твои правила</p><h2 className="mt-2 font-display text-3xl font-bold">Что послушаем?</h2><p className="mt-2 text-muted">Запусти персональное радио или выбери настроение из коллекции ниже.</p></div>
        <div className="flex flex-wrap content-start gap-2 sm:justify-end"><Button disabled={player.busy} onClick={() => void play(() => player.startRadio())} variant="primary">▶ Запустить радио</Button><Button onClick={() => void shareRadio()}>Поделиться</Button><Button onClick={() => void refreshMixes()} pending={refreshing}>Обновить миксы</Button></div>
        <div aria-live="polite" className="grid gap-1 text-sm sm:col-span-2" role="status">{jobStatus && <p className="text-accent2">{jobStatus}</p>}{shareUrl && <a className="break-all text-accent underline" href={shareUrl} rel="noopener noreferrer" target="_blank">{shareUrl}</a>}</div>
      </section>

      <Shelf hint={mixes.data.today_weekday ? `сегодня · ${WEEKDAY_RU[mixes.data.today_weekday] ?? mixes.data.today_weekday}` : undefined} title="Для тебя">
        {primaryMixes.map((mix) => <MixCard key={mix.kind} mix={mix} onPlay={() => playMix(mix.kind)} />)}
      </Shelf>
      <Shelf hint="сегодня подсвечен" title="Дни недели">
        {weekdayMixes.map((mix) => <MixCard key={mix.kind} mix={mix} onPlay={() => playMix(mix.kind)} />)}
      </Shelf>
      <Shelf hint={`${favorites.counts.tracks} песен · ${favorites.counts.artists} артистов · ${favorites.counts.albums} альбомов`} title="Любимые песни">
        {favoriteTracks.length ? favoriteTracks.map((track) => <HomeEntity favorite key={track.id} kind="track" meta={formatTime(track.duration ?? 0)} onFavorite={() => act(() => favorites.toggleFavorite({ type: 'track', track_id: track.id }, { withLike: false }))} onPlay={() => playMix('favorites', track.id)} subtitle={track.artist} title={track.title || 'Без названия'} artwork={track.artwork} />) : <ShelfEmpty description="Жми ♥ в плеере — песня появится здесь" title="Пока пусто" />}
      </Shelf>
      <Shelf hint="♥ на карточке артиста" title="Любимые артисты">
        {favorites.artists.length ? favorites.artists.map((artist) => <HomeEntity artwork={artist.artwork} favorite key={artist.artist} kind="artist" meta={`${artist.tracks} треков`} onFavorite={() => act(() => favorites.toggleFavorite({ type: 'artist', artist: artist.artist }, { withLike: false }))} onPlay={() => playFixed({ artist: artist.artist })} onShuffle={() => playFixed({ artist: artist.artist, shuffle: true })} subtitle="артист" title={artist.artist} />) : <ShelfEmpty description="♥ на карточке артиста" title="Нет любимых артистов" />}
      </Shelf>
      <Shelf hint="♥ на карточке альбома" title="Любимые альбомы">
        {favorites.albums.length ? favorites.albums.map((album) => <HomeEntity artwork={album.artwork} favorite key={albumKey(album.artist, album.album)} kind="album" meta={`${album.tracks} треков`} onFavorite={() => act(() => favorites.toggleFavorite({ type: 'album', artist: album.artist, album: album.album }, { withLike: false }))} onPlay={() => playFixed({ artist: album.artist, album: album.album })} onShuffle={() => playFixed({ artist: album.artist, album: album.album, shuffle: true })} subtitle={album.artist} title={album.album} />) : <ShelfEmpty description="♥ на карточке альбома" title="Нет любимых альбомов" />}
      </Shelf>
      <Shelf hint={recs.empty ? 'сначала добавь любимое' : recs.explanation || 'по звучанию'} title="Похожее на любимое">
        {recs.empty || !(recs.tracks?.length || recs.artists?.length || recs.albums?.length) ? <ShelfEmpty description="Добавь любимые песни, артистов или альбомы" title="Мало данных" /> : <RecommendationCards data={recs} favorites={favorites} onAction={act} play={playFixed} />}
      </Shelf>
      <Shelf action={<LibraryLink tab="artists">все артисты</LibraryLink>} title="Артисты">
        {artists.slice(0, 28).map((artist) => <HomeEntity artwork={artist.cover ?? undefined} favorite={favorites.artistNames.has(artist.artist)} key={artist.artist} kind="artist" meta={`${artist.tracks} треков`} onFavorite={() => act(() => favorites.toggleFavorite({ type: 'artist', artist: artist.artist }, { withLike: false }))} onPlay={() => playFixed({ artist: artist.artist })} onShuffle={() => playFixed({ artist: artist.artist, shuffle: true })} subtitle="артист" title={artist.artist} />)}
      </Shelf>
      <Shelf action={<LibraryLink tab="albums">все альбомы</LibraryLink>} title="Альбомы">
        {albums.slice(0, 28).map((album) => <HomeEntity artwork={album.cover ?? undefined} favorite={favorites.albumKeys.has(albumKey(album.artist, album.album))} key={albumKey(album.artist, album.album)} kind="album" meta={`${album.tracks} треков`} onFavorite={() => act(() => favorites.toggleFavorite({ type: 'album', artist: album.artist, album: album.album }, { withLike: false }))} onPlay={() => playFixed({ artist: album.artist, album: album.album })} onShuffle={() => playFixed({ artist: album.artist, album: album.album, shuffle: true })} subtitle={album.artist} title={album.album} />)}
      </Shelf>
      <Shelf action={<LibraryLink tab="tracks">вся библиотека</LibraryLink>} title="Треки">
        {library.data.slice(0, 36).map((track) => <HomeEntity artwork={track.artwork} favorite={favorites.trackIds.has(track.id)} key={track.id} kind="track" meta={formatTime(track.duration)} onFavorite={() => act(() => favorites.toggleFavorite({ type: 'track', track_id: track.id }, { withLike: false }))} onPlay={() => playFixed({ track_id: track.id, name: track.title })} subtitle={track.artist} title={track.title} />)}
      </Shelf>
      <Shelf title="Открытия"><DiscoveryCard description="Что недавно попало в библиотеку" onClick={() => void showTips('new')} title="Новые альбомы" /><DiscoveryCard description="Забытое, но близко к вкусу" onClick={() => void showTips('old')} title="Из старого" /></Shelf>
      {tips.status !== 'idle' && <TipsPanel onPlay={(tip) => playFixed({ track_ids: tip.track_ids, name: `${tip.artist || ''} — ${tip.album || 'альбом'}`.trim() })} tips={tips} />}
    </div>
  )
}

type Favorites = ReturnType<typeof useFavorites>

function RecommendationCards({ data, favorites, onAction, play }: { data: Recommendations; favorites: Favorites; onAction: (action: () => Promise<unknown>) => Promise<void>; play: (request: Parameters<ReturnType<typeof usePlayer>['play']>[0]) => Promise<void> }) {
  return <>{data.tracks?.slice(0, 16).map((track) => <HomeEntity artwork={track.artwork} favorite={favorites.trackIds.has(track.id)} key={`track-${track.id}`} kind="track" meta={track.explanation} onFavorite={() => onAction(() => favorites.toggleFavorite({ type: 'track', track_id: track.id }, { withLike: false }))} onPlay={() => play({ track_id: track.id, name: track.title })} subtitle={track.artist} title={track.title || 'Без названия'} />)}{data.artists?.slice(0, 6).map((artist) => <HomeEntity artwork={artist.artwork} favorite={favorites.artistNames.has(artist.artist)} key={`artist-${artist.artist}`} kind="artist" meta={artist.explanation || `${artist.tracks ?? 0} треков`} onFavorite={() => onAction(() => favorites.toggleFavorite({ type: 'artist', artist: artist.artist }, { withLike: false }))} onPlay={() => play({ artist: artist.artist })} onShuffle={() => play({ artist: artist.artist, shuffle: true })} subtitle="артист" title={artist.artist} />)}{data.albums?.slice(0, 6).map((album) => <HomeEntity artwork={album.artwork} favorite={favorites.albumKeys.has(albumKey(album.artist, album.album || ''))} key={`album-${albumKey(album.artist, album.album || '')}`} kind="album" meta={album.explanation || `${album.tracks ?? 0} треков`} onFavorite={() => onAction(() => favorites.toggleFavorite({ type: 'album', artist: album.artist, album: album.album }, { withLike: false }))} onPlay={() => play({ artist: album.artist, album: album.album })} onShuffle={() => play({ artist: album.artist, album: album.album, shuffle: true })} subtitle={album.artist} title={album.album || 'Без альбома'} />)}</>
}

function HomeEntity(props: Parameters<typeof EntityCard>[0]) { return <EntityCard {...props} /> }
function ShelfEmpty({ title, description }: { title: string; description: string }) { return <EmptyState className="w-64" description={description} title={title} /> }
function MixCard({ mix, onPlay }: { mix: MixCardData; onPlay: () => void | Promise<void> }) { return <EntityCard artwork={mix.cover_track_id ? `/api/artwork/${mix.cover_track_id}` : undefined} className={mix.today ? 'border-accent ring-2 ring-accent/40' : !mix.ready && mix.kind !== 'later' && mix.kind !== 'favorites' ? 'opacity-60' : undefined} kind="album" meta={mixMeta(mix)} onPlay={() => void onPlay()} subtitle={mix.subtitle} title={mix.title} /> }
function LibraryLink({ tab, children }: { tab: LibraryTab; children: ReactNode }) { return <a className="inline-flex min-h-11 items-center text-sm text-accent underline-offset-4 hover:underline" href="#/library" onClick={() => sessionStorage.setItem(LIBRARY_TAB_KEY, tab)}>{children}</a> }
function DiscoveryCard({ title, description, onClick }: { title: string; description: string; onClick: () => void }) { return <button className="min-h-48 rounded-app border border-line bg-gradient-to-br from-accent2/30 to-bg2 p-4 text-left outline-none hover:border-accent focus-visible:ring-2 focus-visible:ring-accent" onClick={onClick} type="button"><strong className="block text-lg">{title}</strong><span className="mt-2 block text-sm text-muted">{description}</span><span className="mt-4 block text-xs text-accent2">смотреть</span></button> }

function TipsPanel({ tips, onPlay }: { tips: { status: string; items: Tip[]; error?: string }; onPlay: (tip: Tip) => Promise<void> }) {
  if (tips.status === 'loading') return <p aria-live="polite" className="text-muted" role="status">Загрузка открытий…</p>
  if (tips.status === 'error') return <p className="text-red-300" role="alert">{tips.error}</p>
  if (!tips.items.length) return <EmptyState description="Обнови миксы или добавь музыку в библиотеку." title="Пусто" />
  return <section aria-label="Подборка открытий" className="grid gap-3">{tips.items.map((tip, index) => <article className="flex flex-wrap items-center justify-between gap-4 rounded-xl border border-line bg-bg2 p-4" key={`${tip.artist}-${tip.album}-${index}`}><div><h3 className="font-semibold">{tip.artist || 'Неизвестный артист'} — {tip.album || 'Без альбома'}</h3><p className="text-sm text-muted">{tip.explanation}</p></div><Button disabled={!tip.track_ids?.length} onClick={() => void onPlay(tip)} variant="primary">Слушать альбом</Button></article>)}</section>
}

function delay(milliseconds: number) { return new Promise<void>((resolve) => window.setTimeout(resolve, milliseconds)) }
function visibleAgain() { return document.hidden ? new Promise<void>((resolve) => document.addEventListener('visibilitychange', () => resolve(), { once: true })) : Promise.resolve() }
