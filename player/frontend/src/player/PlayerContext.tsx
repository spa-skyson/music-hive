import { useEffect, useImperativeHandle, useRef, useState, type ReactNode, type RefObject } from 'react'
import { apiFetch, UNAUTHORIZED_EVENT } from '../api/client.ts'
import { PlayerContext, PlayerProgressContext, type PlayerProgressContextValue, type PlayerProgressStore, type PlayRequest } from './context.ts'
import {
  accumulateListened,
  initialPlaybackState,
  reducePlaybackPayload,
  shouldPostProgress,
  type PlayPayload,
  type PlaybackState,
  type Track,
} from './state.ts'

export const SESSION_KEY = 'music_hive_session'
const VOLUME_KEY = 'mh_volume'
const MUTED_KEY = 'mh_muted'

interface EventExtra {
  track_id?: number
  reason?: string
  position_sec?: number
  duration_sec?: number
  listened_sec?: number
}

interface ProgressApi {
  beginSeek: () => void
  previewSeek: (value: number) => void
  commitSeek: (value: number) => void
}

interface PlaybackRefs {
  autoplayTrackId: RefObject<number | null>
  lastPosition: RefObject<number>
  lastProgressAt: RefObject<number>
  listenedAccum: RefObject<number>
  state: RefObject<PlaybackState>
}

function storedVolume(): number {
  const value = Number(localStorage.getItem(VOLUME_KEY))
  return Number.isFinite(value) && value >= 0 && value <= 100 ? value : 100
}

