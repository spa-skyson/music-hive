export interface Track {
  id: number
  track_id?: number
  title?: string
  artist?: string
  album?: string
  duration?: number
  stream?: string
  artwork?: string
  current?: boolean
}

export interface QueueItem extends Track {
  explanation?: string
  explore?: boolean
  new_boost?: boolean
}

export interface PlayPayload {
  session_id?: string
  maturity?: string
  fixed?: boolean
  mode?: string
  name?: string
  index?: number
  current?: Track
  next?: Track
  tracks?: Track[] | null
  queue?: Array<Omit<QueueItem, 'id'> & { id?: number }>
  ended?: boolean
  ignored?: boolean
  rating?: 'like' | 'dislike'
}

export interface PlaybackState {
  sessionId: string | null
  maturity: string | null
  fixed: boolean
  mode: string
  current: Track | null
  tracks: Track[]
  queue: QueueItem[]
  currentIndex: number
  ended: boolean
}

export const initialPlaybackState: PlaybackState = {
  sessionId: null,
  maturity: null,
  fixed: false,
  mode: '',
  current: null,
  tracks: [],
  queue: [],
  currentIndex: -1,
  ended: false,
}

export function formatTime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '0:00'
  const minutes = Math.floor(seconds / 60)
  return `${minutes}:${String(Math.floor(seconds % 60)).padStart(2, '0')}`
}

export function accumulateListened(accumulated: number, previous: number, current: number): number {
  return current > previous ? accumulated + current - previous : accumulated
}

export function shouldPostProgress(lastPostedAt: number, now: number, hasCurrent: boolean): boolean {
  return hasCurrent && now - lastPostedAt > 4_000
}

export function reducePlaybackPayload(state: PlaybackState, payload: PlayPayload): PlaybackState {
  const tracks = payload.tracks
  const hasTracks = Array.isArray(tracks)
  const queue = payload.queue?.map((item) => ({ ...item, id: item.id ?? item.track_id ?? 0 }))
  return {
    ...state,
    sessionId: payload.session_id ?? state.sessionId,
    maturity: payload.maturity ?? state.maturity,
    fixed: payload.fixed ?? (hasTracks ? tracks.length > 0 : state.fixed),
    mode: payload.name ?? payload.mode ?? state.mode,
    current: payload.next ?? payload.current ?? state.current,
    tracks: hasTracks ? tracks : (payload.fixed === false && queue ? [] : state.tracks),
    queue: hasTracks ? [] : (queue ?? state.queue),
    currentIndex: payload.index ?? state.currentIndex,
    ended: payload.ended ?? (payload.next || payload.current ? false : state.ended),
  }
}
