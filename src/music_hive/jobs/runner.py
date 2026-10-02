"""Process background jobs by kind."""

from __future__ import annotations

import logging
import threading
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

from music_hive.brain.generators import generate_daily
from music_hive.brain.mixes import generate_mix_pack
from music_hive.config import get_settings
from music_hive.db.schema import connect
from music_hive.db.store import activate_model, list_models, pending_count
from music_hive.discover.albums import rebuild_discover_tips
from music_hive.embed import embed_library
from music_hive.embed.base import AVAILABLE_BACKENDS
from music_hive.index import assign_clusters
from music_hive.jobs.queue import (
    claim_next,
    enqueue_job,
    fail_job,
    find_active_job,
    finish_job,
    heartbeat_job,
    update_job_progress,
)
from music_hive.listen.profile import CONTEXT_OFFLINE, build_profile
from music_hive.scanner import scan_library

log = logging.getLogger(__name__)

JOB_KINDS = frozenset(
    {
        "scan",
        "embed",
        "clusters",
        "daily",
        "album_tips",
        "full_rescan",
        "mix_pack",
        "model_activate",
        "model_status",
    }
)

# After these jobs the Go player should reload its in-memory matrix / tips.
_RELOAD_KINDS = frozenset(
    {
        "embed",
        "full_rescan",
        "clusters",
        "daily",
        "album_tips",
        "mix_pack",
        "model_activate",
    }
)


def _progress_cb(job_id: int, *, start: float = 0, span: float = 100):
    def _cb(progress: dict[str, Any]) -> None:
        try:
            mapped = dict(progress)
            pct = progress.get("pct")
            if isinstance(pct, (int, float)):
                mapped["step_pct"] = round(float(pct), 2)
                mapped["pct"] = round(start + (float(pct) / 100.0) * span, 2)
            update_job_progress(job_id, mapped)
            log.info(
                "job #%s progress %s",
                job_id,
                progress.get("message") or progress.get("phase"),
            )
        except Exception as exc:  # noqa: BLE001
            log.debug("progress update failed: %s", exc)

    return _cb


def _notify_player_reload(kind: str) -> None:
    if kind not in _RELOAD_KINDS:
        return
    settings = get_settings()
    url = settings.player_reload_url.strip()
    if not url:
        return
    try:
        req = urllib.request.Request(url, method="POST", data=b"")
        with urllib.request.urlopen(req, timeout=10) as resp:
            log.info("notified player reload after %s → %s (%s)", kind, url, resp.status)
    except urllib.error.URLError as exc:
        log.warning("player reload notify failed (%s): %s", url, exc)
    except Exception as exc:  # noqa: BLE001
        log.warning("player reload notify failed: %s", exc)


def _run_scan(
    payload: dict[str, Any],
    *,
    job_id: int | None = None,
    progress_start: float = 0,
    progress_span: float = 100,
) -> dict[str, Any]:
    settings = get_settings()
    library = Path(payload["library"]) if payload.get("library") else settings.library
    # #40: чекпоинт обхода (scan_state.rescan_checkpoint) — долгий full_rescan
    # переживает reaper-reset: возобновление с last_path, а не с нуля.
    # Внутри scan_library чекпоинт отключается при limit (тестовые выборки).
    result = scan_library(
        library,
        extract_audio=not payload.get("tags_only", False),
        workers=payload.get("workers"),
        limit=payload.get("limit"),
        checkpoint=True,
        on_progress=(
            _progress_cb(job_id, start=progress_start, span=progress_span)
            if job_id is not None
            else None
        ),
    )
    return {
        "scanned": result.scanned,
        "upserted": result.upserted,
        "skipped_unchanged": result.skipped_unchanged,
        "failed": result.failed,
        "inactivated": result.inactivated,
        "duplicates_marked": result.duplicates_marked,
    }


