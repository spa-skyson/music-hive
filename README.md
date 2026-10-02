# music-hive — self-hosted музыкальный сервер

**music-hive** — self-hosted музыкальный сервер и умный плеер для собственной
коллекции MP3, FLAC и других аудиофайлов. Он превращает папку с музыкой на
компьютере или сервере в личный стриминговый сервис: с веб-плеером, поиском,
избранным, персональным радио и автоматически собранными миксами.

Проект нужен тем, кто хранит музыку у себя и хочет слушать её с разных
устройств, не загружая коллекцию в сторонние сервисы. Библиотека, история
прослушиваний и музыкальный профиль остаются на вашем диске.

> music-hive не скачивает и не продаёт музыку. Для работы нужна собственная
> легально полученная аудиотека. Проект рассчитан на одного владельца.

## Что умеет

- сканировать локальную музыкальную библиотеку и читать теги;
- воспроизводить музыку в браузере и отдавать её через HTTP API;
- искать треки, хранить избранное и историю прослушиваний;
- анализировать звучание треков с помощью CLAP и находить похожую музыку;
- строить персональное радио, которое постепенно учитывает вкус владельца;
- создавать Daily Mix и тематические подборки;
- автоматически обрабатывать новые файлы, добавленные в библиотеку;
- показывать тексты песен и обложки;
- создавать ссылку на непрерывный MP3-эфир для другого плеера;
- работать локально, на домашнем сервере или VPS.

## Как это работает

1. Вы указываете путь к папке со своей музыкой.
2. Python-воркер сканирует файлы, читает метаданные и вычисляет аудиопризнаки.
3. CLAP преобразует звучание каждого трека в числовой вектор. Благодаря этому
   система сравнивает музыку по звуку, даже если жанры и теги заполнены плохо.
4. Go-сервер хранит каталог и историю в PostgreSQL (pgvector для
   эмбеддингов), отдаёт Web UI и стримит аудиофайлы на телефон или компьютер.
5. Лайки, пропуски и прослушивания обновляют профиль вкуса. Радио смешивает
   похожие треки, историю, время суток и небольшую долю новых рекомендаций.

После первичного анализа GPU больше не обязателен: готовую базу и кеш
эмбеддингов можно перенести на обычный маломощный сервер.

**Стек:** Python (сканирование, CLAP, фоновые задачи) + Go (API, стриминг,
рекомендации, Web UI) + PostgreSQL (pgvector).

**Доступ:** логин по username+паролю (Web UI) и per-user Bearer-токены `mht_*` для API.

---

## Архитектура

```
┌─────────────────────┐         ┌──────────────────────┐
│  Web UI / телефон   │  HTTPS  │  Go player :8787     │
│  (browser / API)    │◄───────►│  auth · stream · EMA │
└─────────────────────┘         │  Radio · share MP3   │
                                └──────────┬───────────┘
                                           │ jobs HTTP
                                ┌──────────▼───────────┐
                                │  Python worker :8790 │
                                │  scan · CLAP · mixes │
                                └──────────┬───────────┘
                                           │
                                ┌──────────▼───────────┐
                                │  PostgreSQL + caches │
                                │  data/db + data/cache│
                                └──────────────────────┘
```

| Слой | Где | Порт | Роль |
|------|-----|------|------|
| **Player** | `player/` (Go) | **8787** (публичный) | API, auth, стрим файлов, вкус, Radio/Daily, Web UI, share-radio (ffmpeg) |
| **Worker** | `src/music_hive/` (Python) | **8790** (только localhost / внутренняя сеть) | scan, CLAP embed, clusters, mix_pack, jobs |
| **DB** | `data/db/music-hive.db` | — | треки, фичи, сессии, favorites, jobs |
| **Кеши** | `data/cache/` | — | embeddings `.npy`, artwork |

Контракты и гайды:

| Документ | Содержание |
|----------|------------|
| [docs/API.md](docs/API.md) | HTTP API |
| [docs/openapi.yaml](docs/openapi.yaml) | OpenAPI (`/api/openapi.json`) |
| [docs/DEPLOY.md](docs/DEPLOY.md) | Docker / VPS / HTTPS / share |
| [docs/MOBILE.md](docs/MOBILE.md) | телефон / Flutter |
| [docs/CAPACITY.md](docs/CAPACITY.md) | ресурсы под ~50k треков |
| [docs/ROADMAP.md](docs/ROADMAP.md) | план развития |
| [docs/PUBLISHING.md](docs/PUBLISHING.md) | безопасная публикация backend и Flutter |
| [mobile/README.md](mobile/README.md) | заметки по мобильному клиенту |

---

## Карта репозитория

