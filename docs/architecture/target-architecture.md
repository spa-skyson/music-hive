# Целевая архитектура music-hive: PostgreSQL/pgvector, multi-user, Subsonic, подключаемые эмбеддинги

Статус: **принятый дизайн** (за исключением явно помеченных «открыто» пунктов в §15).
Дата: 2026-09-24. Исходный коммит анализа: `4020171` (main, ребрендинг musik → music-hive завершён).

Сопутствующие файлы:

- [`subsonic-api.md`](subsonic-api.md) — таблица покрытия эндпоинтов Subsonic, маппинг ID, авторизация, формат ответов.

---

## 1. Контекст: что есть сейчас

Два процесса, общая SQLite (WAL), файловые кеши:

```
┌────────────────────────┐        ┌─────────────────────────────┐
│ Go player :8787        │        │ Python worker :8790         │
│ API /api/* (REST)      │        │ scan (mutagen)              │
│ auth: 1 пароль+Bearer, │──HTTP──│ CLAP 512-d (3 окна × 30с)   │
│ HMAC-cookie            │ reload │ кластеры, ночные миксы      │
│ стриминг (Range/ffmpeg)│  poke  │ lyrics (lrclib), watch      │
│ Web UI (app.js, no build)│      │ polling jobs каждые 5с      │
│ RAM-матрица N×512      │        │ .npy кеш (md5+model)        │
└───────────┬────────────┘        └────────────┬────────────────┘
            │        общая SQLite WAL (22 табл.)│
            └──────────────┬────────────────────┘
                    data/db/music-hive.db
```

Фактические проблемы, которые решает этот дизайн (не гипотетические):

1. **Двойное владение схемой**: Python разворачивает 22 таблицы через `executescript(SCHEMA)`, Go поверх этого делает `CREATE TABLE IF NOT EXISTS` + свою таблицу `schema_migrations`. Любое расхождение — молчаливая деградация.
2. **RAM-матрица как отдельная копия правды**: векторы живут в трёх местах (BLOB, `.npy`, RAM). Отсюда весь механизм `POST /api/reload` + HTTP-poke от worker после каждой джобы.
3. **Полный перебор при поиске**: cosine по всей матрице в Go. На 50k треков это единицы-десятки мс, на 500k — 100–200 мс на запрос; короткие списки (`CandidatePoolAt`) — костыль вокруг этого.
4. **Single-owner auth**: один пароль из env, статический Bearer, stateless-cookie. Ни пользователей, ни per-user истории/избранного.
5. **Нет внешних клиентов**: собственный Web UI + мобильный клиент; мир Subsonic-приложений (Symfonium, DSub, Ultrasonic, Feishin) недоступен.
6. **Модель эмбеддинга захардкожена** в `clap.py` (laion CLAP 512-d); смена модели = смена схемы данных руками.

Принятые направления (решения владельца, не пересматриваются): PostgreSQL + pgvector; полный Subsonic `/rest/`; multi-user; подключаемые модели эмбеддингов; self-hosted характер (docker-compose, минимум движущихся частей).

## 2. Целевая топология

Три контейнера, одна сеть, ноль новых брокеров:

```mermaid
flowchart LR
    subgraph "docker-compose (домашний сервер / VPS)"
        PG[("postgres 17 + pgvector 0.8\nvolume: pgdata")]
        PLAYER["player (Go) :8787\n/api/* — Web UI, cookie+Bearer\n/rest/* — Subsonic (token+salt)\nстриминг, radio, admin"]
        WORKER["worker (Python) :8790\nscan, embeddings (clap/onnx),\nмиксы, lyrics, watch\nports: не публикуются"]
    end
    UI["Web UI (app.js)"] --> PLAYER
    SUB["Symfonium / DSub / Ultrasonic / Feishin"] -->|"Subsonic /rest/"| PLAYER
    LISTEN["Слушатель share-radio"] -->|"/listen/{token}.mp3"| PLAYER
    PLAYER <-->|"pgx (database/sql)"| PG
    WORKER <-->|"psycopg3 + pool"| PG
    PLAYER -.->|"HTTP :8790 (status, опц.)"| WORKER
```

- **Процессов по-прежнему два** (player + worker). PG — единственный новый «движок», и он оправдан решением №1; Kafka/Redis/RabbitMQ не появляются нигде: очередь заданий остаётся таблицей (§5.3).
- Worker не публикует портов (как сейчас), ходит только в PG и читает библиотеку (`:/music:ro`).
- Artwork-кеш и библиотека — файловые тома, без изменений. `.npy`-кеш эмбеддингов **упраздняется** (§6.4).

## 3. Сводка ключевых решений

