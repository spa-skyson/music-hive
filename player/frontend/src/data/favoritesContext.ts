import { createContext, useContext } from 'react'
import type { Track } from '../player/state.ts'

export type FavoriteType = 'track' | 'artist' | 'album'

export type FavoriteRequest =
  | { type?: 'track'; track_id: number }
  | { type: 'artist'; artist?: string; track_id?: number }
  | { type: 'album'; artist?: string; album?: string; track_id?: number }

export interface FavoriteArtist {
  artist: string
  tracks: number
  artwork?: string
}

export interface FavoriteAlbum extends FavoriteArtist {
  album: string
}

export interface FavoriteTrack {
  track_id: number
  artist?: string
  title?: string
  duration?: number
  track?: Track
}

export interface FavoriteCounts {
  tracks: number
  artists: number
  albums: number
}

export interface FavoritesData {
  tracks: FavoriteTrack[]
  artists: FavoriteArtist[]
  albums: FavoriteAlbum[]
  ids: number[]
  count: number
  counts: FavoriteCounts
}

export interface FavoritesContextValue extends FavoritesData {
  trackIds: ReadonlySet<number>
  artistNames: ReadonlySet<string>
  albumKeys: ReadonlySet<string>
  loading: boolean
  error: Error | null
  reload: () => Promise<void>
  toggleFavorite: (request: FavoriteRequest, options?: { withLike?: boolean }) => Promise<boolean>
}

export const FavoritesContext = createContext<FavoritesContextValue | null>(null)

export function useFavorites(): FavoritesContextValue {
  const context = useContext(FavoritesContext)
  if (!context) throw new Error('useFavorites must be used within FavoritesProvider')
  return context
}