```
music-hive/
├── README.md                 ← этот файл
├── pyproject.toml            ← Python-пакет `music_hive`, CLI entrypoint
├── Makefile                  ← up/down/rescan/mixes/smoke/bench/player
├── docker-compose.yml        ← player + worker
├── Dockerfile.player         ← Go + ffmpeg
├── Dockerfile.worker         ← Python pipeline
├── .env.example              ← шаблон секретов и путей
├── .env                      ← локальные секреты (не в git)
│
├── docs/                     ← документация
├── scripts/                  ← утилиты
├── src/music_hive/           ← Python: scan / embed / jobs / worker
├── player/                   ← Go: API + UI
├── mobile/README.md          ← контракт отдельного Flutter-клиента
├── tests/                    ← pytest + go tests рядом
└── data/                     ← runtime (БД, кеши; в git почти пусто)
```

### Корень и инфраструктура

| Путь | Назначение |
|------|------------|
| `pyproject.toml` | зависимости Python, скрипт `music-hive` |
| `Makefile` | `make up`, `rescan`, `mixes`, `smoke`, `bench`, `player` |
| `docker-compose.yml` | сервисы `player` (:8787) и `worker` (без публикации порта) |
| `Dockerfile.player` / `Dockerfile.worker` | образы |
| `.env` / `.env.example` | `MUSIC_HIVE_*` переменные |
| `.github/workflows/ci.yml` | CI |
| `.venv/` | обычный Python venv (часто CUDA-wheels — на AMD GPU не подходит) |
| `.venv-rocm/` | отдельный venv Python 3.12 + ROCm torch (локально, в `.gitignore`) |
| `.rocm-extra/` | локально распакованные ROCm-libs при необходимости (`.gitignore`) |

### `data/` — что лежит на диске в рантайме

| Путь | Что это |
|------|---------|
| (внешний PostgreSQL) | данные и векторы — в БД (см. docs/DEPLOY.md) |
| `data/cache/stream/` | кеш транскодов стриминга |
| `data/cache/artwork/` | обложки по hash |
| `data/music/` | опциональная локальная библиотека по умолчанию |
| `data/worker.log` и др. | служебные логи/эксперименты (можно игнорировать) |

Музыкальная коллекция обычно **не** внутри репо: путь задаётся `MUSIC_HIVE_LIBRARY` (и монтируется RO в Docker).

**Бэкап / перенос на сервер:** копируй `data/db/music-hive.db` (+ wal/shm при остановленных процессах) и `data/cache/embeddings/`. Файлы аудио на сервере должны иметь те же MD5 (те же байты) — эмбеддинги подтянутся из кеша.

### `src/music_hive/` — Python

| Путь | Роль |
|------|------|
| `cli.py` | CLI: `music-hive scan`, `embed`, `clusters`, `worker`, … |
| `config.py` | настройки (`MUSIC_HIVE_*`, пути к кешам) |
| `db/schema.py` | доступ воркера к PostgreSQL (пул, goose-гейт) |
| `db/store.py` | чтение/запись треков, embeddings, jobs |
| `scanner/` | обход библиотеки, теги, MD5, LUFS/BPM/key |
| `embed/clap.py` | модель CLAP (GPU/CPU) |
| `embed/segments.py` | окна start/middle/end × 30 с @ 48 kHz |
| `embed/pipeline.py` | очередь эмбеддинга + прогресс |
| `embed/cache.py` | дисковый `.npy` кеш |
| `index/clusters.py` | кластеры по cosine |
| `brain/` | генераторы миксов / explain |
| `discover/` | album tips и т.п. |
| `jobs/` | очередь задач + runner + progress в DB |
| `worker/server.py` | HTTP worker `:8790` |
| `listen/` | история / профиль (offline) |

### `player/` — Go

| Путь | Роль |
|------|------|
| `cmd/music-hive-player/main.go` | точка входа |
| `internal/config/` | env Go-плеера |
| `internal/auth/` | пароль, cookie, Bearer, rate-limit логина |
| `internal/db/` | PostgreSQL: `pg.go` стор, файлы по доменам (pg_*) |
| `internal/index/` | pgvector-индекс: `PGIndex` (ANN-поиск по эмбеддингам) |
| `internal/taste/` | EMA-вкус |
| `internal/queue/` | скоринг очереди: taste + transition + daypart + candidate pool |
| `internal/library/` | правила библиотеки: безопасные пути загрузки, группировка artist/album |
| `internal/playback/` | сессии, радио/плейлист, события прослушивания, вкус |
| `internal/recommend/` | похожие треки / артисты / альбомы |
| `internal/api/` | HTTP: маршруты, JSON, CORS; файлы по эндпоинтам |
| `internal/apitest/` | HTTP-тесты плеера (через публичный Handler) |
| `internal/static/` | Встроенный Web UI: Vite-сборка React-фронта (`dist/`), исходники в `player/frontend/` |