def _run_embed(
    payload: dict[str, Any],
    *,
    job_id: int | None = None,
    progress_start: float = 0,
    progress_span: float = 100,
) -> dict[str, Any]:
    settings = get_settings()
    result = embed_library(
        limit=payload.get("limit"),
        force=bool(payload.get("force", False)),
        workers=payload.get("workers", settings.embed_workers),
        on_progress=(
            _progress_cb(job_id, start=progress_start, span=progress_span)
            if job_id is not None
            else None
        ),
    )
    return {
        "total": result.total,
        "computed": result.computed,
        "failed": result.failed,
        "skipped_missing": result.skipped_missing,
    }


def _chain_embed(after_job_id: int | None) -> int | None:
    """#57: следующая ступень быстрого обновления — embed после scan.

    Ставится только при успешном standalone-скане (исключение из
    _run_scan до сюда не доходит: run_one → fail_job) — упавшее
    обновление эмбеддинги за собой не тянет. Дубли исключены: активная
    (pending/running) embed-джоба → новая не ставится. Embed
    инкрементальный (embed_library force:false), изолированная джоба
    видна в панели задач со своей ошибкой, если энкодер недоступен.
    Возвращает id новой джобы или None, если уже идёт embed.

    Вызывается только из process_job для standalone scan: full_rescan
    считает embed инлайн сам, chain-маркер в его payload игнорирует.
    """
    active = find_active_job("embed")
    if active is not None:
        log.info(
            "chain scan #%s → embed: активная job #%s (%s), не дублирую",
            after_job_id,
            active["id"],
            active["status"],
        )
        return None
    job = enqueue_job("embed", {"trigger": "scan_chain"})
    log.info("chain scan #%s → embed job #%s", after_job_id, job["id"])
    return int(job["id"])


def _run_clusters(payload: dict[str, Any]) -> dict[str, Any]:
    result = assign_clusters(k=int(payload.get("k", 8)))
    return {"k": result.k, "n": result.n, "inertia": result.inertia}


def _run_daily(payload: dict[str, Any]) -> dict[str, Any]:
    build = generate_daily(
        size=int(payload.get("size", 25)),
        explore_ratio=payload.get("explore_ratio"),
        # F2.4: API может запустить персональный daily — user_id из payload
        # (опционально; None → владелец, как раньше)
        user_id=int(payload["user_id"]) if payload.get("user_id") is not None else None,
    )
    pid = build.persist()
    return {
        "playlist_id": pid,
        "name": build.name,
        "tracks": len(build.entries),
        "meta": build.meta,
    }


def _run_mix_pack(payload: dict[str, Any]) -> dict[str, Any]:
    # refresh tips first so "Новинки" has data
    try:
        rebuild_discover_tips()
    except Exception as exc:  # noqa: BLE001
        log.warning("album_tips before mix_pack: %s", exc)
    return generate_mix_pack(
        daily_size=int(payload.get("daily_size", 25)),
        for_you_size=int(payload.get("for_you_size", 30)),
        weekday_size=int(payload.get("weekday_size", 25)),
        weekly_size=int(payload.get("weekly_size", 40)),
        new_size=int(payload.get("new_size", 20)),
    )


def _run_album_tips(payload: dict[str, Any]) -> dict[str, Any]:
    return rebuild_discover_tips(
        new_album_days=int(payload.get("new_album_days", 14)),
        limit_new=int(payload.get("limit_new", 10)),
        limit_old=int(payload.get("limit_old", 10)),
    )


def _run_full_rescan(payload: dict[str, Any], *, job_id: int | None = None) -> dict[str, Any]:
    out: dict[str, Any] = {}
    if job_id is not None:
        update_job_progress(job_id, {"phase": "full_rescan", "message": "scan…", "pct": 0})
    out["scan"] = _run_scan(
        payload, job_id=job_id, progress_start=0, progress_span=40
    )
    if job_id is not None:
        update_job_progress(job_id, {"phase": "full_rescan", "message": "embed…", "pct": 40})
    out["embed"] = _run_embed(
        payload, job_id=job_id, progress_start=40, progress_span=35
    )
    if job_id is not None:
        update_job_progress(job_id, {"phase": "full_rescan", "message": "clusters…", "pct": 75})
    out["clusters"] = _run_clusters(payload)
    if job_id is not None:
        update_job_progress(job_id, {"phase": "full_rescan", "message": "album_tips…", "pct": 85})
    out["album_tips"] = _run_album_tips(payload)
    if job_id is not None:
        update_job_progress(job_id, {"phase": "full_rescan", "message": "mix_pack…", "pct": 92})
    out["mix_pack"] = _run_mix_pack(payload)
    return out


