import { useState } from 'react'
import { Button, EmptyState, EntityCard, ErrorState, Field, IconButton, Section, SkeletonCards, Toast } from './index.ts'

export function Showcase() {
  const [favorite, setFavorite] = useState(false)
  const [toastVisible, setToastVisible] = useState(true)

  return (
    <main className="mx-auto grid max-w-6xl gap-12 p-4 py-10">
      <header>
        <p className="text-sm text-accent">music-hive</p>
        <h1 className="font-display text-4xl font-bold">Витрина UI-компонентов</h1>
        <p className="mt-2 text-muted">Отдельная dev-точка входа, без API и состояния приложения.</p>
      </header>

      <Section title="Кнопки" hint="Основная, вторичная, тихая, занятая и иконка">
        <div className="flex flex-wrap gap-3">
          <Button variant="primary">Слушать</Button>
          <Button>Повторить</Button>
          <Button variant="quiet">Отмена</Button>
          <Button pending>Сохранить</Button>
          <IconButton aria-label="Добавить в избранное" pressed={favorite} onClick={() => setFavorite(value => !value)}>♥</IconButton>
        </div>
      </Section>

      <Section title="Поля" hint="Подсказка, ошибка и disabled">
        <div className="grid max-w-xl gap-4 sm:grid-cols-2">
          <Field hint="Артист, трек или альбом" label="Поиск" placeholder="Поиск…" type="search" />
          <Field defaultValue="короткий" error="Введите не менее 8 символов" label="Пароль" type="password" />
          <Field disabled label="Недоступное поле" value="Только чтение" readOnly />
        </div>
      </Section>

      <Section title="Карточки сущностей" hint="article с независимыми кнопками воспроизведения и избранного">
        <div className="grid grid-cols-[repeat(auto-fit,minmax(11rem,1fr))] gap-4">
          <EntityCard favorite={favorite} kind="track" meta="3:42" onFavorite={() => setFavorite(value => !value)} onPlay={() => setToastVisible(true)} subtitle="Кино" title="Группа крови" />
          <EntityCard kind="album" meta="11 треков" onFavorite={() => undefined} onPlay={() => undefined} subtitle="Massive Attack" title="Mezzanine" />
          <EntityCard kind="artist" meta="48 треков" onFavorite={() => undefined} onPlay={() => undefined} title="Björk" />
        </div>
      </Section>

      <Section title="Состояния списка">
        <SkeletonCards />
        <EmptyState description="Добавьте музыку во вкладке «Загрузка»." title="Пока пусто" />
        <ErrorState description="Проверьте соединение с сервером." onRetry={() => setToastVisible(true)} title="Не удалось загрузить библиотеку" />
      </Section>

      <Section title="Уведомления">
        <div className="grid gap-3">
          {toastVisible && <Toast message="Добавлено в «Потом»" onDismiss={() => setToastVisible(false)} />}
          <Toast message="Не удалось сохранить изменения" variant="error" />
          {!toastVisible && <Button onClick={() => setToastVisible(true)}>Показать уведомление</Button>}
        </div>
      </Section>
    </main>
  )
}