Тесты: HTTP — `internal/apitest`; домен — рядом с пакетом (`internal/library`, `internal/recommend`, `internal/playback`); БД — `internal/pgtest` + живой PG.

Сборка: `make player` → `player/bin/music-hive-player`.

### `scripts/`

| Скрипт | Назначение |
|--------|------------|
| `embed_rocm.sh` | `music-hive embed` через `.venv-rocm` + ROCm libs (AMD GPU) |
| `smoke_api.sh` | HTTP smoke с auth (`make smoke`) |
| `bench_queue.sh` | бенч `radio/start` (`make bench`) |
| `export_paper_cosine.py` | утилита для cosine-таблиц / экспериментов |

### `docs/` и `tests/`

Документация — см. таблицу выше. Тесты: `tests/*.py` (pytest); Go — `player/internal/<пакет>/*_test.go` (`make test-go`).

---

## Быстрый старт

### Docker (рекомендуется на сервере)

```bash
cp .env.example .env
# обязательно: MUSIC_HIVE_LIBRARY (и MUSIC_HIVE_PASSWORD для входа; DSN собирается сам)

make up          # http://127.0.0.1:8787
make rescan      # scan+embed+… через jobs
make mixes
make smoke
```

Подробности: [docs/DEPLOY.md](docs/DEPLOY.md).

### Локально без Docker (CPU / NVIDIA CUDA venv)

```bash
python -m venv .venv && source .venv/bin/activate
pip install -e ".[dev]"

export MUSIC_HIVE_ROOT=$PWD
export MUSIC_HIVE_LIBRARY=/path/to/music
export MUSIC_HIVE_DATABASE_URL=postgres://music_hive:music_hive@127.0.0.1:5432/music_hive?sslmode=disable
export MUSIC_HIVE_PASSWORD=…

# схема применяет player: make player && ./player/bin/music-hive-player migrate up
music-hive scan && music-hive embed && music-hive clusters

make player
./player/bin/music-hive-player   # при MUSIC_HIVE_WORKER_AUTOSTART=1 сам поднимет worker
```

UI: http://127.0.0.1:8787

### AMD GPU (ROCm) — прогон embed на ПК

Обычный `.venv` с CUDA-wheels **не** увидит Radeon. Нужен ROCm torch (у нас: `.venv-rocm`, Python 3.12).

```bash
# после настройки .venv-rocm (см. ниже «AMD / ROCm»)
./scripts/embed_rocm.sh              # только pending
./scripts/embed_rocm.sh --force      # пересчитать всё
```

Проверено на современной AMD GPU с поддерживаемой версией ROCm. Старые
карты без официальной поддержки PyTorch/ROCm могут не работать.

Ориентиры на тестовой библиотеке (~116 треков, `--force`):

| Устройство | Время |
|------------|--------|
| Современная AMD GPU | десятки секунд |
| Современный desktop CPU | несколько минут |

На ~50k: GPU порядка часов; CPU — сильно дольше. Имеет смысл считать на
рабочей станции, затем скопировать `data/db` + `data/cache/embeddings` на
сервер без GPU.

---

## Пайплайн данных (с нуля)

```bash
# 1) чистый старт (остановить player/worker!)
rm -f data/db/music-hive.db data/db/music-hive.db-wal data/db/music-hive.db-shm
rm -rf data/cache/embeddings/* data/cache/artwork/*

# 2) схема создастся сама при первом запуске / init
export MUSIC_HIVE_LIBRARY=/path/to/music

# 3) теги + audio features
music-hive scan                 # или: music-hive scan --tags-only (быстрее, без LUFS/BPM)

# 4) CLAP (лучше GPU)
music-hive embed                # или ./scripts/embed_rocm.sh
# прогресс: CLI + GET /api/jobs/{id} → .progress

# 5) кластеры / миксы
music-hive clusters
# или POST /api/jobs/mix_pack

# 6) плеер
./player/bin/music-hive-player
```

Во время **embed** грузятся и CPU, и GPU — это нормально:

- **CPU** — decode MP3/FLAC, resample, подготовка тензоров (`librosa`)
- **GPU** — inference CLAP
- На трек: 3 окна × 30 с (начало / середина / конец) → средний вектор 512-d

---

## Auth

PostgreSQL-аутентификация: логин по username+паролю (argon2id), серверные
сессии-cookie и per-user Bearer-токены (`mht_*`, страница «Настройки → API»).
`MUSIC_HIVE_AUTH_DISABLED=1` выключает вход (только localhost-отладка).

| | |
|--|--|
| UI | username + пароль → cookie `music_hive_session` |
| API | `Authorization: Bearer mht_…` (per-user токен) |
| Bootstrap | `MUSIC_HIVE_PASSWORD` задаёт пароль владельца при первом старте |
| Login | ≤ 5 попыток / IP / мин → `429` |

