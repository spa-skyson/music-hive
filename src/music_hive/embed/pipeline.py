"""Embed недоготовленных треков (энкодер — get_encoder); векторы — в PG."""

from __future__ import annotations

import logging
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from pathlib import Path

from typing import Any, Callable

import numpy as np
from rich.console import Console
from rich.progress import (
    BarColumn,
    Progress,
    SpinnerColumn,
    TextColumn,
    TimeElapsedColumn,
    TimeRemainingColumn,
)

from music_hive.config import get_settings
from music_hive.db import ensure_db
from music_hive.db.store import (
    list_tracks_needing_embedding,
    mark_feature_failed,
    save_embedding,
)
from music_hive.embed.base import Encoder, get_encoder
from music_hive.embed.clap import device_info
from music_hive.embed.segments import SEGMENT_STRATEGY, load_segment_audio

logger = logging.getLogger(__name__)
console = Console()


@dataclass
class EmbedResult:
    total: int = 0
    computed: int = 0
    failed: int = 0
    skipped_missing: int = 0


def _process_one(
    track: dict,
    *,
    encoder: Encoder,
    segment_sec: float,
    model: str | None = None,
) -> tuple[str, int, str | None]:
    """
    Returns (status, track_id, error).
    status: computed | missing | fail
    """
    tid = int(track["id"])
    path = Path(track["path"])
    md5 = track.get("file_md5") or ""
    if not path.is_file():
        return "missing", tid, f"file missing: {path}"
    if not md5:
        return "fail", tid, "missing file_md5"

    # Файловый энкодер (remote #31): декод и нарезка окон — на сервере,
    # локально сегменты не режем. Duck-typing, как warm_up в F4.1.
    encode_file = getattr(encoder, "encode_file", None)

    def _encode() -> np.ndarray:
        if callable(encode_file):
            return encode_file(path)
        segments = load_segment_audio(
            path, sample_rate=encoder.sample_rate, segment_sec=segment_sec
        )
        return encoder.encode([y for _win, y in segments])

    try:
        # Долговечная копия вектора — сама база PG (§6.4); файлового кеша нет.
        vec = _encode()
        save_embedding(tid, vec, model=model)
        return "computed", tid, None
    except Exception as exc:
        logger.exception("embed failed track_id=%s path=%s", tid, path)
        return "fail", tid, str(exc)


def embed_library(
    *,
    limit: int | None = None,
    force: bool = False,
    workers: int = 1,
    on_progress: Callable[[dict[str, Any]], None] | None = None,
    model: str | None = None,
) -> EmbedResult:
    """
    Compute track embeddings (энкодер — фабрика get_encoder по
    MUSIC_HIVE_EMBEDDING_MODEL) for active non-duplicate tracks that are not ready.

    model (F4.3, job model_activate) — селектор энкодера для фабрики:
    полный ключ ('onnx:<имя>' | 'clap:<repo>') либо имя фабрики ('clap').
    Векторы и проверка готовности всегда идут под ПОЛНЫМ ключом
    encoder.model_key() — имя фабрики 'clap' не должно рождать
    фантомную emb_clap (дефект приёмки F4.3).

    Model inference is heavy — default workers=1 (shared GPU/CPU model).
    """
    settings = get_settings()
    ensure_db()
    encoder = get_encoder(model)
    key = encoder.model_key()
    segment_sec = settings.embed_segment_sec

    remote_mode = callable(getattr(encoder, "encode_file", None))
    if remote_mode:
        # Инференс — на model-сервере; локальный GPU не при чём (#31)
        console.print("[green]Device:[/green] remote model-сервер")
    else:
        device_name, has_cuda = device_info()
        if not has_cuda:
            console.print(
                "[bold yellow]⚠ GPU не найден — энкодер считается на CPU. "
                "На ~100 треках это могут быть десятки минут; на тысячах — часы."
                "[/bold yellow]"
            )
        else:
            console.print(f"[green]Device:[/green] {device_name}")

    console.print(
        f"[bold]Embedding[/bold] model={key} "
        + (
            "windows=сервер (декод и нарезка — на model-сервере)"
            if remote_mode
            else f"strategy={SEGMENT_STRATEGY} windows=start/middle/end × {segment_sec:.0f}s"
        )
    )

    tracks = list_tracks_needing_embedding(limit=limit, force=force, model=key)
    result = EmbedResult(total=len(tracks))
    if not tracks:
        console.print(
            "[green]Нечего эмбеддить — все готовы или библиотека пуста.[/green]"
        )
        return result

    # Warm model once in main thread before workers (если бэкенд умеет)
    warm_up = getattr(encoder, "warm_up", None)
    if callable(warm_up):
        warm_up()

    workers = max(1, workers)
    total = len(tracks)
    done = 0

    def _emit() -> None:
        if on_progress is None:
            return
        pct = (100.0 * done / total) if total else 100.0
        on_progress(
            {
                "phase": "embed",
                "done": done,
                "total": total,
                "pct": round(pct, 2),
                "computed": result.computed,
                "failed": result.failed,
                "message": f"embed {done}/{total} ({pct:.1f}%) · new={result.computed}",
            }
        )

    _emit()
    with Progress(
        SpinnerColumn(),
        TextColumn("[progress.description]{task.description}"),
        BarColumn(),
        TextColumn("{task.completed}/{task.total}"),
        TimeElapsedColumn(),
        TimeRemainingColumn(),
        console=console,
    ) as progress:
        task = progress.add_task("Embedding", total=total)
        with ThreadPoolExecutor(max_workers=workers) as pool:
            futures = {
                pool.submit(
                    _process_one,
                    t,
                    encoder=encoder,
                    segment_sec=segment_sec,
                    model=key,
                ): t
                for t in tracks
            }
            for fut in as_completed(futures):
                status, tid, err = fut.result()
                done += 1
                progress.advance(task)
                progress.update(
                    task,
                    description=(
                        f"Embedding · new={result.computed} fail={result.failed}"
                    ),
                )
                if status == "computed":
                    result.computed += 1
                elif status == "missing":
                    result.skipped_missing += 1
                    mark_feature_failed(tid, err or "missing file")
                else:
                    result.failed += 1
                    mark_feature_failed(tid, err or "unknown error")
                # embed is slow — update every item
                _emit()

    return result