function createProgressStore(): PlayerProgressStore {
  let snapshot: PlayerProgressContextValue = { position: 0, duration: 0, seeking: false, seekValue: 0 }
  const listeners = new Set<() => void>()
  return {
    getSnapshot: () => snapshot,
    set: (progress) => {
      snapshot = { ...snapshot, ...progress }
      listeners.forEach((listener) => listener())
    },
    subscribe: (listener) => {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
  }
}

export function PlayerProvider({ children }: { children: ReactNode }) {
  const audioRef = useRef<HTMLAudioElement>(null)
  const playbackRequestId = useRef(0)
  const stateRef = useRef<PlaybackState>(initialPlaybackState)
  const listenedAccum = useRef(0)
  const lastPosition = useRef(0)
  const lastProgressAt = useRef(0)
  const autoplayTrackId = useRef<number | null>(null)
  const progressApiRef = useRef<ProgressApi>(null)
  const progressStore = useRef(createProgressStore()).current
  const playbackRefs = useRef<PlaybackRefs>({ autoplayTrackId, lastPosition, lastProgressAt, listenedAccum, state: stateRef })
  const [state, setStateValue] = useState(initialPlaybackState)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [playing, setPlaying] = useState(false)
  const [volume, setVolumeValue] = useState(storedVolume)
  const [muted, setMuted] = useState(() => localStorage.getItem(MUTED_KEY) === 'true')

  function applyPayload(payload: PlayPayload, autoplay = true, resetSource = false) {
    if (payload.session_id) sessionStorage.setItem(SESSION_KEY, payload.session_id)
    const incomingTrack = payload.next ?? payload.current
    if (autoplay && incomingTrack) {
      autoplayTrackId.current = incomingTrack.id
      if (resetSource && audioRef.current) audioRef.current.dataset.trackId = ''
    }
    const next = reducePlaybackPayload(stateRef.current, payload)
    stateRef.current = next
    setStateValue(next)
  }

  function applyEventPayload(payload: PlayPayload) {
    if (payload.ended) audioRef.current?.pause()
    applyPayload({ ...payload, current: undefined }, Boolean(payload.next))
  }

  async function playbackMutation(request: () => Promise<PlayPayload>, apply: (payload: PlayPayload) => void) {
    const requestId = ++playbackRequestId.current
    setBusy(true)
    setError('')
    try {
      const payload = await request()
      if (requestId === playbackRequestId.current) apply(payload)
      return payload
    } catch (cause) {
      if (requestId === playbackRequestId.current) {
        setError(cause instanceof Error ? cause.message : 'Ошибка воспроизведения')
      }
      throw cause
    } finally {
      if (requestId === playbackRequestId.current) setBusy(false)
    }
  }

  async function postEvent(type: string, extra: EventExtra = {}, playback = false) {
    const audio = audioRef.current
    const currentState = stateRef.current
    if (!currentState.sessionId && type !== 'track_start') throw new Error('Сначала запусти микс или трек')
    const body = {
      type,
      track_id: currentState.current?.id,
      session_id: currentState.sessionId,
      position_sec: audio?.currentTime || 0,
      duration_sec: audio?.duration || currentState.current?.duration || 0,
      listened_sec: listenedAccum.current,
      ...extra,
    }
    const request = () => apiFetch<PlayPayload>('/api/events', { method: 'POST', body: JSON.stringify(body) })
    if (playback) return playbackMutation(request, applyEventPayload)
    const payload = await request()
    if (type !== 'progress') applyEventPayload(payload)
    return payload
  }

  async function play(request: PlayRequest) {
    return playbackMutation(
      () => apiFetch<PlayPayload>('/api/play', { method: 'POST', body: JSON.stringify(request) }),
      (payload) => applyPayload(payload, true, true),
    )
  }

  async function playMix(kind: string, trackId?: number) {
    return playbackMutation(
      () => apiFetch<PlayPayload>(`/api/mixes/${encodeURIComponent(kind)}/play`, {
        method: 'POST',
        body: JSON.stringify(trackId === undefined ? {} : { track_id: trackId }),
      }),
      (payload) => applyPayload(payload, true, true),
    )
  }

  async function startRadio(seedTrackId?: number) {
    return playbackMutation(
      () => apiFetch<PlayPayload>('/api/radio/start', {
        method: 'POST',
        body: JSON.stringify(seedTrackId === undefined ? {} : { seed_track_id: seedTrackId }),
      }),
      (payload) => applyPayload({ ...payload, fixed: false }),
    )
  }

  async function togglePlay() {
    const audio = audioRef.current
    if (!audio?.src) throw new Error('Сначала выбери микс или трек')
    if (audio.paused) await audio.play()
    else audio.pause()
  }

  async function skip() {
    const audio = audioRef.current
    await postEvent('skip', {
      reason: 'skipped',
      listened_sec: listenedAccum.current,
      duration_sec: audio?.duration || stateRef.current.current?.duration || 0,
    }, true)
  }

  async function rate(type: 'like' | 'dislike') {
    const payload = await rateDetails(type)
    return payload.rating ?? type
  }

  async function rateDetails(type: 'like' | 'dislike') {
    return postEvent(type)
  }

  async function selectTrack(track: Track, index: number) {
    if (track.id === stateRef.current.current?.id) return togglePlay()
    if (!stateRef.current.sessionId) throw new Error('Нет активной сессии')
    await playbackMutation(
      () => apiFetch<PlayPayload>('/api/session/jump', {
        method: 'POST',
        body: JSON.stringify({ session_id: stateRef.current.sessionId, track_id: track.id, index }),
      }),
      (payload) => applyPayload(payload),
    )
  }

  async function shuffle() {
    if (!stateRef.current.sessionId) throw new Error('Нет активной сессии')
    return playbackMutation(
      () => apiFetch<PlayPayload>('/api/session/shuffle', {
        method: 'POST',
        body: JSON.stringify({ session_id: stateRef.current.sessionId }),
      }),
      (payload) => applyPayload(payload, false),
    )
  }

  function setVolume(value: number) {
    const next = Math.max(0, Math.min(100, value))
    if (audioRef.current) audioRef.current.volume = next / 100
    localStorage.setItem(VOLUME_KEY, String(next))
    setVolumeValue(next)
  }

  function toggleMute() {
    const next = !muted
    if (audioRef.current) audioRef.current.muted = next
    localStorage.setItem(MUTED_KEY, String(next))
    setMuted(next)
  }

  useEffect(() => {
    const audio = audioRef.current
    if (audio) {
      audio.volume = storedVolume() / 100
      audio.muted = localStorage.getItem(MUTED_KEY) === 'true'
    }
  }, [])

  useEffect(() => {
    const pause = () => audioRef.current?.pause()
    globalThis.addEventListener(UNAUTHORIZED_EVENT, pause)
    return () => globalThis.removeEventListener(UNAUTHORIZED_EVENT, pause)
  }, [])

  useEffect(() => {
    const sessionId = sessionStorage.getItem(SESSION_KEY)
    if (!sessionId) return
    let cancelled = false
    apiFetch<PlayPayload>(`/api/now?session_id=${encodeURIComponent(sessionId)}`)
      .then((payload) => {
        if (!cancelled && payload.current) applyPayload(payload, false)
      })
      .catch(() => {
        if (!cancelled) sessionStorage.removeItem(SESSION_KEY)
      })
    return () => { cancelled = true }
  }, [])

  return (
    <PlayerContext.Provider value={{
      ...state,
      audioRef,
      busy,
      error,
      playing,
      volume,
      muted,
      play,
      playMix,
      startRadio,
      rate,
      rateDetails,
      togglePlay,
      skip,
      selectTrack,
      shuffle,
      beginSeek: () => progressApiRef.current?.beginSeek(),
      previewSeek: (value) => progressApiRef.current?.previewSeek(value),
      commitSeek: (value) => progressApiRef.current?.commitSeek(value),
      setVolume,
      toggleMute,
    }}>
      <PlayerProgressProvider
        audioRef={audioRef}
        current={state.current}
        muted={muted}
        onError={setError}
        onPlayingChange={setPlaying}
        postEvent={postEvent}
        playbackRefs={playbackRefs.current}
        progressApiRef={progressApiRef}
        progressStore={progressStore}
      >
        {children}
      </PlayerProgressProvider>
    </PlayerContext.Provider>
  )
}

function PlayerProgressProvider({ audioRef, children, current, muted, onError, onPlayingChange, postEvent, playbackRefs, progressApiRef, progressStore }: {
  audioRef: RefObject<HTMLAudioElement | null>
  children: ReactNode
  current: Track | null
  muted: boolean
  onError: (message: string) => void
  onPlayingChange: (playing: boolean) => void
  postEvent: (type: string, extra?: EventExtra, playback?: boolean) => Promise<PlayPayload>
  playbackRefs: PlaybackRefs
  progressApiRef: RefObject<ProgressApi | null>
  progressStore: PlayerProgressStore
}) {
  function previewSeek(value: number) {
    progressStore.set({ seeking: true, seekValue: value, position: (value / 1_000) * progressStore.getSnapshot().duration })
  }

  function commitSeek(value: number) {
    const audio = audioRef.current
    if (audio?.duration) {
      audio.currentTime = (value / 1_000) * audio.duration
      // oxlint-disable-next-line react/immutability -- shared playback accumulator intentionally lives outside render state
      playbackRefs.lastPosition.current = audio.currentTime
      progressStore.set({ position: audio.currentTime, seekValue: value })
    }
    progressStore.set({ seeking: false })
  }

  useImperativeHandle(progressApiRef, () => ({ beginSeek: () => progressStore.set({ seeking: true }), previewSeek, commitSeek }))

  useEffect(() => {
    const audio = audioRef.current
    if (!audio || !current) return
    const source = current.stream ?? `/api/stream/${current.id}`
    if (audio.dataset.trackId !== String(current.id)) {
      audio.dataset.trackId = String(current.id)
      audio.src = source
      // oxlint-disable-next-line react/immutability -- reset shared playback accumulators when the audio source changes
      playbackRefs.listenedAccum.current = 0
      // oxlint-disable-next-line react/immutability -- reset shared playback accumulators when the audio source changes
      playbackRefs.lastPosition.current = 0
      progressStore.set({ position: 0, duration: current.duration ?? 0, seekValue: 0 })
      if (playbackRefs.autoplayTrackId.current === current.id) {
        playbackRefs.autoplayTrackId.current = null
        audio.play().catch(() => onError('Не удалось начать воспроизведение'))
        postEvent('track_start', { track_id: current.id }).catch(() => {})
      }
    }
    // The effect must run only when the track identity changes; mutable refs and callbacks deliberately use their latest values.
    // oxlint-disable-next-line react-hooks/exhaustive-deps
  }, [current?.id])

  return (
    <PlayerProgressContext value={progressStore}>
      {children}
      <audio
        ref={audioRef}
        muted={muted}
        onDurationChange={(event) => progressStore.set({ duration: event.currentTarget.duration || 0 })}
        onEnded={() => {
          postEvent('track_end', {
            reason: 'completed',
            listened_sec: playbackRefs.listenedAccum.current,
            duration_sec: audioRef.current?.duration || playbackRefs.state.current.current?.duration || 0,
          }).catch((cause) => console.error(cause))
        }}
        onLoadedMetadata={(event) => progressStore.set({ duration: event.currentTarget.duration || 0 })}
        onPause={() => onPlayingChange(false)}
        onPlay={() => onPlayingChange(true)}
        onTimeUpdate={(event) => {
          const audio = event.currentTarget
          const nextPosition = audio.currentTime || 0
          // oxlint-disable-next-line react/immutability -- timeupdate advances non-rendering playback accounting refs
          playbackRefs.listenedAccum.current = accumulateListened(playbackRefs.listenedAccum.current, playbackRefs.lastPosition.current, nextPosition)
          // oxlint-disable-next-line react/immutability -- timeupdate advances non-rendering playback accounting refs
          playbackRefs.lastPosition.current = nextPosition
          if (!progressStore.getSnapshot().seeking && audio.duration) {
            progressStore.set({
              position: nextPosition,
              duration: audio.duration,
              seekValue: Math.round((nextPosition / audio.duration) * 1_000),
            })
          }
          const now = Date.now()
          if (shouldPostProgress(playbackRefs.lastProgressAt.current, now, Boolean(playbackRefs.state.current.current))) {
            playbackRefs.lastProgressAt.current = now
            postEvent('progress', {
              position_sec: nextPosition,
              duration_sec: audio.duration || playbackRefs.state.current.current?.duration || 0,
              listened_sec: playbackRefs.listenedAccum.current,
            }).catch(() => {})
          }
        }}
        preload="metadata"
      />
    </PlayerProgressContext>
  )
}
