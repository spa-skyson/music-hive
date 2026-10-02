# Capacity — music-hive на ~50 000 треков

Оценка под self-hosted «личный Spotify» с CLAP + Go realtime.

## Где съедаются ресурсы

| Этап | CPU/GPU | RAM | Диск | Время (ориентир) |
|------|---------|-----|------|------------------|
| **scan** (tags + audio analysis) | CPU multi-thread (`MUSIC_HIVE_WORKERS`) | 1–2 GB | artwork cache | ~2–8 ч на 50k (зависит от extract_audio) |
| **embed** (CLAP) | **GPU сильно желателен** | 4–8 GB (модель) + batch | embeddings cache | GPU: ~5–20 ч; **CPU: сутки+** |
| **clusters / mixes** | CPU | 1–2 GB | DB | минуты |
| **player runtime** | 1–2 CPU | **матрица N×512 float32** + Go | DB RO | постоянно |
| **share radio** (ffmpeg) | 1 CPU / слушатель | ~100–200 MB | — | пока слушают |

Матрица в RAM player: `50_000 × 512 × 4 байта ≈ 100 MB` только эмбеддинги (+ meta ~десятки MB). С запасом **player ~256–512 MB**.

## Рекомендуемый VPS / домашняя машина

### Минимум (CPU-only, терпимо ждать первый embed)

- **4 vCPU**, **8 GB RAM**, **100+ GB SSD** (музыка + cache)
- Первый `embed` на CPU — долго; дальше cache на диске
- Candidate pool уже при N≥8000 (`MUSIC_HIVE_CANDIDATE_POOL_AT`)

### Комфортно под 50k (рекомендуется)

- **6–8 vCPU**, **16 GB RAM**
- **GPU** с ≥8 GB VRAM (RTX 3060/4060 / cloud T4) для CLAP
- SSD 200+ GB если FLAC-библиотека большая
- Отдельный диск/volume под `MUSIC_HIVE_LIBRARY` (RO mount в Compose)

### Запас под share + несколько устройств

- +2 CPU если часто шаришь эфир (ffmpeg LAME)
- `MUSIC_HIVE_SHARE_MAX_LISTENERS=2–4`

## PostgreSQL (F1)

С F1 хранилище — PostgreSQL 17 + pgvector (`music-hive-pgdata`); оценки из [target-architecture.md](architecture/target-architecture.md), §6:

| Параметр | Значение на ~50k треков |
|----------|------------------------|
| Индекс HNSW (512-d, `m=16, ef_construction=64`) | ~8 КБ/строка (вектор 2 КБ + граф) → **~400 МБ**; поиск top-10 — **2–5 мс** (recall@10 ≈ 0.95–0.99 при `ef_search` 40–80). На 500k — ~1.2 ГБ; если RAM жмёт — `halfvec` режет вдвое |
| RAM контейнера postgres | планируйте **1–2 GB** (индекс + shared_buffers + коннекты); тюнинг на 50k не требуется |
| Соединения | player: пул `4×CPU`, cap 16 (MinConns 2); worker: `psycopg_pool` 1–4. Итого ≤ 20 — дефолтный `max_connections=100` покрывает с запасом |

Векторы больше не держатся RAM-матрицей в player — ANN-поиск уходит в pgvector (`emb_*` + HNSW); RAM player по-прежнему ~256–512 MB (meta + очередь). Бэкап: `pg_dump -Fc` по cron вместо копирования `music-hive.db` — см. [DEPLOY.md](DEPLOY.md).

## Env для большой библиотеки

```bash
MUSIC_HIVE_WORKERS=6              # scan parallelism
MUSIC_HIVE_CANDIDATE_POOL_AT=8000 # shortlist в Go queue (уже default)
# тест shortlist на малой библиотеке:
# MUSIC_HIVE_CANDIDATE_POOL_AT=50
```

SimsTo в Go параллелится по `GOMAXPROCS` при N≥1500.

## Прогресс pipeline

CLI: rich progress bar (`music-hive scan` / `music-hive embed`).

Jobs / UI: `GET /api/jobs/{id}` → поле `progress`:

```json
{
  "status": "running",
  "progress": {
    "phase": "embed",
    "done": 1200,
    "total": 50000,
    "pct": 2.4,
    "message": "embed 1200/50000 (2.4%) · new=1200"
  }
}
```

Worker пишет progress в `jobs.result_json` каждые ~25 файлов (scan) / каждый трек (embed).

## Порядок первого прогона на 50k

```bash
# 1) только теги (быстро) — опционально
music-hive scan --tags-only   # если флаг есть; иначе полный scan

# 2) полный scan
music-hive scan

# 3) embed (лучше на GPU, оставить на ночь)
music-hive embed

# 4) clusters + mixes
music-hive clusters
# или POST /api/jobs/mix_pack

# 5) player (нужна мигрированная БД и DSN)
export MUSIC_HIVE_DATABASE_URL=postgres://… MUSIC_HIVE_PASSWORD=…
./player/bin/music-hive-player
```

Через API: `POST /api/library/rescan` → poll `GET /api/jobs/{id}` и смотри `progress`.

## Бэкап

Данные и векторы — в PostgreSQL: `pg_dump -Fc` по cron не блокирует живую базу
и сохраняет эмбеддинги (пересчитывать CLAP не придётся). Готовые команды —
в [DEPLOY.md](DEPLOY.md#Бэкапы-postgresql): дамп, cron-пример, восстановление
через `pg_restore`.

## Чего ждать по UX на 50k

- Radio/skip: с candidate pool — миллисекунды–десятки ms на очередь
- Первый cold start UI `/api/library` — тяжёлый JSON; лучше полки artists/albums
- ANN (HNSW) — теперь штатно в PG-режиме (pgvector, `emb_*`); 2–5 мс top-10 на 50k
