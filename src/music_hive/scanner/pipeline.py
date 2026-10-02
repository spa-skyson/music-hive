from __future__ import annotations

import json
import logging
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from pathlib import Path

from typing import Any, Callable

from rich.progress import Progress, SpinnerColumn, BarColumn, TextColumn, TimeElapsedColumn

from music_hive.config import get_settings
from music_hive.db import (
    ensure_db,
    link_track_artist_album,
    mark_duplicates,
    mark_missing_inactive,
    set_genres,
    track_file_states,
    update_audio_scalars,
    update_fingerprint_and_lufs,
    upsert_track,
)
from music_hive.db.schema import connect, utcnow
from music_hive.scanner.audio_params import (
    compute_bpm,
    compute_fingerprint,
    compute_key_mode,
    compute_lufs,
)
from music_hive.scanner.hashing import file_md5
from music_hive.scanner.tags import read_tags
from music_hive.scanner.walk import iter_audio_files

logger = logging.getLogger(__name__)


@dataclass
class ScanResult:
    scanned: int = 0
    upserted: int = 0
    skipped_unchanged: int = 0
    failed: int = 0
    inactivated: int = 0
    duplicates_marked: int = 0


def _save_artwork(track_md5: str, data: bytes | None) -> str | None:
    if not data:
        return None
    settings = get_settings()
    out = settings.artwork_cache / f"{track_md5}.jpg"
    if not out.exists():
        out.write_bytes(data)
    return str(out)


# ------------------------------------------------------------------ #40 чекпоинт
# Долгий полный обход (36k файлов — часы анализа) переживает смерть воркера:
# scan_state['rescan_checkpoint'] хранит ватермарк last_path, возобновление
# идёт с него, а не с нуля. Ключ глобальный — полный обход один за раз:
# воркер исполняет job'ы по одной (run_pending(max_jobs=1)), параллельного
# второго обхода нет; чекпоинт дополнительно валидируется по library и
# extract_audio (несовпадение — обход с нуля, см. scan_library).

_CHECKPOINT_KEY = "rescan_checkpoint"
# Ватермарк между записями чекпоинта: ~1400 апдейтов scan_state на 36k
# файлов вместо одного на файл; потеря при крахе — не более 24 файлов.
_CHECKPOINT_EVERY = 25


def _checkpoint_load() -> dict[str, Any] | None:
    with connect() as conn:
        row = conn.execute(
            "SELECT value FROM scan_state WHERE key = ?", (_CHECKPOINT_KEY,)
        ).fetchone()
    if row is None:
        return None
    try:
        cp = json.loads(row["value"])
    except json.JSONDecodeError:
        return None
    return cp if isinstance(cp, dict) else None


def _checkpoint_save(state: dict[str, Any]) -> None:
    with connect() as conn:
        conn.execute(
            """
            INSERT INTO scan_state(key, value) VALUES(?, ?)
            ON CONFLICT(key) DO UPDATE SET value = excluded.value
            """,
            (_CHECKPOINT_KEY, json.dumps(state, ensure_ascii=False)),
        )


def _checkpoint_clear() -> None:
    with connect() as conn:
        conn.execute("DELETE FROM scan_state WHERE key = ?", (_CHECKPOINT_KEY,))


