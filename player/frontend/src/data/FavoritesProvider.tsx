import { useEffect, useState, type ReactNode } from 'react'
import { apiFetch } from '../api/client.ts'
import { albumKey } from './catalog.ts'
import {
  FavoritesContext,
  type FavoriteCounts,
  type FavoriteRequest,
  type FavoritesData,
} from './favoritesContext.ts'
import { useToast } from './toastContext.ts'

interface ToggleResponse {
  type: 'track' | 'artist' | 'album'
  favorited: boolean
  track_id?: number
  artist?: string
  album?: string
  count?: number
  counts?: FavoriteCounts
}

const emptyData: FavoritesData = {
  tracks: [], artists: [], albums: [], ids: [], count: 0,
  counts: { tracks: 0, artists: 0, albums: 0 },
}

function normalize(data: Partial<FavoritesData>): FavoritesData {
  const tracks = data.tracks ?? []
  const artists = data.artists ?? []
  const albums = data.albums ?? []
  const ids = (data.ids ?? tracks.map(({ track_id }) => track_id)).map(Number)
  return {
    tracks, artists, albums, ids,
    count: data.count ?? ids.length,
    counts: data.counts ?? { tracks: data.count ?? ids.length, artists: artists.length, albums: albums.length },
  }
}

function toggleMessage(type: ToggleResponse['type'], favorited: boolean): string {
  if (type === 'track') return favorited ? 'Любимая песня' : 'Песня убрана из любимых'
  if (type === 'artist') return favorited ? 'Любимый артист' : 'Артист убран'
  return favorited ? 'Любимый альбом' : 'Альбом убран'
}

export function FavoritesProvider({ children, enabled = true, onTrackLiked }: { children: ReactNode; enabled?: boolean; onTrackLiked?: (trackId: number) => void | Promise<void> }) {
  const { showToast } = useToast()
  const [data, setData] = useState(emptyData)
  const [trackIds, setTrackIds] = useState<ReadonlySet<number>>(new Set())
  const [artistNames, setArtistNames] = useState<ReadonlySet<string>>(new Set())
  const [albumKeys, setAlbumKeys] = useState<ReadonlySet<string>>(new Set())
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<Error | null>(null)

  async function reload(silent = false) {
    if (!enabled) return
    if (!silent) {
      setLoading(true)
      setError(null)
    }
    try {
      const next = normalize(await apiFetch<Partial<FavoritesData>>('/api/favorites'))
      setData(next)
      setTrackIds(new Set(next.ids))
      setArtistNames(new Set(next.artists.map(({ artist }) => artist)))
      setAlbumKeys(new Set(next.albums.map(({ artist, album }) => albumKey(artist, album))))
    } catch (cause) {
      if (!silent) setError(cause instanceof Error ? cause : new Error('Не удалось загрузить избранное'))
      throw cause
    } finally {
      if (!silent) setLoading(false)
    }
  }

  useEffect(() => {
    if (!enabled) return
    let active = true
    apiFetch<Partial<FavoritesData>>('/api/favorites').then(
      (response) => {
        if (!active) return
        const next = normalize(response)
        setData(next)
        setTrackIds(new Set(next.ids))
        setArtistNames(new Set(next.artists.map(({ artist }) => artist)))
        setAlbumKeys(new Set(next.albums.map(({ artist, album }) => albumKey(artist, album))))
        setLoading(false)
      },
      (cause: unknown) => {
        if (!active) return
        setError(cause instanceof Error ? cause : new Error('Не удалось загрузить избранное'))
        setLoading(false)
      },
    )
    return () => { active = false }
  }, [enabled])

  async function toggleFavorite(request: FavoriteRequest, options: { withLike?: boolean } = {}) {
    const response = await apiFetch<ToggleResponse>('/api/favorites/toggle', {
      method: 'POST', body: JSON.stringify(request),
    })
    if (response.type === 'track' && response.track_id) {
      setTrackIds((current) => toggledSet(current, response.track_id as number, response.favorited))
    } else if (response.type === 'artist' && response.artist) {
      setArtistNames((current) => toggledSet(current, response.artist as string, response.favorited))
    } else if (response.type === 'album' && response.album) {
      setAlbumKeys((current) => toggledSet(current, albumKey(response.artist ?? '', response.album as string), response.favorited))
    }
    setData((current) => ({
      ...current,
      count: response.count ?? current.count,
      counts: response.counts ?? current.counts,
    }))
    showToast(toggleMessage(response.type, response.favorited))
    void reload(true).catch(() => {})
    if (response.type === 'track' && response.favorited && options.withLike !== false && response.track_id) {
      Promise.resolve(onTrackLiked?.(response.track_id)).catch(() => {})
    }
    return response.favorited
  }

  return (
    <FavoritesContext.Provider value={{ ...data, trackIds, artistNames, albumKeys, loading, error, reload, toggleFavorite }}>
      {children}
    </FavoritesContext.Provider>
  )
}

function toggledSet<T>(current: ReadonlySet<T>, value: T, included: boolean): ReadonlySet<T> {
  const next = new Set(current)
  if (included) next.add(value)
  else next.delete(value)
  return next
}
