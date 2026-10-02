import type { CatalogAlbum, CatalogArtist } from '../../data/catalog.ts'

export type LibraryTab = 'tracks' | 'artists' | 'albums' | 'favorites'
export const LIBRARY_TAB_KEY = 'music_hive_library_tab'

export interface LibraryTrack {
  id: number
  artist: string
  title: string
  album: string
  duration: number
  artwork?: string
  ready?: boolean
}

export function filterTracks(tracks: readonly LibraryTrack[], query: string): LibraryTrack[] {
  const needle = query.trim().toLocaleLowerCase('ru')
  return tracks.filter((track) => !needle || `${track.artist} ${track.title} ${track.album}`.toLocaleLowerCase('ru').includes(needle))
}

export function filterArtists(artists: readonly CatalogArtist[], query: string): CatalogArtist[] {
  const needle = query.trim().toLocaleLowerCase('ru')
  return artists.filter(({ artist }) => !needle || artist.toLocaleLowerCase('ru').includes(needle))
}

export function filterAlbums(albums: readonly CatalogAlbum[], query: string): CatalogAlbum[] {
  const needle = query.trim().toLocaleLowerCase('ru')
  return albums.filter(({ artist, album }) => !needle || `${artist} ${album}`.toLocaleLowerCase('ru').includes(needle))
}

export function libraryPlaceholder(tab: LibraryTab): string {
  return {
    tracks: 'Артист, трек, альбом…', artists: 'Поиск артиста…',
    albums: 'Поиск альбома…', favorites: 'Поиск в избранном…',
  }[tab]
}

export function isLibraryTab(value: string | null): value is LibraryTab {
  return value === 'tracks' || value === 'artists' || value === 'albums' || value === 'favorites'
}