| # | Решение | Главные альтернативы | Почему так |
|---|---------|----------------------|-----------|
| D1 | Векторы — только в pgvector; RAM-матрица и `.npy`-кеш удаляются | (а) BLOB+RAM как сейчас, PG рядом; (б) гибрид | Матрица и reload-механика существуют лишь потому, что SQLite не умеет ANN. HNSW на 500k быстрее полного перебора и всегда свежий; третья копия правды — источник уже имеющихся багов |
| D2 | Таблица эмбеддингов **на модель**: `emb_<model>(embedding vector(N))`, создаётся активацией модели; реестр `embedding_models`, ровно одна активная | (а) одна нетипизированная колонка `vector`; (б) фиксированная `vector(512)` на инстанс; (в) паддинг до 2000-d; (г) таблица на модель через миграции | HNSW/IVFFlat требуют фиксированной размерности колонки — проверено по документации pgvector. (а) = перебор, 1–2 с на 500k; (б) блокирует легитимную смену модели на другую размерность; (в) ×4 памяти/IO. Активация модели — ~30 строк ограниченного runtime-DDL, единственный момент DDL вне миграций, залогирован в реестре |
| D3 | Индекс **HNSW** (`m=16, ef_construction=64`, `vector_cosine_ops`) | IVFFlat | Библиотека растёт ежедневно: HNSW не требует обучения и rebuild-ов, стабильный recall при инкрементальных вставках. IVFFlat экономнее по памяти, но требует retrain при росте и подбора lists. Запасной вариант при дефиците RAM — `halfvec` |
| D4 | Очередь заданий — таблица `jobs` + polling (2 с), claim через `FOR UPDATE SKIP LOCKED` | LISTEN/NOTIFY; внешняя очередь | Задания длятся минуты; выигрыш 2–5 с старта несущественен, а слушающее соединение + второй канал пробуждения — лишняя движущаяся часть. NOTIFY задокументирован как будущий тюнинг (§5.3) |
| D5 | Мигратор один: **goose**, SQL-файлы в `embed.FS`, применяет player при старте; Python не делает DDL | golang-migrate; alembic; atlas | SQL-файлы нейтральны к языку; goose — одна зависимость, один вызов в main, привычные `+goose Up/Down`. golang-migrate эквивалентен (если команда предпочтёт — не принципиально, важно ровно один инструмент); alembic питон-центричен и тянет SQLAlchemy-концепции, которых у нас нет |
| D6 | Go: `pgx/v5` через адаптер `database/sql` + `pgvector-go`; Python: `psycopg 3` + `psycopg_pool` (1–4 conn) | Нативный `pgxpool` в Go; SQLAlchemy; asyncpg | Адаптер `database/sql` сохраняет форму существующего `Store` — минимальный дифф при переносе ~25 таблиц. Нативный пул — задокументированный escape-hatch (§5.1) |
| D7 | Multi-user: `users` (argon2id + `subsonic_md5`), серверные сессии в PG, per-user API-токены (sha256-хеш); изоляция — фильтрация `WHERE user_id` в репозиторном слое, **без RLS** | RLS; OIDC | RLS требует корректного `SET LOCAL app.user_id` в обоих приложениях и отдельной сервисной роли для worker; для двух доверенных приложений это операционная сложность без соразмерной выгоды. OIDC — отложено (§15) |
| D8 | Subsonic: отдельный пакет `internal/subsonic`, mux на `/rest/`, JSON и XML | Поддержка только JSON; отдельный сервис | DSub использует XML; Symfonium/Feishin — JSON. Один набор DTO с xml/json-тегами покрывает оба. Отдельный процесс не даёт ничего, кроме эксплуатации |
| D9 | Переход SQLite→PG — **одношаговый импортёр** `migrate-sqlite` (stop-the-world на минуты), без double-write | Double-write фаза; онлайн-репликация | Double-write = два кодовых пути SQL-диалектов и рассинхрон как класс багов. Для self-hosted пауза в минуты на обслуживании — приемлема; откат = старый бинарник + нетронутый файл SQLite (§11) |
| D10 | Активная модель эмбеддингов — **на инстанс**, не на пользователя | Модель на владельца | Каталог общий: вектор трека один на всех. Per-owner модели = N-кратная стоимость эмбеддинга и невозможность шаринга ANN-индекса. Профили вкуса всех пользователей всегда в пространстве активной модели |
| D11 | Web UI не переписывается; получает минимальные правки (username в логине, переключение пользователя) | Переписать UI; заморозить UI | Контракты `/api/*` сохраняют форму ответов; меняется только семантика «чьи данные» (по сессии). См. §8.4 |

## 4. Схема данных PostgreSQL

