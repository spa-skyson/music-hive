# Embedding-модели: ONNX-бэкенд (F4.2)

Проект умеет считать эмбеддинги двумя бэкендами за одним протоколом
(`src/music_hive/embed/base.py`):

| `MUSIC_HIVE_EMBEDDING_MODEL` | Бэкенд | Зависимости |
|---|---|---|
| `clap` (по умолчанию) | `transformers` + torch | основные |
| `onnx:<имя>` | `onnxruntime` (CPU) | extra `music-hive[onnx]` |

## Как подключить ONNX-модель

1. Конфиг `models.d/onnx/<имя>.json` + веса `<имя>.onnx` рядом (веса в git не
   коммитятся). Каталог можно переопределить: `MUSIC_HIVE_ONNX_DIR=/path/to/dir`.

   ```json
   {
     "path": "clap-reference.onnx",   // .onnx: относительно конфига или абсолютный
     "sample_rate": 48000,            // сегменты подаются на этой частоте
     "window_sec": 30.0,              // окно инференса; hop_sec (опц.) — шаг, по умолчанию = window_sec
     "input_name": "input_features",  // имя входа графа (sess.get_inputs())
     "output_name": "embedding",      // имя выхода
     "input_kind": "waveform",        // "waveform" ([B, W]) | "log_mel" (мел считает ClapFeatureExtractor)
     "feature_extractor": "laion/…",  // только для log_mel: HF repo или каталог с preprocessor_config.json
     "aggregate": "mean",             // агрегация окон (пока только mean)
     "batch_size": 1,                 // окон за один sess.run (модели с фиксированным batch=1 — оставьте 1)
     "dim": 512                       // опционально: проверка факта при encode (конфиг может врать — §6.1)
   }
   ```

2. Установите extra и переключите модель:

   ```bash
   uv sync --extra onnx            # или pip install 'music-hive[onnx]'
   export MUSIC_HIVE_EMBEDDING_MODEL=onnx:<имя>
   music-hive embed                # job embed считает недостающие векторы
   ```

3. Активация в реестре (PG-режим). Первая запись векторов регистрирует модель
   в `embedding_models` (таблица `emb_<имя>` создаётся сама), но **не флипает
   активную** — смена активной модели это отдельный шаг (job `model_activate`
   из #29 / `activate_model()` в `db.store`):

   ```python
   from music_hive.db.store import activate_model
   activate_model("onnx:<имя>")    # атомарный флип, ровно одна активная
   ```

## Re-embed и смена модели

`list_tracks_needing_embedding` считает готовность по таблице **рабочей**
модели (той, что в `MUSIC_HIVE_EMBEDDING_MODEL`): после переключения job embed
добирает недостающие векторы в `emb_<имя>`, чужие таблицы не трогаются.
Чтение рекомендаций идёт из таблицы **активной** модели — пока не активировали
новую, рекомендации продолжают считаться по старой (страховка от полуэтапа).

**Пространства векторов разных моделей несовместимы** — активная модель всегда
одна (partial unique index в `embedding_models`), смешение в одном запросе
невозможно по построению.

## Ограничения

- Инференс CPU (`CPUExecutionProvider`); GPU-провайдер — когда появится
  профиль с CUDA-ONNX (не сейчас).
- `aggregate` — только `mean`; `input_kind=log_mel` использует
  `ClapFeatureExtractor` (transformers, основная зависимость; подходит
  CLAP-семейству).
- Сегменты подаются на `sample_rate` конфига (ресемплинг делает
  `load_segment_audio` пайплайна); хвост сегмента короче окна отбрасывается,
- Валидация каждого вектора (NaN/Inf, L2 ∈ [0.98, 1.02]) — общая для бэкендов,
  в `ValidatedEncoder`.

## Референс: экспорт CLAP в ONNX

Скрипт `scripts/export_clap_onnx.py` экспортирует
`laion/larger_clap_music_and_speech` → `models.d/onnx/clap-reference.onnx`
+ конфиг `clap-reference.json` (совместимые с `clap`-бэкендом векторы, dim=512):

```bash
uv run --extra onnx python scripts/export_clap_onnx.py
```

Основной путь — скрипт (обёртка `get_audio_features` через `torch.onnx.export`,
тот же движок, что под капотом optimum). Альтернатива для инспекции графа:

```bash
pip install 'optimum[onnx]' && optimum-cli export onnx \
    --model laion/larger_clap_music_and_speech --task feature-extraction out/
```

— у optimum нет конфига под архитектуру `clap`, а `ClapModel.forward` экспортирует
текст+аудио входы **без** `audio_projection`, поэтому CLI-граф не годится для
эмбеддингов напрямую; скрипт — рабочий путь.

### Чек-лист проверки экспорта (скрипт делает сам и падает при нарушении)

- [ ] `dim = 512` (проверка факта, не из конфига);
- [ ] `sample_rate = 48000` (совпадает с `ClapFeatureExtractor`);
- [ ] cosine(torch, onnx) на 30-с сегменте ≥ 0.999;
- [ ] нет NaN/Inf; конфиг `clap-reference.json` сгенерирован;
- [ ] переключение `MUSIC_HIVE_EMBEDDING_MODEL=onnx:clap-reference` + `music-hive embed`
      на 2–3 треках: векторы в `emb_onnx_clap_reference`, cosine с векторами
      `clap`-бэкенда на тех же треках ≥ 0.98.