def _encoder_name(key: str) -> str | None:
    """model_key реестра → селектор фабрики get_encoder (F4.3).

    Возвращает сам ключ: 'onnx:<имя>' фабрика собирает как есть,
    'clap:<repo>' — с явным repo (полный ключ — дефект приёмки: имя
    фабрики 'clap' рождало фантомную emb_clap). Неизвестный бэкенд
    (часть до ':') → None — фабрика его не знает, re-embed пропускается,
    activate_model даст внятную ошибку.
    """
    kind = key.split(":", 1)[0]
    if kind in AVAILABLE_BACKENDS:
        return key
    return None


def _taste_user_ids() -> list[int]:
    """Пересчёт вкуса: все пользователи users."""
    with connect() as conn:
        rows = conn.execute("SELECT id FROM users ORDER BY id").fetchall()
    return [int(r["id"]) for r in rows]


def _run_model_activate(
    payload: dict[str, Any], *, job_id: int | None = None
) -> dict[str, Any]:
    """F4.3 (#29): смена активной модели — re-embed → флип реестра → вкус.

    Порядок: сначала embed (первая запись создаёт emb_<slug> и строку
    реестра, §6.2 — активировать незарегистрированную модель нельзя),
    затем атомарный activate_model, затем пересчёт вкуса всех
    пользователей под ключ новой активной модели. Идемпотентно: повторный
    прогон не находит недостающих векторов, флип в ту же модель — no-op.
    """
    key = str(payload.get("model_key") or "").strip()
    if not key:
        raise ValueError("payload.model_key обязателен, напр. 'onnx:toy'")
    out: dict[str, Any] = {"model_key": key}
    name = _encoder_name(key)
    if name is None:
        log.warning(
            "model_activate %s: неизвестный бэкенд — фабрика его не собирает, "
            "re-embed пропущен",
            key,
        )
    else:
        if job_id is not None:
            update_job_progress(
                job_id,
                {"phase": "model_activate", "message": f"embed {key}…", "pct": 2},
            )
        result = embed_library(
            workers=payload.get("workers", get_settings().embed_workers),
            model=name,
            on_progress=(
                _progress_cb(job_id, start=5, span=70) if job_id is not None else None
            ),
        )
        out["embed"] = {
            "total": result.total,
            "computed": result.computed,
            "failed": result.failed,
            "skipped_missing": result.skipped_missing,
        }
    if job_id is not None:
        update_job_progress(
            job_id, {"phase": "model_activate", "message": f"activate {key}", "pct": 80}
        )
    activate_model(key)
    # Вкус: per-user offline_report под ключ активной модели; контекст
    # 'global' не трогаем — он принадлежит Go EMA (authority F2.4).
    user_ids = _taste_user_ids()
    ready = 0
    for i, uid in enumerate(user_ids, 1):
        profile = build_profile(context=CONTEXT_OFFLINE, persist=True, user_id=uid)
        if profile.ready:
            ready += 1
        if job_id is not None:
            update_job_progress(
                job_id,
                {
                    "phase": "model_activate",
                    "message": f"taste {i}/{len(user_ids)}",
                    "pct": round(85.0 + 15.0 * i / max(len(user_ids), 1), 2),
                },
            )
    out["taste"] = {"users": len(user_ids), "ready": ready}
    return out


