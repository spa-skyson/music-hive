import { type ComponentType, type FormEvent, type ReactNode, useEffect, useRef, useState } from 'react'
import { AuthProvider } from './auth/AuthContext.tsx'
import { useAuth } from './auth/context.ts'
import { FavoritesProvider, JobsProvider, ToastProvider } from './data/index.ts'
import { apiFetch } from './api/client.ts'
import { PlayerProvider } from './player/PlayerContext.tsx'
import { usePlayer } from './player/context.ts'
import { MiniPlayer } from './player/MiniPlayer.tsx'
import { usePlayerHotkeys } from './player/usePlayerHotkeys.ts'
import { routeHash, type Route, type View } from './routing.ts'
import { useHashRoute } from './useHashRoute.ts'
import { HomeView } from './views/HomeView.tsx'
import { LibraryView } from './views/LibraryView.tsx'
import { PlayerView } from './views/PlayerView.tsx'
import { ProfileView } from './views/ProfileView.tsx'
import { UploadView } from './views/UploadView.tsx'
import { UsersView } from './views/UsersView.tsx'

const views: ReadonlyArray<{ view: View; label: string }> = [
  { view: 'home', label: 'Главная' },
  { view: 'player', label: 'Сейчас' },
  { view: 'library', label: 'Библиотека' },
  { view: 'profile', label: 'Профиль' },
  { view: 'upload', label: 'Загрузка' },
  { view: 'users', label: 'Пользователи' },
]

const viewComponents: Record<View, ComponentType> = {
  home: HomeView,
  player: PlayerView,
  library: LibraryView,
  profile: ProfileView,
  upload: UploadView,
  users: UsersView,
}

function LoginGate() {
  const { login } = useAuth()
  const [error, setError] = useState('')
  const [pending, setPending] = useState(false)
  const passwordRef = useRef<HTMLInputElement>(null)

  useEffect(() => passwordRef.current?.focus(), [])

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const data = new FormData(event.currentTarget)
    setPending(true)
    setError('')
    try {
      await login(String(data.get('username') ?? ''), String(data.get('password') ?? ''))
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Не удалось войти')
    } finally {
      setPending(false)
    }
  }

  return (
    <div className="fixed inset-0 z-20 grid place-items-center bg-bg/95 p-4">
      <form className="w-full max-w-sm rounded-app border border-line bg-surface-strong p-6 shadow-app" onSubmit={submit}>
        <p className="mb-1 text-sm text-muted">music-hive</p>
        <h1 className="font-display text-2xl font-bold">Вход</h1>
        <label className="mt-6 block text-sm" htmlFor="username">Имя пользователя</label>
        <input className="mt-2 min-h-11 w-full rounded-lg border border-line bg-bg2 px-3 outline-none focus-visible:ring-2 focus-visible:ring-accent" id="username" name="username" autoComplete="username" required />
        <label className="mt-4 block text-sm" htmlFor="password">Пароль</label>
        <input ref={passwordRef} className="mt-2 min-h-11 w-full rounded-lg border border-line bg-bg2 px-3 outline-none focus-visible:ring-2 focus-visible:ring-accent" id="password" name="password" type="password" autoComplete="current-password" required />
        {error && <p className="mt-4 text-sm text-red-300" role="alert">{error}</p>}
        <button className="mt-6 min-h-11 w-full rounded-lg bg-accent px-4 font-bold text-bg outline-none hover:brightness-110 focus-visible:ring-2 focus-visible:ring-fg" disabled={pending} type="submit">
          {pending ? 'Входим…' : 'Войти'}
        </button>
      </form>
    </div>
  )
}

function Shell() {
  const { authEnabled, isAdmin, ready, user, logout } = useAuth()
  const { route } = useHashRoute()
  const headingRef = useRef<HTMLHeadingElement>(null)
  const activeRoute: Route = route.view === 'users' && !isAdmin ? { view: 'home' } : route
  const activeView = views.find(({ view }) => view === activeRoute.view) ?? views[0]
  const ActiveView = viewComponents[activeRoute.view]
  usePlayerHotkeys()

  useEffect(() => {
    if (ready && route.view === 'users' && !isAdmin) location.hash = routeHash({ view: 'home' })
  }, [isAdmin, ready, route.view])

  useEffect(() => {
    document.title = `${activeView.label} — music-hive`
    headingRef.current?.focus({ preventScroll: true })
    window.scrollTo({ top: 0, behavior: 'smooth' })
  }, [activeView.label])

  if (!ready) return <div className="grid min-h-screen place-items-center text-muted">Загрузка…</div>

  const authenticated = !authEnabled || user !== null

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-10 border-b border-line-soft bg-surface-strong backdrop-blur">
        <div className="mx-auto flex max-w-6xl flex-wrap items-center gap-4 px-4 py-3">
          <a className="font-display text-xl font-bold text-accent outline-none focus-visible:ring-2 focus-visible:ring-accent" href={routeHash({ view: 'home' })}>music-hive</a>
          <nav aria-label="Основная навигация" className="flex flex-1 flex-wrap gap-1">
            {views.filter(({ view }) => view !== 'users' || isAdmin).map(({ view, label }) => (
              <a
                aria-current={activeRoute.view === view ? 'page' : undefined}
                className="min-h-9 rounded-lg px-3 py-2 text-sm text-muted outline-none hover:bg-line-soft hover:text-fg focus-visible:ring-2 focus-visible:ring-accent aria-[current=page]:bg-line-soft aria-[current=page]:text-fg"
                href={routeHash({ view })}
                key={view}
              >{label}</a>
            ))}
          </nav>
          {authEnabled && authenticated && (
            <button className="min-h-9 rounded-lg border border-line px-3 text-sm outline-none hover:border-accent focus-visible:ring-2 focus-visible:ring-accent" onClick={() => void logout()} type="button">Выйти</button>
          )}
        </div>
      </header>
      <main className="mx-auto max-w-6xl p-4 py-10">
        <section className="rounded-app border border-line-soft bg-surface p-6 shadow-app">
          <h1 ref={headingRef} className="font-display text-3xl font-bold outline-none" tabIndex={-1}>{activeView.label}</h1>
          <ActiveView />
        </section>
      </main>
      <MiniPlayer activeView={activeRoute.view} />
      {!authenticated && <LoginGate />}
    </div>
  )
}

function App() {
  return <AuthProvider><PlayerProvider><ToastProvider><PlayerDataProviders><GlobalDataProviders><Shell /></GlobalDataProviders></PlayerDataProviders></ToastProvider></PlayerProvider></AuthProvider>
}

function GlobalDataProviders({ children }: { children: ReactNode }) {
  const { authEnabled, ready, user } = useAuth()
  return <JobsProvider enabled={ready && (!authEnabled || user !== null)}>{children}</JobsProvider>
}

function PlayerDataProviders({ children }: { children: ReactNode }) {
  const player = usePlayer()
  const { authEnabled, ready, user } = useAuth()

  async function postLike(trackId: number) {
    if (!player.sessionId || player.current?.id !== trackId) return
    const audio = player.audioRef.current
    await apiFetch('/api/events', {
      method: 'POST',
      body: JSON.stringify({
        type: 'like', track_id: trackId, session_id: player.sessionId,
        position_sec: audio?.currentTime || 0,
        duration_sec: audio?.duration || player.current.duration || 0,
      }),
    })
  }

  return <FavoritesProvider enabled={ready && (!authEnabled || user !== null)} onTrackLiked={postLike}>{children}</FavoritesProvider>
}

export default App