def _process_one(path: Path, *, extract_audio: bool) -> tuple[str, dict | None, str | None]:
    """Returns (status, payload_or_none, error). status: ok|skip|fail"""
    settings = get_settings()
    try:
        stat = path.stat()
        md5 = file_md5(path)
        # Skip if unchanged md5 already in DB — handled by caller via mtime/md5 check optionally
        tags = read_tags(path)
        artwork = _save_artwork(md5, tags.artwork_bytes)

        fingerprint = None
        lufs = None
        bpm = None
        key_name = None
        mode = None
        if extract_audio:
            fingerprint = compute_fingerprint(path)
            lufs = compute_lufs(path, max_seconds=settings.analysis_seconds)
            bpm = compute_bpm(path, max_seconds=settings.analysis_seconds)
            key_name, mode = compute_key_mode(path, max_seconds=min(45.0, settings.analysis_seconds))

        payload = {
            "path": str(path.resolve()),
            "file_md5": md5,
            "file_mtime": float(stat.st_mtime),
            "file_size": int(stat.st_size),
            "title": tags.title,
            "artist": tags.artist,
            "album_artist": tags.album_artist,
            "album": tags.album,
            "year": tags.year,
            "track_number": tags.track_number,
            "duration": tags.duration,
            "bitrate": tags.bitrate,
            "sample_rate": tags.sample_rate,
            "channels": tags.channels,
            "fingerprint": fingerprint,
            "lufs": lufs,
            "artwork_path": artwork,
            "genres": tags.genres,
            "bpm": bpm,
            "key_name": key_name,
            "mode": mode,
        }
        return "ok", payload, None
    except Exception as exc:
        logger.exception("Failed %s", path)
        return "fail", None, str(exc)


