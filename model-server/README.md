# model-server — inference-сервис эмбеддингов (подзадача #32)

Автономный HTTP-сервис эмбеддингов аудио для **отдельной VM**: не зависит от
кода `src/music_hive`, образ живёт своей жизнью и релизится независимо.
Держит две модели в RAM и отдаёт векторы по фиксированному контракту, на
который стучится воркер music-hive (энкодер `remote:<url>`, #31).

> [!WARNING]
> **Лицензия весов MuQ — CC-BY-NC 4.0 (некоммерческое использование).**
> Код пакета `muq` — MIT, но чекпойнт `OpenMuQ/MuQ-MuLan-large` распространяется
> под [CC-BY-NC 4.0](https://creativecommons.org/licenses/by-nc/4.0/).
> Веса используются опционально в model-server: не запускайте сервис с
> `MODELS=muq` (или вообще не раздавайте его результаты) в коммерческом
> контуре. CLAP (`laion/larger_clap_music_and_speech`) — MIT. См. также
> `THIRD_PARTY.md` в корне репозитория.

## Модели

| slug | model_key (реестр воркера) | модель | вход | dim |
|------|---------------------------|--------|------|-----|
| `clap` | `clap:laion/larger_clap_music_and_speech` | LAION CLAP (transformers `ClapModel`), MIT | 48 kHz mono | 512 |
| `muq` | `muq:MuQ-MuLan-large` | `OpenMuQ/MuQ-MuLan-large` (пакет `muq`), веса CC-BY-NC 4.0 | 24 kHz mono **fp32** | 512 |
| `onnx:<name>` | `onnx:<name>` | любая ONNX-модель из `/models/onnx` (формат конфигов воркера, #28) | из конфига | из конфига |

Окна/агрегация:

- **CLAP** — та же стратегия `clap_3x30_v1`, что в пайплайне music-hive:
  3 окна по 30 с (start / middle / end; трек короче 30 с — целиком), каждое
  окно → 512-d вектор → per-window L2 → mean → финальный L2.
- **MuQ-MuLan** — модель **сама** режет длинный wav на 10-секундные клипы и
  усредняет латенты (`extract_audio_latents` в исходниках muq), поэтому сервис
  подаёт весь декодированный трек целиком (24 kHz, fp32 — требование README
  MuQ против NaN) → финальный L2.
- **ONNX (`onnx:<name>`)** — математика 1-в-1 с `src/music_hive/embed/onnx.py`
  воркера: трек → окна `window_sec` (шаг `hop_sec`, короткий трек дополняется
  нулями) → батч-инференс onnxruntime → per-window L2 → mean → финальный L2.

Финальная L2-нормализация вектора делается сервисом в одной точке — это
согласовано с валидациями §6.1 воркера (норма ≈ 1.0).

## Варианты образа (cpu / gpu / onnx)

Один `Dockerfile` — три таргета (`docker build --target`); общие слои
(ffmpeg, non-root, healthcheck) вынесены в базовую стадию.

| вариант | образ (Harbor) | инференс | размер ~ | RAM ~ | когда |
|---------|----------------|----------|----------|-------|-------|
| `cpu` (default) | `model-server-cpu` | torch-cpu: CLAP + MuQ | ~2.5 ГБ | 6–8 ГБ (обе модели) | основной: качество CLAP/MuQ на CPU-VM |
| `gpu` (heavy) | `model-server-gpu` | torch-cu124: CLAP + MuQ | ~6 ГБ | как cpu + VRAM GPU | есть NVIDIA GPU + Container Toolkit; быстрее в разы |
| `onnx` | `model-server-onnx` | onnxruntime (CPU) | ~400 МБ | 1–2 ГБ | лёгкая VM, не хочется качать HF-веса в volume; нужны примонтированные `.onnx` |

```bash
# default = cpu (--target можно не указывать — cpu последняя стадия)
docker build -t model-server:dev model-server/

# onnx (лёгкий)
docker build --target onnx -t model-server-onnx:dev model-server/

# gpu (heavy ~6 ГБ)
docker build --target gpu -t model-server-gpu:dev model-server/
```

CI (GitLab) собирает матрицу в Harbor автоматически: `build:model-server-cpu`
и `build:model-server-onnx` — на main и релизных тегах; `build:model-server-gpu`
— manual-джоба (heavy-образ собирается по необходимости). Публикация
версионных тегов — та же джоба `publish:images`, что и для player/worker.

### ONNX-вариант: подготовка весов

Весов `.onnx` в образе **нет** (сотни МБ — гигабайты, git/Harbor для них не
место) — при запуске монтируются каталогом вместе с конфигами:

```bash
# 1. экспорт весов (в корне репо; нужны сеть до HuggingFace и ~2.5 ГБ на веса):
uv run --extra onnx python scripts/export_clap_onnx.py
#    → models.d/onnx/clap-reference.onnx + clap-reference.json

# 2. на VM: MODELS=onnx:clap-reference (имя = имя <имя>.json без расширения),
#    каталог монтируется ro в /models/onnx (env MUSIC_HIVE_ONNX_DIR):
docker run -d --name model-server \
  -p 8100:8100 -e REMOTE_TOKEN=... -e MODELS=onnx:clap-reference \
  -v /srv/models/onnx:/models/onnx:ro \
  cr.home.fwz.ru/homelab/music-hive/model-server-onnx:vX.Y.Z
```

Формат конфига `<имя>.json` — тот же, что у воркера (`models.d/onnx/`,
схема в [docs/embed-models.md](../docs/embed-models.md)): `path` (относительно
конфига), `sample_rate`, `window_sec`, `input_name`, `output_name`, опционально
`hop_sec`, `batch_size`, `dim`, `aggregate`. Ограничение model-server:
`input_kind` — только `waveform` (для `log_mel` нужен transformers, которого
в onnx-образе сознательно нет — такие модели остаются на воркере). `dim`
опциональна: до первого инференса `/v1/models` отдаёт 0, дальше — фактическую.
Проверенный энкодер: `MUSIC_HIVE_REMOTE_MODEL=onnx:clap-reference` на стороне
воркера — реестр (`emb_onnx_clap_reference`) согласован с локальным ONNX-путём.

<!-- ROADMAP (#34+): muq-экспорт в ONNX — scripts/export_muq_onnx.py по
     образцу export_clap_onnx.py (экспорт MuQMuLan.audio_tower/latents);
     веса CC-BY-NC 4.0 — в образ/реестр не класть, только локальный маунт. -->

### GPU-вариант

Требуется NVIDIA Container Toolkit (`docker run --gpus all ...`) и драйвер
с CUDA >= 12.4. Устройство выбирается переменной `MUSIC_HIVE_DEVICE`
(по умолчанию `cpu`): для GPU-инференса ставьте `MUSIC_HIVE_DEVICE=cuda`.
Образ heavy (~6 ГБ: CUDA-рантайм идёт внутри torch-вилок) и собирается в CI
вручную (`build:model-server-gpu` — manual).

## HTTP-контракт

Все `/v1/*` требуют `Authorization: Bearer <REMOTE_TOKEN>`. `/health` — без auth.

### `GET /v1/models`

```json
{"models": [
  {"name": "clap", "model_key": "clap:laion/larger_clap_music_and_speech", "dim": 512, "loaded": true},
  {"name": "muq", "model_key": "muq:MuQ-MuLan-large", "dim": 512, "loaded": false}
]}
```

### `POST /v1/embeddings?model=clap`

Тело — raw-байты аудиофайла (`Content-Type: application/octet-stream`):
mp3 / m4a / flac / wav / ogg / opus. Декод — ffmpeg внутри контейнера.

```json
{"model_key": "clap:laion/larger_clap_music_and_speech", "dim": 512, "embedding": [0.0123, ...]}
```

Ошибки: `401` — нет/неверный Bearer; `404` — нет модели `?model=`; `413` —
тело больше `MAX_BODY_MB`; `415` — не декодируется ffmpeg; `500` — прочее
(проблемы модели и т.п.).

### `GET /health`

```json
{"ok": true, "models_loaded": 1}
```

Форма `/v1/models` + `/v1/embeddings` совместима с OpenAI-образными шлюзами:
сервис можно поставить за **LiteLLM** (опционально) как embedding-провайдера.

## Деплой на VM

Нужны docker + docker compose;.weights качаются с HuggingFace при первом
старте в named volume `model-server-hf` (~3 ГБ).

```bash
# на VM, из корня репо (или скопируйте model-server/ + docker-compose.model-server.yml)
export REMOTE_TOKEN="$(openssl rand -hex 32)"
docker compose -f docker-compose.model-server.yml up -d --build

# прогрев (первый старт качает веса): смотрим, как грузятся модели
curl -s http://localhost:8100/health
```

За Harbor-зеркало: база образа `python:3.12-slim` — замените в
`model-server/Dockerfile` на `cr.home.fwz.ru/dockerhub/python:3.12-slim`.

### Переменные окружения

| env | default | смысл |
|-----|---------|-------|
| `REMOTE_TOKEN` | — (**обязателен**) | Bearer-токен; пустой → сервис не стартует (fail-closed) |
| `MODELS` | `clap,muq` | какие модели поднимать (через запятую, подмножество) |
| `WORKERS` | `1` | воркеры uvicorn; **>1 = копия весов в RAM на каждый** |
| `WARMUP` | `1` | `0` — не греть модели при старте (грузятся при первом запросе) |
| `MAX_BODY_MB` | `100` | лимит тела запроса, свыше — 413 |
| `DECODE_TIMEOUT` | `120` | таймаут ffmpeg-декода, сек |
| `HF_HOME` | `/cache/hf` | кеш весов HuggingFace (volume) |
| `MUSIC_HIVE_ONNX_DIR` | `/models/onnx` | каталог `.onnx` + конфигов (onnx-вариант; монтируйте `:ro`) |
| `MUSIC_HIVE_DEVICE` | `cpu` | torch-устройство (`cuda` на gpu-образе) |

### RAM-бюджет

CPU-вариант, обе модели в одном процессе (CLAP ~600 МБ + MuQ-MuLan ~700M
параметров fp32 ≈ 2.8 ГБ веса + активации/декод) — суммарно **≈ 6–8 ГБ**.
Рекомендация `mem_limit: 8g` закомментирована в
`docker-compose.model-server.yml`. Сжать бюджет можно `MODELS=clap` или
`MODELS=muq`. ONNX-вариант: одна модель (например, CLAP onnx ~600 МБ fp32)
+ onnxruntime ≈ **1–2 ГБ**; размеры образов — в таблице «Варианты образа».

## curl-примеры

```bash
TOKEN="$REMOTE_TOKEN"

# список моделей
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8100/v1/models

# эмбеддинг трека
curl -s -X POST "http://localhost:8100/v1/embeddings?model=clap" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @/music/track.flac

# та же песня через MuQ (веса CC-BY-NC 4.0!)
curl -s -X POST "http://localhost:8100/v1/embeddings?model=muq" \
  -H "Authorization: Bearer $TOKEN" \
  --data-binary @/music/track.flac
```

## Связка с music-hive

Воркер music-hive (>= #31) умеет энкодер `remote:<url>` — он шлёт файл
целиком на этот сервис и получает вектор. На стороне music-hive:

```bash
MUSIC_HIVE_EMBEDDING_MODEL=remote:http://<vm-ip>:8100
MUSIC_HIVE_REMOTE_MODEL=clap        # или muq
MUSIC_HIVE_REMOTE_TOKEN=<тот же REMOTE_TOKEN>
```

`model_key`, которые возвращает сервис (`clap:laion/...`,
`muq:MuQ-MuLan-large`), совпадают с ключами реестра воркера — таблица
`embedding_models`/`emb_<slug>` согласована, отдельные векторы не смешиваются.

## Разработка и тесты

```bash
# из корня репо; тесты идут на мок-энкодерах, веса не нужны
uv run pytest model-server/ -q

# локальный запуск (нужны ffmpeg + настоящие зависимости из requirements.txt)
REMOTE_TOKEN=dev uvicorn app:app --port 8100   # из каталога model-server/
```

Структура: `app.py` — HTTP-слой (контракт, auth, лимиты, L2); `encoders.py` —
ffmpeg-декод и обёртки моделей (CLAP/MuQ — ленивая загрузка из HF-кеша
`HF_HOME`; ONNX — ленивая onnxruntime-сессия по конфигу, `MUSIC_HIVE_ONNX_DIR`).
Варианты образа cpu/gpu/onnx — таргеты одного `Dockerfile` (см. «Варианты
образа»); устройство torch — `MUSIC_HIVE_DEVICE`.
