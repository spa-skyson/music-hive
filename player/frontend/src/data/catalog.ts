export interface CatalogTrack {
  id: number
  artist?: string
  album?: string
  artwork?: string
}

export interface CatalogArtist {
  artist: string
  tracks: number
  cover: string | null
  sampleId: number
}

export interface CatalogAlbum extends CatalogArtist {
  album: string
}

export function albumKey(artist: string, album: string): string {
  return `${artist}\0${album}`
}

export function groupCatalog(tracks: readonly CatalogTrack[]): { artists: CatalogArtist[]; albums: CatalogAlbum[] } {
  const artists = new Map<string, CatalogArtist>()
  const albums = new Map<string, CatalogAlbum>()

  for (const track of tracks) {
    const artist = track.artist?.trim() || 'Unknown'
    const artistGroup = artists.get(artist)
    if (artistGroup) {
      artistGroup.tracks += 1
      if (!artistGroup.cover && track.artwork) artistGroup.cover = track.artwork
    } else {
      artists.set(artist, { artist, tracks: 1, cover: track.artwork || null, sampleId: track.id })
    }

    const album = track.album?.trim()
    if (!album) continue
    const key = albumKey(artist, album)
    const albumGroup = albums.get(key)
    if (albumGroup) {
      albumGroup.tracks += 1
      if (!albumGroup.cover && track.artwork) albumGroup.cover = track.artwork
    } else {
      albums.set(key, { artist, album, tracks: 1, cover: track.artwork || null, sampleId: track.id })
    }
  }

  return {
    artists: [...artists.values()].sort((left, right) => right.tracks - left.tracks || left.artist.localeCompare(right.artist, 'ru')),
    albums: [...albums.values()].sort((left, right) => right.tracks - left.tracks || left.album.localeCompare(right.album, 'ru')),
  }
}
