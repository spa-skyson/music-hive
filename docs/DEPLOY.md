# Deploy music-hive

PostgreSQL-аутентификация: логин по username+паролю (argon2id), серверные
сессии-cookie, per-user Bearer-токены `mht_*`. Worker не публикуется наружу.

## Auth

Пароль/токен проверяются на каждом защищённом пути; `MUSIC_HIVE_AUTH_DISABLED=1`
полностью выключает вход (только localhost-отладка, не для публичного IP).

| Переменная | Назначение |
|------------|------------|
| `MUSIC_HIVE_DATABASE_URL` | DSN PostgreSQL — **обязателен** для player и worker |
| `MUSIC_HIVE_PASSWORD` | bootstrap: пароль владельца при первом старте (опционально; иначе владелец активируется через `/api/auth/setup`) |
| `MUSIC_HIVE_SESSION_SECRET` | HMAC cookie |
| `MUSIC_HIVE_SECURE_COOKIE=1` | cookie только по HTTPS |
| `MUSIC_HIVE_AUTH_DISABLED=1` | открытый режим (не для публичного IP) |
| `MUSIC_HIVE_CORS_ORIGINS` | список Origin через запятую для cross-origin (Flutter web); пусто = same-origin |

Login: не больше 5 попыток / IP / минуту (`429`). API-токены выпускаются
в Web UI (Профиль → API-токены, префикс `mht_`) и передаются как
`Authorization: Bearer mht_…`.

## Docker Compose (рекомендуется)

```bash
cp .env.example .env
# обязательно: MUSIC_HIVE_LIBRARY (и MUSIC_HIVE_PASSWORD для входа; DSN собирается сам)

make up
make logs
make rescan && make mixes
make smoke
```

Volumes: `music-hive-data` → кэши (эмбеддинги, artwork); `music-hive-pgdata` → PostgreSQL (основное хранилище, F1). Библиотека RO из `MUSIC_HIVE_LIBRARY`.

### Публичный VPS (белый IP)

1. Задай сильные секреты в `.env` (не `AUTH_DISABLED`).
2. Наружу только `:8787` (player). Worker не публикуй.
3. Reverse-proxy (Caddy/Nginx) → HTTPS.
4. Env:
   ```bash
   MUSIC_HIVE_PUBLIC_BASE_URL=https://music.example.com
   MUSIC_HIVE_SECURE_COOKIE=1
   ```
5. Бэкап: `pg_dump -Fc` по cron (см. «Миграция на PostgreSQL» → «Бэкапы»).
6. Play-сессии пишутся в PostgreSQL — переживают рестарт контейнера.

### LAN / телефон

1. Compose уже публикует `8787`.
2. `http://<lan-ip>:8787` → пароль.
3. Для share-ссылок: `MUSIC_HIVE_PUBLIC_BASE_URL=http://<lan-ip>:8787`.

### Share radio

Нужен **ffmpeg** (есть в `Dockerfile.player`).

```bash
MUSIC_HIVE_PUBLIC_BASE_URL=https://music.example.com
# MUSIC_HIVE_SHARE_BITRATE=192k
# MUSIC_HIVE_SHARE_MAX_LISTENERS=4
```

UI **Поделиться** → `…/listen/<token>.mp3`. Отозвать в Профиле. Слушатели не меняют вкус.

## Makefile

| Target | Действие |
|--------|----------|
| `make up` / `down` / `logs` | Compose |
| `make rescan` / `mixes` | jobs (Bearer) |
| `make smoke` | HTTP smoke с auth |
| `make player` | сборка Go |

## Bare-metal

```bash
export MUSIC_HIVE_DATABASE_URL=postgres://…   # обязателен (player и worker)
export MUSIC_HIVE_PASSWORD=…                  # bootstrap владельца (первый старт)
export MUSIC_HIVE_LIBRARY=/path/to/music
music-hive-player migrate   # схема (goose)
music-hive scan && music-hive embed && music-hive clusters
music-hive worker   # terminal 1
./player/bin/music-hive-player   # terminal 2
```

Schema — только goose-миграции (`music-hive-player migrate`); player и
worker DDL не выполняют, воркер на старте делает fail-fast проверку версии
схемы (goose-гейт).

## Миграция с musik

Проект — форк musik; имена env-переменных, файла БД, cookie и Docker-томов переименованы. Обновление без переноса даёт пустую библиотеку и молча игнорирует старые `MUSIK_*` (player и worker при их обнаружении печатают warning при старте).

### База данных

```bash
mv data/db/musik.db data/db/music-hive.db
mv data/db/musik.db-wal data/db/music-hive.db-wal   # при наличии
mv data/db/musik.db-shm data/db/music-hive.db-shm
```

WAL/SHM переноси только при остановленных процессах. Без переноса при старте создастся пустая `music-hive.db`.

### Переменные окружения

Правило: префикс `MUSIK_` → `MUSIC_HIVE_`, остальная часть имени без изменений. Пример: `MUSIK_SESSION_SECRET` → `MUSIC_HIVE_SESSION_SECRET`. Скрытых переименований нет; полный список актуальных переменных — [.env.example](../.env.example).

### Docker

Имя тома изменилось: `musik-data` → `music-hive-data`. Compose при поднятии создаст новый пустой том — перенеси данные со старого:

```bash
docker compose down
docker run --rm -v musik-data:/from -v music-hive-data:/to alpine cp -a /from/. /to/
```

### Сессии

Cookie сменилась с `musik_session` на `music_hive_session` — все сессии инвалидируются, ожидаем повторный вход в Web UI.

### Кэши

Кэш artwork от имён проекта не зависит — переносится как есть (`data/cache/artwork`; векторный кеш на диске не используется — векторы живут в PG).

