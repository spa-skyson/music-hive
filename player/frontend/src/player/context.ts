import { createContext, use, useSyncExternalStore, type RefObject } from 'react'
import type { PlayPayload, PlaybackState, Track } from './state.ts'

export interface PlayRequest {
  track_id?: number
  track_ids?: number[]
  artist?: string
  album?: string
  start_index?: number
  start_track_id?: number
  name?: string
  shuffle?: boolean
}

export interface PlayerContextValue extends PlaybackState {
  audioRef: RefObject<HTMLAudioElement | null>
  busy: boolean
  error: string
  playing: boolean
  volume: number
  muted: boolean
  play: (request: PlayRequest) => Promise<PlayPayload>
  playMix: (kind: string, trackId?: number) => Promise<PlayPayload>
  startRadio: (seedTrackId?: number) => Promise<PlayPayload>
  rate: (type: 'like' | 'dislike') => Promise<'like' | 'dislike'>
  rateDetails: (type: 'like' | 'dislike') => Promise<Pick<PlayPayload, 'ignored' | 'rating'>>
  togglePlay: () => Promise<void>
  skip: () => Promise<void>
  selectTrack: (track: Track, index: number) => Promise<void>
  shuffle: () => Promise<PlayPayload>
  beginSeek: () => void
  previewSeek: (value: number) => void
  commitSeek: (value: number) => void
  setVolume: (value: number) => void
  toggleMute: () => void
}

export interface PlayerProgressContextValue {
  position: number
  duration: number
  seeking: boolean
  seekValue: number
}

export interface PlayerProgressStore {
  getSnapshot: () => PlayerProgressContextValue
  set: (progress: Partial<PlayerProgressContextValue>) => void
  subscribe: (listener: () => void) => () => void
}

export const PlayerContext = createContext<PlayerContextValue | null>(null)
export const PlayerProgressContext = createContext<PlayerProgressStore | null>(null)

export function usePlayer(): PlayerContextValue {
  const context = use(PlayerContext)
  if (!context) throw new Error('usePlayer must be used within PlayerProvider')
  return context
}

export function usePlayerProgress(): PlayerProgressContextValue {
  const store = use(PlayerProgressContext)
  if (!store) throw new Error('usePlayerProgress must be used within PlayerProvider')
  return useSyncExternalStore(store.subscribe, store.getSnapshot, store.getSnapshot)
}
