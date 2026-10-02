import assert from 'node:assert/strict'
import test from 'node:test'
import { filterAlbums, filterArtists, filterTracks, libraryPlaceholder } from './model.ts'

const tracks = [
  { id: 1, artist: 'Кино', title: 'Звезда', album: 'Чёрный альбом', duration: 1 },
  { id: 2, artist: 'Björk', title: 'Jóga', album: 'Homogenic', duration: 2 },
]

test('поиск треков нечувствителен к регистру и ищет по всем полям', () => {
  assert.deepEqual(filterTracks(tracks, 'ЧЁРНЫЙ').map(({ id }) => id), [1])
  assert.deepEqual(filterTracks(tracks, '  björk ').map(({ id }) => id), [2])
  assert.equal(filterTracks(tracks, '').length, 2)
})

test('поиск групп каталога и подсказки вкладок', () => {
  assert.equal(filterArtists([{ artist: 'Кино', tracks: 1, cover: null, sampleId: 1 }], 'кин').length, 1)
  assert.equal(filterAlbums([{ artist: 'Кино', album: 'Ночь', tracks: 1, cover: null, sampleId: 1 }], 'ноч').length, 1)
  assert.equal(libraryPlaceholder('favorites'), 'Поиск в избранном…')
})