def _run_model_status(payload: dict[str, Any]) -> dict[str, Any]:
    """F4.3: мгновенный срез реестра (job без побочных эффектов)."""
    models = list_models()
    active = next((m["key"] for m in models if m["active"]), None)
    return {"active": active, "models": models, "pending": pending_count(active)}


def process_job(job: dict[str, Any]) -> dict[str, Any]:
    kind = job["kind"]
    payload = job.get("payload") or {}
    job_id = int(job["id"]) if job.get("id") is not None else None
    if kind == "scan":
        result = _run_scan(payload, job_id=job_id)
        # #57: быстрое обновление (rescan {"full":false}) — одно действие
        # «scan → embed»: маркер цепочки ставит плеер в payload джобы.
        if payload.get("chain_embed"):
            result["embed_job_id"] = _chain_embed(job_id)
        return result
    if kind == "embed":
        return _run_embed(payload, job_id=job_id)
    if kind == "clusters":
        return _run_clusters(payload)
    if kind == "daily":
        return _run_daily(payload)
    if kind == "mix_pack":
        return _run_mix_pack(payload)
    if kind == "album_tips":
        return _run_album_tips(payload)
    if kind == "full_rescan":
        return _run_full_rescan(payload, job_id=job_id)
    if kind == "model_activate":
        return _run_model_activate(payload, job_id=job_id)
    if kind == "model_status":
        return _run_model_status(payload)
    raise ValueError(f"unknown job kind: {kind}")


class _Heartbeat:
    """#40: фоновое продление аренды (claimed_at), пока job исполняется.

    Daemon-тред: без него смерть воркера неотличима от долгой job'ы —
    reaper не может тронуть «живую» аренду. Интервал ~lease/10 (≤30с):
    даже пара пропущенных heartbeat не даёт аренде истечь (lease 300с).
    """

    def __init__(self, job_id: int, interval: float) -> None:
        self._job_id = job_id
        self._interval = interval
        self._stop = threading.Event()
        self._thread = threading.Thread(
            target=self._loop, name=f"job-heartbeat-{job_id}", daemon=True
        )

    def _loop(self) -> None:
        while not self._stop.wait(self._interval):
            try:
                heartbeat_job(self._job_id)
            except Exception:  # noqa: BLE001 — потерянный heartbeat чинит reaper
                log.debug("heartbeat job #%s failed", self._job_id, exc_info=True)

    def __enter__(self) -> "_Heartbeat":
        self._thread.start()
        return self

    def __exit__(self, *exc: object) -> None:
        self._stop.set()
        self._thread.join(timeout=5.0)


def run_one() -> dict[str, Any] | None:
    """Claim and process a single pending job. Returns job summary or None."""
    job = claim_next()
    if not job:
        return None
    job_id = int(job["id"])
    kind = job["kind"]
    log.info("running job #%s kind=%s", job_id, kind)
    # ~lease/10 (≤30с): heartbeat не подпускает reaper к живой job'е (lease 300с).
    interval = max(1.0, min(30.0, get_settings().lease_sec / 10.0))
    try:
        if kind not in JOB_KINDS:
            raise ValueError(f"unknown job kind: {kind}")
        with _Heartbeat(job_id, interval):
            update_job_progress(
                job_id, {"phase": kind, "message": "starting", "pct": 0}
            )
            result = process_job(job)
            finish_job(job_id, result)
        _notify_player_reload(kind)
        log.info("job #%s done", job_id)
        return {"id": job_id, "kind": kind, "status": "done", "result": result}
    except Exception as exc:
        log.exception("job #%s failed", job_id)
        fail_job(job_id, str(exc))
        return {"id": job_id, "kind": kind, "status": "failed", "error": str(exc)}


def run_pending(*, max_jobs: int | None = None) -> list[dict[str, Any]]:
    """Process pending jobs until queue is empty or max_jobs reached."""
    results: list[dict[str, Any]] = []
    while True:
        if max_jobs is not None and len(results) >= max_jobs:
            break
        summary = run_one()
        if summary is None:
            break
        results.append(summary)
    return results
