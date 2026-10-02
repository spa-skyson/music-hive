import assert from 'node:assert/strict'
import test from 'node:test'
import { groupCatalog } from './catalog.ts'

test('groupCatalog groups, sorts and fills missing covers', () => {
  const grouped = groupCatalog([
    { id: 1, artist: 'B', album: 'Second' },
    { id: 2, artist: ' A ', album: ' First ', artwork: '/a.jpg' },
    { id: 3, artist: 'B', album: 'Second', artwork: '/b.jpg' },
    { id: 4, artist: 'B', album: '' },
  ])

  assert.deepEqual(grouped.artists, [
    { artist: 'B', tracks: 3, cover: '/b.jpg', sampleId: 1 },
    { artist: 'A', tracks: 1, cover: '/a.jpg', sampleId: 2 },
  ])
  assert.deepEqual(grouped.albums, [
    { artist: 'B', album: 'Second', tracks: 2, cover: '/b.jpg', sampleId: 1 },
    { artist: 'A', album: 'First', tracks: 1, cover: '/a.jpg', sampleId: 2 },
  ])
})

test('groupCatalog uses Unknown and skips blank albums', () => {
  const grouped = groupCatalog([{ id: 1 }, { id: 2, artist: '  ', album: 'Album' }])
  assert.deepEqual(grouped.artists, [{ artist: 'Unknown', tracks: 2, cover: null, sampleId: 1 }])
  assert.equal(grouped.albums[0]?.artist, 'Unknown')
})