Ключевые env — в `.env.example`. Для большой библиотеки см. также `MUSIC_HIVE_WORKERS`, `MUSIC_HIVE_CANDIDATE_POOL_AT` ([docs/CAPACITY.md](docs/CAPACITY.md)).

### Генерация секретов

Никогда не записывайте реальные значения в исходники, Dockerfile или
документацию. Локальный `.env` игнорируется Git.

```bash
umask 077
cp .env.example .env
openssl rand -hex 24  # MUSIC_HIVE_PASSWORD (bootstrap владельца)
openssl rand -hex 32  # MUSIC_HIVE_SESSION_SECRET
```

После утечки или публикации APK со встроенным токеном значения необходимо
ротировать. Правила публикации уязвимостей описаны в [SECURITY.md](SECURITY.md).

---

## Возможности плеера (кратко)

- Каталог / поиск / стрим файлов
- **Radio** по EMA-вкусу: top-K sample старта, explore, recently-played penalty
- Transition boost + daypart; при большом N — **candidate pool** (`MUSIC_HIVE_CANDIDATE_POOL_AT`, default 8000)
- Daily / mixes / favorites
- **Тексты:** `music-hive lyrics` (LRCLIB) → UI «Текст песни» / `GET /api/tracks/{id}/lyrics`
- **Watch:** `music-hive watch` — новые файлы в `MUSIC_HIVE_LIBRARY` → scan/embed/mixes/reload сами
- **Share radio:** непрерывный MP3 через ffmpeg → `GET /listen/{token}.mp3`
- Maturity профиля: `discovering` → `forming` → `ready`
- Jobs с прогрессом (scan/embed/full_rescan)

---

## AMD / ROCm (кратко)

1. Системный ROCm (`rocm-smi`, `/opt/rocm`) + Python **3.12** (wheels до 3.13; системный 3.14 не подходит).
2. Venv `.venv-rocm` + torch/triton с [repo.radeon.com](https://repo.radeon.com/rocm/manylinux/) под вашу версию ROCm.
3. При нехватке `libhipsparselt`: `sudo pacman -S hipsparselt` (Arch) или локальный `.rocm-extra`.
4. Запуск: `./scripts/embed_rocm.sh` (выставляет `LD_LIBRARY_PATH`).

Проверка:

```bash
.venv-rocm/bin/python -c "import torch; print(torch.cuda.is_available(), torch.cuda.get_device_name(0))"
# True <supported AMD GPU>
```

CPU-only тест (спрятать GPU):

```bash
CUDA_VISIBLE_DEVICES= HIP_VISIBLE_DEVICES= .venv-rocm/bin/music-hive embed --force
```

---

## Make-цели

| Target | Действие |
|--------|----------|
| `make up` / `down` / `logs` | Docker Compose |
| `make player` | сборка Go → `player/bin/music-hive-player` |
| `make rescan` | `POST /api/library/rescan` (Bearer) |
| `make mixes` | `POST /api/jobs/mix_pack` |
| `make smoke` | smoke API |
| `make bench` | бенч radio/start |
| `make test-go` | `go test` |

---

## Перенос на сервер (после GPU-прогона на ПК)

1. На ПК: `scan` → `embed` (ROCm) → `clusters`.
2. Остановить player/worker.
3. Скопировать на сервер:
   - `data/db/music-hive.db` (и wal/shm, либо после checkpoint)
   - `data/cache/embeddings/`
4. На сервере: та же (байтово) музыкальная библиотека, `MUSIC_HIVE_LIBRARY=…`, запуск Compose/player **без** обязательного GPU.

---

## Ёмкость ~50k

Сводка: **8–16 GB RAM**, GPU желателен для первого embed, player держит матрицу ~100 MB на 50k×512. Детали и env: [docs/CAPACITY.md](docs/CAPACITY.md).

---

## Источники

- **Публичный репозиторий:** [github.com/spa-skyson/music-hive](https://github.com/spa-skyson/music-hive)
- Основная разработка ведётся в приватном GitLab; GitHub-репозиторий — публикуемое зеркало.
- Зеркало обновляется snapshot'ами: `scripts/sync-github-mirror.sh` (см. [CONTRIBUTING.md](CONTRIBUTING.md)).

---

## Лицензия

Проект распространяется под [GPL-3.0](LICENSE). Форк musik (MIT) — код перенесён под GPL-3.0 с сохранением атрибуции; подробности в LICENSE и THIRD_PARTY.md.

## Происхождение

music-hive — форк проекта [musik](https://github.com/torwin-job/musik) (MIT, © musik contributors).

Обновляешь существующую инсталляцию musik? См. [миграционные заметки](docs/DEPLOY.md#миграция-с-musik) в DEPLOY.md.