## Миграция на PostgreSQL (F1 / v2.0)

Единственное хранилище — PostgreSQL 17 + pgvector (сервис `postgres` в compose, порт наружу не публикуется). Схема — goose-миграции: `music-hive-player migrate`.

### Свежая установка на PostgreSQL

```bash
cp .env.example .env
# обязательно: MUSIC_HIVE_LIBRARY (и MUSIC_HIVE_PASSWORD для входа; DSN собирается сам)
# для VPS: смени MUSIC_HIVE_PG_PASSWORD (дефолт music_hive — dev-only)

docker compose up -d postgres                                    # healthcheck pg_isready
docker compose run --rm --no-deps player music-hive-player migrate   # схема: up + status
docker compose up -d                                             # player/worker (схема уже на месте)
# далее задай MUSIC_HIVE_LIBRARY и наполни библиотеку:
docker compose exec worker music-hive scan
docker compose exec worker music-hive embed
```

Повторный прогон миграций на работающем стеке: `docker compose exec player music-hive-player migrate` (goose идемпотентен).

Проверка: `curl -f http://127.0.0.1:8787/api/health` → 200. Пока схема не применена, player/worker перезапускаются с понятной ошибкой (`relation "users" does not exist` / `NotMigratedError`) — штатный fail-closed, поэтому порядок выше: сначала postgres + миграция, потом остальной стек.

Миграции можно накатывать и локально (вне контейнера): `MUSIC_HIVE_DATABASE_URL=postgres://… music-hive-player migrate`. Порт postgres наружу закрыт — либо `docker compose exec`, либо dev-overlay `docker-compose.postgres.yml` (localhost-порт `MUSIC_HIVE_PG_PORT`).

### Перенос существующей SQLite-инсталляции (единственный путь апгрейда)

SQLite-режим снят (#11): единственный способ обновить живую SQLite-инсталляцию —
однократный импорт через `migrate-sqlite`. Импортёр читает файл SQLite **read-only**
(файл — план отката) и переносит данные в PG; `--apply` сам накатывает goose-схему
(повторный запуск безопасен). CLI: `music-hive migrate-sqlite --sqlite <path> --dry-run|--apply|--verify`.

```bash
# 1) Остановить сервисы (SQLite-файл не должен быть занят). -v НЕ указывать!
docker compose down

# 2) Backup файла SQLite из volume (страховка, импортёр его не меняет)
mkdir -p backups
docker run --rm -v music-hive-data:/data -v "$PWD/backups:/backup" alpine \
  sh -c 'cp /data/db/music-hive.db /backup/; cp /data/db/music-hive.db-wal /backup/ 2>/dev/null; true'

# 3) Импорт: dry-run → apply → verify. Порядок переноса и счётчики — в отчёте команды.
docker compose run --rm worker music-hive migrate-sqlite --sqlite /data/db/music-hive.db --dry-run
docker compose run --rm worker music-hive migrate-sqlite --sqlite /data/db/music-hive.db --apply
docker compose run --rm worker music-hive migrate-sqlite --sqlite /data/db/music-hive.db --verify

# 4) Старт на PG (дефолтный compose уже PG-режим: MUSIC_HIVE_DATABASE_URL в compose)
docker compose up -d
curl -f http://127.0.0.1:8787/api/health
```

`docker compose run` поднимет postgres как зависимость (health-gate) и не оставит постоянных player/worker на время импорта.

Перенос однонаправленный: записи, появившиеся в PG после миграции, в SQLite не вернутся. Возврат к прежнему релизу возможен только восстановлением бэкапа SQLite-файла (шаг 2) на старой версии стека.

### Бэкапы PostgreSQL

Вместо копирования `music-hive.db` — `pg_dump` (согласованный дамп, живая база не блокируется):

```bash
mkdir -p backups
docker compose exec -T postgres pg_dump -U music_hive -Fc music_hive \
  > backups/music-hive-$(date +%F).dump
```

cron-пример (ежедневно в 03:00, `%` в crontab экранируется):

```cron
0 3 * * * cd /opt/music-hive && docker compose exec -T postgres pg_dump -U music_hive -Fc music_hive > backups/music-hive-$$(date +\%F).dump
```

Восстановление из дампа:

```bash
docker compose up -d postgres
cat backups/music-hive-2026-09-24.dump | \
  docker compose exec -T postgres pg_restore -U music_hive -d music_hive --clean --if-exists
```

Глобальные объекты (роли) не кастомизируются — единственный владелец `music_hive`, достаточно `pg_dump`; `pg_dumpall --globals-only` нужен только если роли менялись вручную.

### Переменные PostgreSQL

| Переменная | Где действует | Назначение |
|------------|---------------|------------|
| `MUSIC_HIVE_DATABASE_URL` | player, worker | DSN PostgreSQL — обязателен. В compose собирается из `MUSIC_HIVE_PG_PASSWORD`; задавайте целиком только для внешнего/удалённого PG |
| `MUSIC_HIVE_PG_PASSWORD` | `.env` | Пароль `POSTGRES_PASSWORD` и DSN (дефолт `music_hive` — dev-only, на VPS сменить) |
| `MUSIC_HIVE_PG_PORT` | `.env` | localhost-порт postgres в dev-overlay `docker-compose.postgres.yml`; в docker-сети сервис всегда `postgres:5432` |

Кэш artwork остаётся в volume `music-hive-data`; векторы и все данные — в PostgreSQL.

Подробнее: [API.md](API.md) · [MOBILE.md](MOBILE.md) · **[CAPACITY.md](CAPACITY.md)** (ресурсы под 50k треков).
