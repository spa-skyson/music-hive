import { useState, type FormEvent } from 'react'
import { apiFetch } from '../api/client.ts'
import { useApiData, useToast } from '../data/index.ts'
import { Button, EmptyState, ErrorState, Field, Section, SkeletonCards } from '../ui/index.ts'

interface Profile {
  maturity: string
  confidence: number
  n_positive: number
  ready_at: number
  explore_ratio: number
  top_artists?: Array<{ artist: string; count: number }>
}

interface WeeklyMetrics {
  listens_7d: number
  skips_7d: number
  skip_rate_7d: number
  unique_artists_7d: number
}

interface RadioShare {
  token: string
  url: string
  active: boolean
  listen_count: number
}

interface TokenInfo {
  id: number
  name: string
  prefix: string
  created_at: string
}

interface TokenResponse {
  tokens?: TokenInfo[]
}

export function ProfileView() {
  const summary = useApiData('profile-summary', async (signal) => {
    const [profile, metrics] = await Promise.all([
      apiFetch<Profile>('/api/profile', { signal }),
      apiFetch<WeeklyMetrics>('/api/metrics/weekly', { signal }),
    ])
    return { profile, metrics }
  })
  const shares = useApiData('profile-shares', (signal) => apiFetch<{ shares?: RadioShare[] }>('/api/share/radio', { signal }))
  const tokens = useApiData('profile-tokens', (signal) => apiFetch<TokenResponse>('/api/auth/tokens', { signal }))
  const { showToast } = useToast()
  const [sharePending, setSharePending] = useState(false)
  const [shareUrl, setShareUrl] = useState('')
  const [revokingShare, setRevokingShare] = useState<string | null>(null)
  const [tokenPending, setTokenPending] = useState(false)
  const [tokenSecret, setTokenSecret] = useState('')
  const [revokingToken, setRevokingToken] = useState<number | null>(null)
  const [passwordPending, setPasswordPending] = useState(false)
  const [passwordError, setPasswordError] = useState('')

  async function copy(text: string, success: string) {
    try {
      await navigator.clipboard.writeText(text)
      showToast(success)
    } catch {
      showToast('Не удалось скопировать', { variant: 'error' })
    }
  }

  async function createShare() {
    setSharePending(true)
    try {
      const created = await apiFetch<{ url?: string; url_bare?: string }>('/api/share/radio', {
        method: 'POST', body: JSON.stringify({ name: 'music-hive radio' }),
      })
      const url = created.url ?? created.url_bare ?? ''
      setShareUrl(url)
      try {
        await navigator.clipboard.writeText(url)
        showToast('Ссылка скопирована')
      } catch {
        showToast('Ссылка создана')
      }
      shares.retry()
    } catch (cause) {
      showToast(cause instanceof Error ? cause.message : 'Не удалось создать ссылку', { variant: 'error' })
    } finally {
      setSharePending(false)
    }
  }

  async function revokeShare(share: RadioShare) {
    setRevokingShare(share.token)
    try {
      await apiFetch<{ ok: boolean }>(`/api/share/radio/${encodeURIComponent(share.token)}`, { method: 'DELETE' })
      showToast('Ссылка отозвана')
      shares.retry()
    } catch (cause) {
      showToast(cause instanceof Error ? cause.message : 'Не удалось отозвать ссылку', { variant: 'error' })
    } finally {
      setRevokingShare(null)
    }
  }

  async function createToken() {
    setTokenPending(true)
    setTokenSecret('')
    try {
      const created = await apiFetch<{ token?: string }>('/api/auth/tokens', {
        method: 'POST', body: JSON.stringify({ name: `web ${new Date().toISOString().slice(0, 10)}` }),
      })
      setTokenSecret(created.token ?? '')
      showToast('Токен создан')
      tokens.retry()
    } catch (cause) {
      showToast(cause instanceof Error ? cause.message : 'Не удалось создать токен', { variant: 'error' })
    } finally {
      setTokenPending(false)
    }
  }

  async function revokeToken(id: number) {
    setRevokingToken(id)
    try {
      await apiFetch<{ ok: boolean }>(`/api/auth/tokens/${id}`, { method: 'DELETE' })
      showToast('Токен отозван')
      tokens.retry()
    } catch (cause) {
      showToast(cause instanceof Error ? cause.message : 'Не удалось отозвать токен', { variant: 'error' })
    } finally {
      setRevokingToken(null)
    }
  }

  async function saveSubsonicPassword(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = event.currentTarget
    const data = new FormData(form)
    const password = String(data.get('password') ?? '')
    const confirmation = String(data.get('confirmation') ?? '')
    setPasswordError('')
    if (password.length < 4) {
      setPasswordError('Пароль должен содержать не менее 4 символов')
      return
    }
    if (password !== confirmation) {
      setPasswordError('Пароли не совпадают')
      return
    }
    setPasswordPending(true)
    try {
      await apiFetch<{ ok: boolean }>('/api/auth/subsonic-password', {
        method: 'PUT', body: JSON.stringify({ password }),
      })
      form.reset()
      showToast('Subsonic-пароль обновлён')
    } catch (cause) {
      setPasswordError(cause instanceof Error ? cause.message : 'Не удалось обновить пароль')
    } finally {
      setPasswordPending(false)
    }
  }

  return (
    <div className="mt-2 grid gap-8">
      <p className="text-muted">Вкус растёт вместе со слушанием</p>

      <Section title="Профиль слушателя">
        {summary.status === 'loading' && !summary.data && <SkeletonCards count={2} label="Загрузка профиля" />}
        {summary.status === 'error' && <ErrorState description={summary.error.message} onRetry={summary.retry} title="Не удалось загрузить профиль" />}
        {summary.data && (
          <div className="grid gap-4 md:grid-cols-2" aria-live="polite">
            <article className="rounded-app border border-line bg-bg2 p-5">
              <p><strong className="text-lg">{summary.data.profile.maturity}</strong> · уверенность {Math.round((summary.data.profile.confidence || 0) * 100)}%</p>
              <p className="mt-2 text-sm text-muted">{summary.data.profile.n_positive}/{summary.data.profile.ready_at} позитивных сигналов · explore {Number(summary.data.profile.explore_ratio).toFixed(2)}</p>
              <h3 className="mt-5 font-semibold">Топ артисты</h3>
              {(summary.data.profile.top_artists?.length ?? 0) > 0 ? (
                <ul className="mt-2 grid gap-1 text-sm text-muted">
                  {summary.data.profile.top_artists?.map((artist) => <li key={artist.artist}>{artist.artist} · {artist.count}</li>)}
                </ul>
              ) : <p className="mt-2 text-muted">—</p>}
            </article>
            <article className="rounded-app border border-line bg-bg2 p-5">
              <h3 className="font-semibold">Неделя</h3>
              <p className="mt-2 text-sm text-muted">
                {summary.data.metrics.listens_7d} прослушиваний · {summary.data.metrics.skips_7d} скипов ({Math.round(summary.data.metrics.skip_rate_7d * 100)}%) · {summary.data.metrics.unique_artists_7d} артистов
              </p>
            </article>
          </div>
        )}
      </Section>

      <Section
        action={<Button pending={sharePending} onClick={() => void createShare()} variant="primary">Создать ссылку</Button>}
        hint="Непрерывный MP3-поток для VLC и любых плееров. Слушатели не меняют твой вкус."
        title="Публичное радио"
      >
        {shareUrl && <p className="break-all rounded-xl border border-line p-3 text-sm" role="status" aria-live="polite"><a className="text-accent underline" href={shareUrl} rel="noopener noreferrer" target="_blank">{shareUrl}</a></p>}
        {shares.status === 'loading' && !shares.data && <SkeletonCards count={1} label="Загрузка публичных ссылок" />}
        {shares.status === 'error' && <ErrorState description={shares.error.message} onRetry={shares.retry} title="Не удалось загрузить ссылки" />}
        {shares.data && (shares.data.shares?.length ?? 0) === 0 && <EmptyState title="Пока нет ссылок" description="Нажмите «Создать ссылку»." />}
        {shares.data && (shares.data.shares?.length ?? 0) > 0 && (
          <ul className="grid gap-3" aria-live="polite">
            {shares.data.shares?.map((share) => (
              <li className={`flex flex-wrap items-center justify-between gap-3 rounded-app border border-line p-4 ${share.active ? 'bg-bg2' : 'opacity-60'}`} key={share.token}>
                <div className="min-w-0 flex-1">
                  <a className="block truncate text-accent underline" href={share.url} rel="noopener noreferrer" target="_blank">{share.url}</a>
                  <span className="text-sm text-muted">{share.active ? `слушали ${share.listen_count || 0}` : 'отозвана'}</span>
                </div>
                {share.active && <div className="flex flex-wrap gap-2"><Button pending={revokingShare === share.token} onClick={() => void revokeShare(share)} variant="quiet">Отозвать</Button><Button onClick={() => void copy(share.url, 'Скопировано')} variant="quiet">Копировать</Button></div>}
              </li>
            ))}
          </ul>
        )}
      </Section>

      <Section
        action={<Button pending={tokenPending} onClick={() => void createToken()} variant="primary">Создать токен</Button>}
        hint="Bearer-токены для скриптов и интеграций. Секрет показывается один раз."
        title="API-токены"
      >
        {tokenSecret && (
          <div className="grid gap-3 rounded-app border border-accent/50 bg-accent/10 p-4" role="status" aria-live="polite">
            <code className="break-all text-sm">Новый токен (показан один раз): {tokenSecret}</code>
            <div className="flex flex-wrap gap-2"><Button onClick={() => void copy(tokenSecret, 'Токен скопирован')} variant="quiet">Копировать</Button><Button onClick={() => setTokenSecret('')} variant="quiet">Скрыть</Button></div>
          </div>
        )}
        {tokens.status === 'loading' && !tokens.data && <SkeletonCards count={1} label="Загрузка API-токенов" />}
        {tokens.status === 'error' && <ErrorState description={tokens.error.message} onRetry={tokens.retry} title="Не удалось загрузить токены" />}
        {tokens.data && (tokens.data.tokens?.length ?? 0) === 0 && <EmptyState title="Пока нет токенов" />}
        {tokens.data && (tokens.data.tokens?.length ?? 0) > 0 && (
          <ul className="grid gap-3" aria-live="polite">
            {tokens.data.tokens?.map((token) => (
              <li className="flex flex-wrap items-center justify-between gap-3 rounded-app border border-line bg-bg2 p-4" key={token.id}>
                <div><strong>{token.name || 'без имени'}</strong><p className="text-sm text-muted">{token.prefix} · с {new Date(token.created_at).toLocaleDateString()}</p></div>
                <Button pending={revokingToken === token.id} onClick={() => void revokeToken(token.id)} variant="quiet">Отозвать</Button>
              </li>
            ))}
          </ul>
        )}
      </Section>

      <Section hint={`Отдельный пароль для Subsonic-клиентов. URL сервера: ${globalThis.location.origin}`} title="Subsonic">
        <form className="grid max-w-xl gap-4 rounded-app border border-line bg-bg2 p-4" onSubmit={saveSubsonicPassword}>
          <Field autoComplete="new-password" label="Новый Subsonic-пароль" minLength={4} name="password" required type="password" />
          <Field autoComplete="new-password" label="Подтверждение пароля" minLength={4} name="confirmation" required type="password" />
          {passwordError && <p className="text-sm text-red-300" role="alert">{passwordError}</p>}
          <Button className="justify-self-start" pending={passwordPending} type="submit" variant="primary">Сохранить</Button>
        </form>
      </Section>
    </div>
  )
}
