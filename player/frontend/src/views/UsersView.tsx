import { useState, type FormEvent } from 'react'
import { apiFetch } from '../api/client.ts'
import { useApiData, useToast } from '../data/index.ts'
import { Button, EmptyState, ErrorState, Field, Section, SkeletonCards } from '../ui/index.ts'

interface AdminUser {
  id: number
  username: string
  is_admin: boolean
  is_owner: boolean
  disabled: boolean
  created_at: string
}

interface UsersResponse {
  users?: AdminUser[]
}

export function UsersView() {
  const users = useApiData('admin-users', (signal) => apiFetch<UsersResponse>('/api/admin/users', { signal }))
  const { showToast } = useToast()
  const [createError, setCreateError] = useState('')
  const [creating, setCreating] = useState(false)
  const [updatingId, setUpdatingId] = useState<number | null>(null)

  async function createUser(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = event.currentTarget
    const data = new FormData(form)
    setCreating(true)
    setCreateError('')
    try {
      await apiFetch<{ ok: boolean }>('/api/admin/users', {
        method: 'POST',
        body: JSON.stringify({
          username: String(data.get('username') ?? '').trim(),
          password: String(data.get('password') ?? ''),
          is_admin: data.get('is_admin') === 'on',
        }),
      })
      form.reset()
      showToast('Пользователь создан')
      users.retry()
    } catch (cause) {
      setCreateError(cause instanceof Error ? cause.message : 'Не удалось создать пользователя')
    } finally {
      setCreating(false)
    }
  }

  async function toggleUser(user: AdminUser) {
    setUpdatingId(user.id)
    try {
      await apiFetch<{ ok: boolean }>(`/api/admin/users/${user.id}`, {
        method: 'PATCH',
        body: JSON.stringify({ disabled: !user.disabled }),
      })
      showToast('Обновлено')
      users.retry()
    } catch (cause) {
      showToast(cause instanceof Error ? cause.message : 'Не удалось обновить пользователя', { variant: 'error' })
    } finally {
      setUpdatingId(null)
    }
  }

  return (
    <div className="mt-2 grid gap-8">
      <p className="text-muted">Аккаунты с доступом к библиотеке</p>

      <Section title="Новый пользователь">
        <form className="grid max-w-xl gap-4 rounded-app border border-line bg-bg2 p-4" onSubmit={createUser}>
          <Field
            autoComplete="off"
            label="Имя пользователя"
            name="username"
            pattern="[a-z0-9_\-]{3,32}"
            required
            title="От 3 до 32 символов: a-z, 0-9, подчёркивание или дефис"
          />
          <Field autoComplete="new-password" label="Пароль" name="password" required type="password" />
          <label className="flex min-h-11 items-center gap-3 rounded-xl px-1 text-sm font-semibold">
            <input className="size-5 accent-accent" name="is_admin" type="checkbox" />
            Администратор
          </label>
          {createError && <p className="text-sm text-red-300" role="alert">{createError}</p>}
          <Button className="justify-self-start" pending={creating} type="submit" variant="primary">Создать</Button>
        </form>
      </Section>

      <Section title="Все пользователи">
        {users.status === 'loading' && !users.data && <SkeletonCards count={2} label="Загрузка пользователей" />}
        {users.status === 'error' && <ErrorState description={users.error.message} onRetry={users.retry} title="Не удалось загрузить пользователей" />}
        {users.data && (users.data.users?.length ?? 0) === 0 && <EmptyState title="Пользователей пока нет" />}
        {users.data && (users.data.users?.length ?? 0) > 0 && (
          <ul className="grid gap-3" aria-live="polite">
            {users.data.users?.map((user) => (
              <li className={`flex flex-wrap items-center justify-between gap-4 rounded-app border border-line p-4 ${user.disabled ? 'opacity-60' : 'bg-bg2'}`} key={user.id}>
                <div className="grid gap-1">
                  <strong>{user.username}</strong>
                  <span className="text-sm text-muted">
                    {[user.is_owner && 'владелец', user.is_admin && 'админ', user.disabled && 'отключён'].filter(Boolean).join(' · ') || 'пользователь'}
                  </span>
                </div>
                {!user.is_owner && (
                  <Button pending={updatingId === user.id} onClick={() => void toggleUser(user)} variant="quiet">
                    {user.disabled ? 'Включить' : 'Выключить'}
                  </Button>
                )}
              </li>
            ))}
          </ul>
        )}
      </Section>
    </div>
  )
}