Единственный источник схемы — goose-миграции [`player/migrations/`](../../player/migrations/) (применяет `music-hive-player migrate`; отдельный DDL-файл удалён как протухший, #11). Здесь — ER и обоснования.

### 4.1 ER-диаграмма

```mermaid
erDiagram
    USERS ||--o{ SESSIONS : "логинится"
    USERS ||--o{ API_TOKENS : "владеет"
    USERS ||--o{ USER_FAVORITES : "звёздит"
    USERS ||--o{ USER_RATINGS : "оценивает"
    USERS ||--o{ LISTENING_HISTORY : "слушает"
    USERS ||--o{ USER_TRACK_STATS : "накапливает"
    USERS ||--o{ RECOMMENDATION_IMPRESSIONS : "получает"
    USERS ||--o{ PLAYLISTS : "владеет"
    USERS ||--o{ PLAY_SESSIONS : "state плеера"
    USERS ||--o{ USER_LISTEN_LATER : "откладывает"
    USERS ||--o{ USER_TASTE_PROFILES : "профиль вкуса"
    USERS ||--o{ USER_FEATURE_WEIGHTS : "дрейф по неделям"
    USERS ||--o{ RADIO_SHARES : "создаёт"
    USERS ||--o{ DISCOVER_TIPS : "получает"
    ARTISTS ||--o{ ALBUMS : "выпускает"
    ARTISTS ||--o{ TRACKS : "исполняет"
    ALBUMS ||--o{ TRACKS : "содержит"
    TRACKS ||--o{ TRACK_GENRES : "тегирован"
    GENRES ||--o{ TRACK_GENRES : "тегирует"
    TRACKS ||--|| TRACK_AUDIO_FEATURES : "анализ"
    EMBEDDING_MODELS ||--o{ EMB_TRACKS : "пространство"
    EMBEDDING_MODELS ||--o{ EMB_GROUPS : "центроиды"
    EMBEDDING_MODELS ||--o{ USER_TASTE_PROFILES : "пространство"
    TRACKS ||--o{ EMB_TRACKS : "вектор"
    PLAYLISTS ||--o{ PLAYLIST_TRACKS : "содержит"
    TRACKS ||--o{ PLAYLIST_TRACKS : "входит"
    TRACKS ||--o{ LISTENING_HISTORY : "в истории"
    TRACKS ||--o| LYRICS : "текст"
    TRACKS ||--o{ TRANSITIONS : "переходы"

    USERS { bigint id PK
      citext username UK
      text password_argon2
      text subsonic_md5
      boolean is_admin
      boolean is_owner
      jsonb settings }
    SESSIONS { uuid id PK
      bigint user_id FK
      timestamptz expires_at
      timestamptz revoked_at }
    API_TOKENS { bigint id PK
      bigint user_id FK
      text token_hash UK
      timestamptz expires_at }
    ARTISTS { bigint id PK
      text name_norm UK
      text sort_name
      uuid mbid }
    ALBUMS { bigint id PK
      bigint artist_id FK
      text title
      int year
      bigint cover_track_id FK }
    TRACKS { bigint id PK
      text path UK
      text file_md5
      bigint artist_id FK
      bigint album_artist_id FK
      bigint album_id FK
      int disc_number
      boolean is_active
      text artwork_path }
    TRACK_AUDIO_FEATURES { bigint track_id PK "FK tracks.id"
      real bpm
      text key_name
      int cluster_id
      text status }
    EMBEDDING_MODELS { text model_key PK
      text backend
      int dim
      jsonb params
      boolean is_active }
    EMB_TRACKS { bigint track_id PK "FK tracks.id"
      vector embedding "vector(N) на модель"
      text status }
    EMB_GROUPS { text kind "artist|album"
      bigint group_id
      vector embedding }
    USER_TASTE_PROFILES { bigint user_id FK
      text context
      text model_key FK
      vector vec }
    LISTENING_HISTORY { bigint id PK
      bigint user_id FK
      bigint track_id FK
      timestamptz ts
      text action }
    PLAYLISTS { bigint id PK
      bigint owner_user_id FK
      text visibility "private|public" }
    USER_FAVORITES { bigint user_id FK
      text kind "track|artist|album"
      bigint track_id FK
      bigint artist_id FK
      bigint album_id FK }
    RADIO_SHARES { text token PK
      bigint user_id FK
      timestamptz revoked_at }
    JOBS { bigint id PK
      text kind
      text status
      jsonb payload_json
      jsonb result_json }
```

`emb_<model>` и `emb_<model>_groups` — шаблон; физические таблицы создаются активацией модели (§6.2), в миграцию не входят.

### 4.2 Соответствие 22 таблицам SQLite

| SQLite (сейчас) | PostgreSQL (цель) | Примечания миграции |
|---|---|---|
| `tracks` | `tracks` + `artists` + `albums` | артисты/альбомы становятся сущностями (нужны Subsonic ID); в tracks остаются denorm-строки `artist`/`album` для быстрых листингов; добавляется `disc_number` (ID3-поле Subsonic) |
| `genres`, `track_genres` | без изменений | |
| `features` | **делится**: `track_audio_features` (bpm/key/mode/lufs/cluster/status) + `emb_<model>` (embedding/status) | аудио-фичи не зависят от модели эмбеддинга; статус эмбеддинга становится per-model |
| `listening_history` | + `user_id NOT NULL` | легаси-строки → владелец (id=1) |
| `rec_stats` | `user_track_stats` | per-user counters |
| `recommendation_impressions` | + `user_id` | |
| `transitions` | `transitions` (глобально) | **осознанное упрощение** (потолок: сигнал смешивает пользователей); см. §15 |
| `playlists`, `playlist_tracks` | + `owner_user_id`, `visibility` | |
| `feature_weights` | `user_feature_weights` (+`user_id`, `model_key`) | |
| `user_profile_snapshots` | `user_taste_profiles` (+`user_id`, `model_key`) | |
| `scan_state` | без изменений | |
| `listen_later` | `user_listen_later` | |
| `jobs` | `jobs` (jsonb, `claimed_at`, `attempts`) | |
| `discover_tips` | + `user_id` | подсказки считаются от вкуса → per-user |
| `favorites`, `favorite_artists`, `favorite_albums` | **сливаются** в `user_favorites(kind)` | 3 таблицы → 1 |
| `radio_shares` | + `user_id` | |
| `play_sessions` | + `user_id` | |
| `lyrics` | без изменений (глобальный кеш) | |
| — (новое) | `users`, `sessions`, `api_tokens`, `user_ratings`, `embedding_models`, `emb_*` | |

Всего: 22 старые → 26 целевых (включая шаблонные `emb_*`, исключая `schema_migrations` goose).

### 4.3 Ключевые решения схемы

- **Идентификаторы**: `BIGINT GENERATED ALWAYS AS IDENTITY` везде; импортёр сохраняет старые id треков/плейлистов (§11.3), после загрузки — `setval` секвенций.
- **Время**: `TIMESTAMPTZ` (сейчас ISO-TEXT); импортёр парсит.
- **Artists/Albums upsert** по `name_norm`/`(artist_id, title_norm)` — ключи стабильны, id стабильны между rescan → Subsonic-клиенты не теряют кеш. `album_artist_id` берётся из тега albumartist (fallback: artist первого трека) — корректные компиляции.
- **`embedding_models`**: реестр (model_key, backend, repo, dim, params jsonb, `is_active`); частичный уникальный индекс гарантирует **ровно одну** активную модель.
- **`user_taste_profiles.vec vector`** — без typmod: строк мало (users × contexts), индекс не нужен; размерность валидируется приложением против активной модели. Используется только как *параметр* запроса к ANN-индексу `emb_*`.
- **`user_favorites`/`user_ratings`**: CHECK «ровно одна цель заполнена» + уникальность через COALESCE-выражение; Subsonic star → favorites, rating 1–5 → user_ratings (в Subsonic это разные сущности).

## 5. Доступ к БД из Go и Python

### 5.1 Драйверы и пулы

**Go (player)**: `github.com/jackc/pgx/v5/stdlib` — `sql.Open("pgx", dsn)` поверх `pgxpool`. Существующий `internal/db.Store` (`*sql.DB`) сохраняет форму: замена драйвера + плейсхолдеры `?`→`$1` + типы. Векторы — `github.com/pgvector/pgvector-go` (тип `pgvector.Vector` реализует `driver.Valuer`/`sql.Scanner`; текстовый формат протокола для параметра 512-d ≈ 5 КБ — на фоне одного запроса пренебрежимо).

Пул: `MaxConns = 4×CPU` (cap 16), `MinConns = 2`, `MaxConnLifetime = 30m`. Стриминг и ffmpeg не занимают соединений на время отдачи (запросы короткие).

*Escape-hatch (не делаем сейчас)*: переход на нативный `pgxpool` с бинарным кодеком векторов, если профилирование покажет накладные расходы текстового формата — одна точка замены в `Store`.

**Python (worker)**: `psycopg[binary]` (v3) + `psycopg_pool.ConnectionPool(minsize=1, maxsize=4)`. Текущий паттерн «connect() на операцию» меняется на контекст-менеджер пула; транзакции — как сейчас (`with conn.transaction()` вместо autoclomit sqlite-контекста). `psycopg.types` + `register_vector` (pgvector поставляет адаптер) для записи векторов.

Обе стороны: `statement_timeout=10s`, `idle_in_transaction_session_timeout=30s` в DSN.

### 5.2 Единственный владелец схемы

- goose-миграции (`migrations/*.sql`) вшиты в бинарник player (`embed.FS` + `goose.SetBaseFS`), применяются при старте до открытия порта.
- **Worker никогда не выполняет DDL.** Compose: `worker.depends_on: player: condition: service_healthy`; worker дополнительно делает retry-цикл на первом запросе (защита от запуска вне compose).
- Предположение «один экземпляр player» — зафиксировано (self-hosted, single node). Горизонтальное масштабирование player потребует выноса применятора миграций — задокументировано, не делается.
- `schema.py::SCHEMA` и `ensureSchema()` в Go удаляются в фазе F1 (§14). Единственное исключение из «DDL только в миграциях» — активация модели (§6.2), ограниченный и залогированный в реестре сценарий.

### 5.3 Очередь заданий: остаётся таблица + polling

Схема сохраняется концептуально (проверенный паттерн), меняется реализация claim:

```sql
UPDATE jobs SET status='running', claimed_at=now(), updated_at=now(), attempts=attempts+1
WHERE id = (SELECT id FROM jobs WHERE status='pending' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING *;
```

`SKIP LOCKED` — одна строка, делает безопасным будущий второй worker. Polling интервал: `MUSIC_HIVE_WORKER_POLL_SEC` (по умолчанию теперь **2 с**; для заданий минутной длительности задержка старта нерелевантна).

**HTTP-poke `/api/reload` умирает естественно**: RAM-матрицы больше нет, player читает PG напрямую и всегда видит свежие данные. Эндпоинт остаётся no-op до фазы F5 (совместимость), затем удаляется. Единственные «пихатели» — события воспроизведения от UI (`/api/events`), они уже ходят в player.

`LISTEN/NOTIFY` отвергнут: сокрытие 2-с задержки старта задания не оправдывает постоянное слушающее соединение и второй канал доставки. Если когда-нибудь понадобится интерактивная очередь — `NOTIFY jobs_wake` после INSERT, worker ждёт с таймаутом; задокументировано как изолированное улучшение, менять больше ничего не придётся.

## 6. Embedding-слой

### 6.1 Интерфейс энкодера (Python)

```python
class Encoder(Protocol):
    def key(self) -> str: ...          # 'clap:laion/larger_clap_music_and_speech', 'onnx:<name>'
    def dim(self) -> int: ...          # размерность выходного пространства
    def embed_file(self, path: Path) -> np.ndarray: ...   # L2-нормированный float32
```

Фабрика по `MUSIC_HIVE_EMBEDDING_MODEL`:

| Значение | Бэкенд | Реализация |
|---|---|---|
| `clap` (по умолчанию) | `transformers` | текущий `clap.py` за интерфейсом |
| `onnx:<имя>` | `onnxruntime` | ONNX-экспорт аудио-энкодера; конфиг модели (вход: sr, окна) — в `models.d/onnx/<имя>.json` |
| `custom` | внешний модуль | `MUSIC_HIVE_EMBEDDING_MODULE=mypkg.encoder:make` — точка входа, возвращающая `Encoder` |

Реестр (таблица `embedding_models`) заполняется при первом использовании/активации: `dim` фиксируется из фактического вывода модели на тестовом сигнале (валидация), не из конфига — конфиг может врать.

**Валидация каждого вектора** перед записью: `dim == registry.dim`, L2-норма ∈ [0.98, 1.02] (иначе ренормализация и лог), нет NaN/Inf. Невалидный → `status='failed'` + error, ретрай по общим правилам джоб.

### 6.2 Активация модели и запрет смешения пространств

Смена/первая установка модели = job `model_activate` (worker):

1. `CREATE TABLE IF NOT EXISTS emb_<key>(track_id PK FK, embedding vector(N) NOT NULL, status TEXT DEFAULT 'pending', error TEXT, computed_at TIMESTAMPTZ)`.
2. `CREATE TABLE emb_<key>_groups(kind TEXT CHECK(artist|album), group_id BIGINT, embedding vector(N), updated_at, PK(kind, group_id))` — центроиды артистов/альбомов.
3. `INSERT ... ON CONFLICT (model_key)` в `embedding_models`; **атомарный флип `is_active`** (частичный уникальный индекс не даст двух активных).
4. HNSW-индексы создаются сразу (пустая таблица) и наполняются инкрементально; для первичной загрузки 50k+ — приемлемо за ночь (при массовой миграции модели индекс строится после bulk-insert, §11).
5. Enqueue `re_embed` (все активные треки) + `rebuild_centroids`.

Физическое разделение таблиц делает смешение пространств **невозможным** на уровне схемы: запрос идёт только к таблице активной модели; вектор вкуса из другого пространства не сможет даже скастоваться к `vector(N)` (runtime-ошибка размерности, если приложение ошибётся).

**Политика re-embed при смене модели**:

- Старая таблица `emb_<old>` сохраняется до полного покрытия новой (не удаляется сразу) — мгновенный откат модели без повторного эмбеддинга.
- Профили вкуса (`user_taste_profiles`) пересчитываются из listening_history в новом пространстве после покрытия ≥80% активных треков (taste = агрегат векторов прослушанного; история сохраняется, пересчёт детерминирован). До пересчёта radio/mixes работают в режиме cold-start (maturity=discovering) — тот же механизм, что сейчас у нового пользователя.
- `user_feature_weights` (дрейф) обнуляется для новой модели.
- Удаление старой таблицы — админ-действие (`POST /api/admin/models/{key}/drop`) после недели стабильности; данные невосстановимы, поэтому ручное подтверждение.

### 6.3 Поиск рекомендаций в Go: pgvector вместо RAM-матрицы

Двухступенчатый паттерн (ANN-кандидаты → бизнес-скоринг):

```sql
-- радио/подобное: top-K кандидатов по вкусу/сиду, исключая сыгранное
SELECT t.id, t.artist, t.title, t.album, t.duration, t.artwork_path <> '' AS has_artwork,
       1 - (e.embedding <=> $1::vector) AS sim
FROM emb_<active> e JOIN tracks t ON t.id = e.track_id
WHERE t.is_active AND e.track_id <> ALL($2)      -- exclude-list сессии
ORDER BY e.embedding <=> $1::vector
LIMIT $3;                                          -- K = 200–400
```

Затем в Go — прежняя бизнес-логика без изменений (explore-ratio, new-boost по `created_at`, штрафы `user_track_stats`, cluster-диверсификация), только на пуле из K кандидатов вместо всей матрицы. Численные параметры (K, `hnsw.ef_search`) — конфиг; `SET LOCAL hnsw.ef_search = 80` на сессии запроса.

- `similar/{track_id}` — ANN по вектору трека; клоны (тот же md5/song-key) исключаются как сейчас.
- **Похожие артисты/альбомы** — из `emb_*_groups` (центроиды поддерживает worker после embed/кластеризации; раньше — Go при reload). Похожие по одному артисту «на лету»: среднее векторов его треков (десятки строк, без отдельного кеша).
- Фильтрованный ANN: pgvector ≥0.8 умеет iterative index scans — `WHERE id <> ALL(...)` с сотнями исключений не деградирует в seq scan. Ретрай-страховка: если recall визуально падает на больших exclude — поднять `ef_search`.
- `transitions` остаются глобальным сигналом секвенирования (запросы обычные, без векторов).

### 6.4 Что удаляется

- RAM-матрица `internal/index` (загрузка BLOB, `CandidatePoolAt`, reload) — вся.
- `.npy`-кеш (`embed/cache.py`): PG — долговечная копия; кеш по md5 терял смысл и раньше при смене модели (ключ включает модель). Импортёр читает BLOB из SQLite напрямую.
- `POST /api/reload` → no-op (удаление в F5).
- `user_profile_snapshots` как «снимки» → живые строки `user_taste_profiles` (upsert EMA из Go по-прежнему периодический — поведение сохранено).

### 6.5 Производительность на 50k–500k (оценка)

| Метрика | RAM-матрица (сейчас) | pgvector HNSW 512-d |
|---|---|---|
| Поиск top-10, 50k | ~5–15 мс (полный перебор) | 1–3 мс |
| Поиск top-10, 500k | 100–200 мс | 2–5 мс (recall@10 ≈ 0.95–0.99 при `ef_search` 40–80) |
| Память процессом player | матрица 100 МБ–1 ГБ + клоны индексов | 0 (память у PG: ~1.2 ГБ shared buffers на 500k) |
| Свежесть данных | после reload | всегда |
| Старт player | загрузка всей матрицы | нет |

Радио-рефилл = 1 ANN + 1 fetch статистик (`user_track_stats` по IN-списку) + µs-скоринг → single-digit ms; доминирует ffmpeg. Пропускной способности одного узла хватает с запасом на порядок (десятки RPS сценариев).

Индекс: ~8 КБ/строка на 512-d (вектор 2 КБ + граф HNSW) → 500k ≈ 1.2 ГБ. Если домашний сервер упрётся в RAM: `halfvec` (`vector_half_ops`) режет вдвое, `embedding_halfvec(512)` — задокументированный одним абзацем запасной ход, не делаем заранее.

## 7. Multi-user

### 7.1 Модель пользователей и авторизации

- **users**: `username` (citext, уникальный), `password_argon2` (argon2id, OWASP-параметры m=64 МБ/t=3/p=1), `is_admin`, `is_owner` (первый пользователь; владелец = необорудуемый админ), `settings jsonb`.
- Роли две (admin / listener) — без таблиц ролей (YAGNI): слушатель стримит, скроблит, фаворитит, ведёт плейлисты и профиль вкуса; админ дополнительно управляет библиотекой, заданиями, пользователями, share-ссылками и моделями.
- **Сессии**: серверные строки в PG, cookie `music_hive_session` теперь несёт opaque UUID (имя cookie сохранено — браузерные сессии переживают апгрейд с legacy-проверкой: старое HMAC-значение невалидно → перелогин, приемлемо). TTL 14 дней, sliding-обновление `last_seen_at`, отзыв = `revoked_at`. Stateless-HMAC уходит: ревокация и multi-user невозможны без серверного состояния.
- **API-токены per-user**: `api_tokens` (name, `token_hash` = sha256 hex, показ секрета один раз при создании). `Authorization: Bearer` резолвится в пользователя по хешу. Legacy env-токен `MUSIC_HIVE_API_TOKEN` удалён (#11, этап D): импортёр SQLite переносил его как токен владельца, сейчас переменная игнорируется.
- **Bootstrap**: если `users` пуста — первый `POST /api/auth/login` (или `/api/auth/setup`) создаёт владельца; пароль берётся из `MUSIC_HIVE_OWNER_PASSWORD` при наличии, иначе задаётся в момент setup (форма доступна только пока таблица пуста). Для мигрированного инстанса пароль владельца = текущий `MUSIC_HIVE_PASSWORD` (импортёр хеширует его).

### 7.2 Subsonic-аутентификация (особенность протокола)

Subsonic-клиенты шлют `u=<user>&t=md5(password+salt)&s=<salt>`. Проверка требует серверного эквивалента `md5(password)` — ограничение протокола, одинаковое у Navidrome/Airsonic. Решение:

- `users.subsonic_md5` заполняется при каждой установке пароля.
- Параллельно поддерживается `u&p=<password>` (включая hex-префикс `enc:`): проверяется против argon2id — тогда md5-колонка не участвует.
- OpenSubsonic `tokenAuthentication`: `Authorization: Bearer <api_token>` на `/rest/*` — резолвится через `api_tokens`.
- Компромисс задокументирован: `subsonic_md5` — слабое (несолёное) производное пароля, читающий БД может подобрать словарём. Митигируется: LAN-деплой по умолчанию, `MUSIC_HIVE_SUBSONIC_LEGACY_AUTH=0` отключает token+salt (остаются Bearer и plain-over-TLS), колонку можно затереть нулями ценой отказа от token+salt. Осознанный размен за совместимость с DSub/Symfonium.

### 7.3 Изоляция и RLS

**Изоляция на уровне запросов в репозиторном слое** (`internal/db`): каждый user-scoped метод принимает контекст с `user_id`; WHERE-фильтр живёт в одном месте на сущность; contract-тесты (уже есть infra `apitest`) расширяются кейсами «пользователь A не видит избранное B» (зелёный тест на отрицание — обязательный критерий приёмки F2).

RLS отвергнут осознанно: потребовал бы `SET LOCAL app.user_id` в каждом соединении обоих приложений, отдельной сервисной роли для worker (он пишет в user-таблицы от имени системы: ночные миксы per-user), и усложнил бы дампы/отладку. Сила RLS — защита от багов приложения; при двух доверенных приложениях и ~25 таблицах это неокупаемая операционная сложность. Задокументированный путь ужесточения (если появятся внешние потребители): `ALTER TABLE ... ENABLE ROW LEVEL SECURITY FORCE` + GUC `app.user_id`, без изменения прикладных запросов.

Общий каталог (tracks/artists/albums/genres/emb_*/lyrics/transitions) не изолируется: читают все, пишут worker и админ.

### 7.4 Влияние на существующие механизмы

- **Cookie/Bearer**: форма авторизации API не меняется (`/api/*` как раньше: cookie или Bearer), меняется резолвция → пользователь. Все `/api/*`-ответы сохраняют форму (Web UI не переписывается).
- **Share-radio**: токены получают `user_id` (чьё радио); слушатель анонимен (счётчик `listen_count` как сейчас), скроблинг анонимов не ведётся. Больше одного владельца → каждый шарит своё.
- **Профиль вкуса**: `user_taste_profiles` per-user; EMA в RAM player — map по user_id (ленивая загрузка, LRU-вытеснение не нужно: пользователей десятки).
- **Ночные миксы/подсказки**: job `mix_pack`/`album_tips` итерирует пользователей (стоимость ×N пользователей, N мал).
- **`/api/auth/me`**: `{ok, auth_enabled, user: {name, is_admin}}`; `/api/status`, `/api/profile` — данные сессионного пользователя.

## 8. Слой Subsonic

Подробности — [`subsonic-api.md`](subsonic-api.md); здесь рамка.

### 8.1 Встраивание

- Пакет `internal/subsonic`: свой `http.Handler`, монтируется на `/rest/` в существующем mux. Общий доступ к Store/playback/artwork-сервисам; отдельная DTO-модель (`subsonic-response` конверт: `status/version/type:"music-hive"/serverVersion/openSubsonic`).
- Формат: `f=json|xml` (по умолчанию — XML, как в спецификации; JSON-клиенты всегда шлют `f=json`). Один набор структур с json- и xml-тегами.
- Версия протокола: `1.16.1` + OpenSubsonic-расширения (`tokenAuthentication`, `formPost` не заявляем).

### 8.2 Маппинг сущностей

- `artistID` = `artists.id`, `albumID` = `albums.id`, `songID` = `tracks.id` — стабильны между rescan (upsert по norm-ключам). Это вторая (после совместимости) причина появления таблиц artists/albums.
- ID3-поля: `album`, `title`, `artist`, `albumArtist` (`album_artist_id`), `track`, `disc` (`disc_number`, новая колонка), `year`, `genre` (первый из track_genres; для альбома — мажоритарный), `contentType`/`suffix` по расширению, `duration`, `bitRate`, `playCount` (из listening_history), `created`.
- `getCoverArt`: `tr-<trackID>` (файл трека) / `al-<albumID>` (cover_track_id альбома).
- Транскодинг: `maxBitRate` (kbps) + `format` → существующие ffmpeg-профили `/api/stream` (`?q=mobile`); `format=raw` → оригинал. `contentDir`+Range — как сейчас.

### 8.3 Чего не хватало в данных и добавлено схемой

`user_ratings` (0–5), `disc_number`, сущности artists/albums со стабильными id, `playCount`-агрегаты считаются запросом (индексы `(user_id, ts)`, `(track_id)` покрывают; материализация не нужна до ~10⁷ строк истории — задокументированный потолок).

### 8.4 Web UI и контракты `/api/*`

- Ответы `/api/*` сохраняют форму; наделяются сессионным user. Единственное изменение запроса: `POST /api/auth/login` принимает опциональное `username` (отсутствует → владелец; старые вызывающие стороны не ломаются).
- Правки app.js точечные: поле логина, меню пользователя/выход, скрытие админ-разделов для не-админов. Переписывание UI — вне инициативы.
- `docs/openapi.yaml` версионируется: `api_version` minor-bump на F2 (семантика user-scoping), Subsonic описывается отдельно (`subsonic-api.md`, не OpenAPI — протокол чужой и таблично-описан).

## 9. Конфигурация и поставка

Префикс `MUSIC_HIVE_` сохраняется для всего (единый `.env`, отсутствие коллизий в docker-compose-окружении). `DATABASE_URL` без префикса не используется: **`MUSIC_HIVE_DATABASE_URL`**.

| Переменная | Статус | Назначение |
|---|---|---|
| `MUSIC_HIVE_DATABASE_URL` | **новая, обязательная** | `postgres://user:pass@postgres:5432/music_hive?sslmode=disable&application_name=player\|worker` |
| `MUSIC_HIVE_PLAYER_ADDR`, `MUSIC_HIVE_LIBRARY`, `MUSIC_HIVE_WORKER_URL`, `MUSIC_HIVE_WORKER_AUTOSTART` | без изменений | |
| `MUSIC_HIVE_WORKER_ADDR`, `MUSIC_HIVE_WORKER_POLL_SEC` | без изменений (poll default 2с) | |
| `MUSIC_HIVE_EMBEDDING_MODEL`, `MUSIC_HIVE_CLAP_MODEL`, `MUSIC_HIVE_EMBED_SEGMENT_SEC`, `MUSIC_HIVE_EMBED_WORKERS` | без изменений; `EMBEDDING_MODEL` теперь фабричный ключ (`clap`\|`onnx:*`\|`custom`) | |
| `MUSIC_HIVE_OWNER_PASSWORD` | новая, опциональная | bootstrap владельца при пустой `users` |
| `MUSIC_HIVE_SESSION_TTL` | новая | default 336h |
| `MUSIC_HIVE_SUBSONIC_LEGACY_AUTH` | новая | `0` — отключить token+salt (§7.2) |
| `MUSIC_HIVE_DB_PATH` | legacy: только у импортёра | |
| `MUSIC_HIVE_PASSWORD`, `MUSIC_HIVE_API_TOKEN` | deprecated: читаются **один раз** импортёром/при первом старте → users/api_tokens | |
| `MUSIC_HIVE_SESSION_SECRET` | retired (сессии серверные) | |
| `MUSIC_HIVE_EMBEDDINGS_CACHE` | retired (.npy удалён) | |
| транскодинг/share/CORS/nightly-переменные | без изменений | |

`docker-compose.yml` (цель): сервис `postgres` (образ `pgvector/pgvector:pg17`, volume `pgdata`, healthcheck `pg_isready`), `player` (depends_on postgres-healthy; применяет миграции; публикует 8787), `worker` (depends_on player-healthy; без портов; `:/music:ro`). Бэкапы: `pg_dump` по cron в volume (задокументировать одну строку crontab — кода не пишем).

## 10. Стратегия миграции SQLite → PostgreSQL

Принцип: **один переключаемый релиз, одношаговый импорт, файл SQLite неприкосновенен** (он и есть план отката).

### 10.1 Почему не double-write

Double-write потребовал бы диалектных веток в каждом запросе обоих приложений месяцы (до F2/F3) и создал бы класс багов рассинхронизации, которого сейчас нет. Риск одношагового перехода локализован импортёром с верификацией; пауза — минуты.

### 10.2 Формат перехода

1. Обновление до v2.0 (compose с PG). Старый compose/бинарник остаются рядом работоспособными.
2. `docker compose exec player /music-hive-player migrate-sqlite --sqlite /data/db/music-hive.db [--owner-password ...] [--legacy-api-token ...] --dry-run` → отчёт (план, счётчики строк, проблемы).
3. Та же команда с `--apply`: goose накатывает схему → читает SQLite **read-only** → потоково COPY-ит таблицы в порядке FK → строит HNSW-индекс после загрузки эмбеддингов → `ANALYZE` + `setval` секвенций → печатает верификацию.
4. Переключение `.env` на `MUSIC_HIVE_DATABASE_URL` (или это делает сама команда в `.env`-хеле — нет: руками, явно) и `docker compose up -d`. Player стартует, применяет миграции (no-op), открывает порт.

Пауза обслуживания = шаги 3–4 (минуты на 500k треков: COPY ~10⁵ строк/с, векторный импорт через binary COPY с адаптером pgvector).

### 10.3 Правила преобразования (существенные)

- Владелец: `users` (id=1, is_owner, is_admin); все user-строки получают `user_id=1`.
- `tracks`: upsert `artists`/`albums` из denorm-строк (album_artist = artist первого трека группы); старые id треков сохраняются; `(artist, album)` → `album_id`.
- `features`: split на `track_audio_features` + `emb_clap_...`; BLOB валидируется (`len == dim*4`, dim=512, норма ≈1); битые → `status='pending'` (пересчитает worker).
- `favorites*` (3 таблицы) → `user_favorites` c lookup артистов/альбомов по имени; исчезнувшие цели — skip с warning (отчёт).
- `user_profile_snapshots` → `user_taste_profiles(model_key='clap:…')`; `feature_weights` → `user_feature_weights`.
- `MUSIC_HIVE_PASSWORD` → argon2 + `subsonic_md5`; `MUSIC_HIVE_API_TOKEN` → `api_tokens(owner)`.
- Времена ISO-TEXT → timestamptz; `jobs` (payload/result → jsonb) — импортируются последние 1000 (история, не данные).

### 10.4 Верификация и откат

Верификация (встроена в импортёр, ненулевой код выхода при провале): счётчики строк по всем таблицам; 100 случайных треков (md5, duration, path); для 100 случайных векторов — dim/норма; FK-целостность (`WHERE NOT EXISTS` проверки осиротевших ссылок).

**Откат**: SQLite-файл не менялся; откат = прежний compose + прежний `.env` + прежний бинарник. Данные, записанные в PG после переключения, при откате теряются — принять (окно риска = осознанный выбор оператора; заметить в runbook: переключаться вечером, проверить день). Повторный импорт поверх PG — `--apply --reset` (DROP SCHEMA public CASCADE) или чистый volume.

## 11. Безопасность (по компонентам)

| Компонент | Меры |
|---|---|
| Логин/пароли | argon2id (OWASP), rate-limit на `/api/auth/login` и `/rest/*` (per-IP, расширение существующего `ratelimit.go`), ротация сессии при логине, `subsonic_md5`-компромисс см. §7.2 |
| Сессии | opaque UUID (128 бит), HttpOnly/SameSite=Lax/Secure (как сейчас), серверный отзыв, TTL |
| API-токены | sha256-хеш в БД, показ однажды, `expires_at`/`revoked_at`, лог `last_used_at` |
| Authorzation | user_id-фильтрация в репозиториях + контракт-тесты на изоляцию; `is_admin`-гейты на admin/jobs/users/models эндпоинтах; `/rest/*` — те же гейты |
| Инъекции | только параметризованные запросы (`$n`) обоих приложениях; jsonb вместо ручного JSON-склейки |
| Стриминг/загрузка | без изменений (Range, whitelist путей загрузки уже реализованы); share-токены 128 бит |
| Сеть | worker без опубликованных портов (compose), PG не публикуется наружу; TLS за reverse-proxy при внешнем доступе (SecureCookie уже есть) |
| Секреты | единственный новый секрет — DSN в `.env` (0600); `SESSION_SECRET`/`PASSWORD` выводятся из эксплуатации |

## 12. Наблюдаемость

- `/api/health`: `pg: ok` (SELECT 1 через пул), `embeddings: {model_key, dim, ready/total}`, `tracks`; отказ PG → 503 (compose healthcheck ловит).
- Медленные запросы: `log_min_duration_statement=200ms` на стороне PG (нулевого кода) + `statement_timeout=10s`.
- Джобы: существующий UI/API `/api/jobs` — история уже в таблице; добавляется `claimed_at/attempts` для диагностики зависаний.
- Логи: slog JSON уже в player — добавляется `request_id` (middleware, короткий random) в лог-контекст и в `X-Request-Id`; worker логирует job_id/model_key.
- Метрики: существующие `/api/metrics/*` сохраняются; добавляется p50/p95 латентности ANN-запросов в `/api/metrics/recommendations` (уже считает p50/p95/p99 — расширить источник). OpenTelemetry-экспорт — отложено (§15).

## 13. Риски и их удержание

| Риск | Вероятность | Удержание |
|---|---|---|
| Расхождение SQL-диалектов при переносе Store | средняя | contract-тесты `apitest` гоняются на PG в CI с первой же фазы F1 |
| Recall HNSW ниже перебора на редких запросах | низкая | `ef_search`-тюнинг per-запрос; бенчмарк-джоба «ANN vs exact на 1k запросов» в F1.4 (один скрипт) |
| DDL активации модели ломает миграционный инвариант | низкая | единственный источник — код активации; re-запуск идемпотентен (`IF NOT EXISTS`); реестр отражает физику |
| Subsonic-клиентские причуды (DSub XML, Feishin-версии) | средняя | ручная матрица проверки клиентами в F3; эндпоинт-таблица — чеклист |
| Потеря PG-данных (нет бэкапа) | средняя | runbook: `pg_dump` cron с первого дня v2.0; volume отдельный от кода |
| Импортёр неточен на грязных данных | средняя | `--dry-run` отчёт + верификация + неприкосновенный SQLite |

## 14. Фазовый roadmap

Оценки для одного разработчика part-time: **S** ≤ недели, **M** = 1–3 нед, **L** = 3–6 нед, **XL** = 6–12 нед. Фазы независимо поставляемые; внутри фаз — тоже (указано).

```mermaid
flowchart LR
    F0[F0 ADR+контракты S] --> F1[F1 PG cutover XL]
    F1 --> F2[F2 Multi-user L]
    F1 --> F4[F4 Embeddings pluggable M]
    F2 --> F3[F3 Subsonic L]
    F1 --> F5[F5 Decommission S]
    F4 -. параллельно .-> F3
```

### F0 — фиксация контрактов (S)
Ревью этого документа; правки DDL; OpenAPI minor-bump план. *Ценность: отсутствие импровизации дальше.*

### F1 — переход на PostgreSQL + pgvector (XL; один мажорный релиз v2.0)
Подфазы (внутренний порядок; наружу поставляется вместе):
1. **F1.1 goose + схема** (M): миграции, embed.FS, `00001_init.sql`; CI на PG (testcontainer или сервис).
2. **F1.2 scanner: artists/albums** (M): upsert сущностей, albumartist, disc_number, mbid; бэкфилл при rescan.
3. **F1.3 player на PG** (L): Store → pgx, `?`→`$n`, типы; удаление RAM-индекса; рекоменд/radio через ANN (§6.3); request_id; health с PG.
4. **F1.4 worker на PG** (M): psycopg+pool, очередь SKIP LOCKED, запись emb через pgvector-адаптер, кластеры/центроиды → `emb_*_groups`; модель `clap` регистрируется/активируется автоматически при первом старте.
5. **F1.5 импортёр `migrate-sqlite`** (M): dry-run/apply/verify (§10).
6. **F1.6 compose v2 + docs** (S): postgres-сервис, healthchecks, runbook миграции/отката/бэкапа.

*Поставка: v2.0 с инструкцией «запусти импортёр и переверни env». Ценность: фундамент всего + исчезают reload-механика, .npy, потолок 500k.*

### F2 — multi-user ядро (L; поставляется целиком)
- users/sessions/api_tokens + логин с username + setup-flow; legacy-импорт (уже в F1.5); user-scoping всех `/api/*` + тесты изоляции; админ-API (`/api/admin/users` CRUD, роль is_admin); минимальные правки app.js (S внутри). *Ценность: семьи/друзья на одном сервере.*

### F3 — Subsonic `/rest/` (L; двумя поставками)
- **F3a (M)**: browsing ID3 (getArtists/getArtist/getAlbum/getSong/getIndexes/getMusicDirectory/getGenres/getAlbumList2/search3), stream/download/getCoverArt (транскодинг), scrobble, star/unstar/setRating/getStarred2, ping/getLicense/getMusicFolders/getScanStatus, JSON+XML, token+salt. → *Symfonium/Ultrasonic/Feishin/DSub подключаются и играют.*
- **F3b (M)**: плейлисты CRUD, play-queue sync (getPlayQueue/savePlayQueue ↔ play_sessions), getNowPlaying, getSimilarSongs2/getArtistInfo2/getTopSongs, getRandomSongs/getSongsByGenre, internet radio ↔ radio_shares, lyrics (getLyrics + OpenSubsonic getLyricsBySongId), bookmarks ↔ listen_later, startScan (admin), getOpenSubsonicExtensions + Bearer.
- *Зависимость от F2: per-user scrobble/star/очереди.*

### F4 — подключаемые эмбеддинги (M; независимо от F2/F3, стартует после F1)
- Протокол Encoder + фабрика + реестр (schema уже в F1); ONNX-бэкенд (M внутри: фиксация референс-модели — открытый пункт §15); job `model_activate`/`re_embed`/`rebuild_centroids`; админ-API моделей + пересчёт вкуса; документация политики re-embed. *Ценность: апгрейд моделей без хирургии.*

### F5 — декоммиссия (S)
Удаление: SQLite-путей, `internal/index`, `embed/cache.py`, `/api/reload`, deprecated-env, `MUSIC_HIVE_PASSWORD`-веток. *Ценность: минус тысячи строк.*

**Порядок = ценность:** F1 (фундамент) → F2 → F3a (мобильные клиенты — крупнейший пользовательский выигрыш) → F3b → F4 → F5 (в любой момент после F1). Общая длительность ≈ 4–7 месяцев part-time.

## 15. Открытые вопросы и осознанно отложенное (YAGNI)

- **Референс-модель ONNX** для F4 — выбрать при старте фазы (кандидаты: экспорты CLAP в ONNX, ECAP-TDNN-эмбеддинги); контракт от выбора не зависит.
- **OIDC/LDAP**: локальные пароли покрывают целевой сценарий (домашний сервер); OIDC — отдельная фаза при появлении потребности.
- **Per-user transitions**: глобальная таблица смешивает вкусы; потолок проявится при активных пользователях >5 — тогда добавить `user_id` (миграция тривиальна).
- **RLS**: путь ужесточения описан (§7.3), включать при появлении недоверенных потребителей.
- **OpenTelemetry/экспорт метрик**: health+jobs+slow-query log достаточно; трейсинг — при втором сервисе.
- **Материализация playCount**: запрос по истории достаточен до ~10⁷ строк.
- **Горизонтальное масштабирование player**: предположение singleton зафиксировано (§5.2).
- **Subsonic `getShares`**: семантика share-треков ≠ наше share-radio; не реализуем (наш маппинг — internetRadioStations).
- **`formPost`/Jukebox/HLS/Podcast/Video**: вне области, отвечаем стандартной ошибкой protocol.