def scan_library(
    library: Path | None = None,
    *,
    extract_audio: bool = True,
    workers: int | None = None,
    limit: int | None = None,
    on_progress: Callable[[dict[str, Any]], None] | None = None,
    checkpoint: bool = False,
) -> ScanResult:
    """Полный/инкрементальный обход библиотеки.

    Тяжёлый анализ (md5, теги, fingerprint/LUFS/BPM/key — `_process_one`)
    ЗАПУСКАЕТСЯ ТОЛЬКО для изменившихся файлов: совпадение mtime+size с БД
    (track_file_states) → файл сразу в skipped_unchanged, аудио не читается.
    Это главный short-circuit; чекпоинт ниже не заменяет, а дополняет его —
    он экономит сам обход (walk) после смерти воркера посреди full_rescan.

    #40 checkpoint: писать ватермарк обхода в scan_state['rescan_checkpoint'].
    После краха (reaper вернул job в pending) следующий прогон продолжает с
    last_path детерминированного sorted-walk (iter_audio_files сортирует),
    а по завершении чекпоинт удаляется. Не в finally: падение обязано
    оставить точку возобновления. `limit` (частичные выборки) и
    несовпадение library/extract_audio чекпоинт отключают/сбрасывают.
    """
    settings = get_settings()
    ensure_db()
    lib_root = (library or settings.library).expanduser().resolve()
    files = iter_audio_files(library, settings)
    workers = workers or settings.workers
    result = ScanResult(scanned=len(files))
    known = track_file_states()

    resume_after: str | None = None
    started_at = utcnow()
    use_checkpoint = checkpoint and limit is None
    if use_checkpoint:
        cp = _checkpoint_load()
        if (
            cp
            and cp.get("library") == str(lib_root)
            and bool(cp.get("extract_audio")) == extract_audio
        ):
            resume_after = str(cp.get("last_path") or "") or None
            counts = cp.get("counts") or {}
            result.upserted = int(counts.get("upserted", 0))
            result.skipped_unchanged = int(counts.get("skipped_unchanged", 0))
            result.failed = int(counts.get("failed", 0))
            started_at = str(cp.get("started_at") or started_at)
            logger.info(
                "rescan checkpoint: resume after %s (upserted=%s skipped=%s failed=%s)",
                resume_after,
                result.upserted,
                result.skipped_unchanged,
                result.failed,
            )

    seen: set[str] = set()
    pending: list[Path] = []
    for path in files:
        path_str = str(path.resolve())
        seen.add(path_str)
        if resume_after is not None and path_str <= resume_after:
            # Префикс уже обработан прерванным прогоном, его вклад — в
            # счётчиках чекпоинта. mtime намеренно не проверяем; файлы,
            # изменившиеся за время простоя, подхватит следующий
            # инкрементальный scan.
            continue
        state = known.get(path_str)
        try:
            stat = path.stat()
        except OSError:
            pending.append(path)
            continue
        if (
            state
            and int(state.get("is_active") or 0) == 1
            and int(state.get("file_size") or -1) == int(stat.st_size)
            and float(state.get("file_mtime") or -1) == float(stat.st_mtime)
        ):
            result.skipped_unchanged += 1
        else:
            pending.append(path)
    if limit is not None:
        pending = pending[:limit]

    total = len(pending)
    done = 0

    # Ватермарк чекпоинта: last_path = конец непрерывного префикса готовых
    # файлов. as_completed отдаёт результаты не по порядку, порядок
    # восстанавливаем через done_idx — чекпоинт продвигается только через
    # действительно завершённые файлы, при возобновлении ничего не теряется.
    idx_of: dict[str, int] = {}
    done_idx: set[int] = set()
    watermark = -1
    flushed = -1
    if use_checkpoint:
        idx_of = {str(p.resolve()): i for i, p in enumerate(pending)}

    def _advance_checkpoint(src: Path) -> None:
        nonlocal watermark, flushed
        if not use_checkpoint:
            return
        i = idx_of.get(str(src.resolve()))
        if i is None:
            return
        done_idx.add(i)
        while watermark + 1 in done_idx:
            watermark += 1
        if watermark - flushed >= _CHECKPOINT_EVERY:
            flushed = watermark
            _checkpoint_save(
                {
                    "last_path": str(pending[watermark].resolve()),
                    "library": str(lib_root),
                    "extract_audio": extract_audio,
                    "started_at": started_at,
                    "updated_at": utcnow(),
                    "counts": {
                        "scanned": result.scanned,
                        "upserted": result.upserted,
                        "skipped_unchanged": result.skipped_unchanged,
                        "failed": result.failed,
                    },
                }
            )

    def _emit() -> None:
        if on_progress is None:
            return
        pct = (100.0 * done / total) if total else 100.0
        on_progress(
            {
                "phase": "scan",
                "done": done,
                "total": total,
                "pct": round(pct, 2),
                "upserted": result.upserted,
                "failed": result.failed,
                "message": f"scan {done}/{total} ({pct:.1f}%)",
            }
        )

    _emit()
    with Progress(
        SpinnerColumn(),
        TextColumn("[progress.description]{task.description}"),
        BarColumn(),
        TextColumn("{task.completed}/{task.total}"),
        TimeElapsedColumn(),
    ) as progress:
        task = progress.add_task(f"Scanning 0/{total}", total=total or 1)
        with ThreadPoolExecutor(max_workers=max(1, workers)) as pool:
            futures = {
                pool.submit(_process_one, path, extract_audio=extract_audio): path
                for path in pending
            }
            for fut in as_completed(futures):
                status, payload, err = fut.result()
                src = futures[fut]
                done += 1
                progress.advance(task)
                progress.update(
                    task,
                    description=f"Scanning · ok={result.upserted} fail={result.failed}",
                )
                if status != "ok" or payload is None:
                    result.failed += 1
                    _advance_checkpoint(src)
                    if done % 25 == 0 or done == total:
                        _emit()
                    continue
                path_str = payload["path"]
                seen.add(path_str)
                genres = payload.pop("genres", [])
                album_artist = payload.pop("album_artist", None)
                bpm = payload.pop("bpm", None)
                key_name = payload.pop("key_name", None)
                mode = payload.pop("mode", None)
                tid = upsert_track(payload)
                link_track_artist_album(
                    tid,
                    artist=payload.get("artist"),
                    album=payload.get("album"),
                    album_artist=album_artist,
                    year=payload.get("year"),
                )
                set_genres(tid, genres)
                if extract_audio:
                    update_audio_scalars(
                        tid, bpm=bpm, key_name=key_name, mode=mode, lufs=payload.get("lufs")
                    )
                result.upserted += 1
                _advance_checkpoint(src)
                if done % 25 == 0 or done == total:
                    _emit()

    if on_progress is not None:
        on_progress(
            {
                "phase": "scan_finalize",
                "done": total,
                "total": total,
                "pct": 100.0,
                "message": "marking missing / duplicates",
            }
        )
    result.inactivated = mark_missing_inactive(seen)
    if extract_audio:
        result.duplicates_marked = mark_duplicates()
    if use_checkpoint:
        # Успешное завершение обхода — точка возобновления больше не нужна.
        _checkpoint_clear()
    _emit()
    return result
