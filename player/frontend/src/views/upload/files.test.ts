import assert from 'node:assert/strict'
import test from 'node:test'
import { audioExt, fileRelPath, withRelPath } from './files.ts'

test('audioExt normalizes supported-looking extensions', () => {
  assert.equal(audioExt('Track.FLAC'), '.flac')
  assert.equal(audioExt('archive'), '')
  assert.equal(audioExt('.hidden'), '.hidden')
})

test('fileRelPath prefers a normalized explicit relative path', () => {
  const file = new File(['audio'], 'track.mp3')
  assert.equal(fileRelPath(withRelPath(file, 'Artist\\Album\\track.mp3')), 'Artist/Album/track.mp3')
})

test('fileRelPath falls back to the filename', () => {
  assert.equal(fileRelPath(new File([], 'track.ogg')), 'track.ogg')
})
