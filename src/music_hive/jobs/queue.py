"""Очередь job'ов в PostgreSQL (#40 lease; #11 F5 — SQLite-ветки сняты)."""

from __future__ import annotations

import json
from typing import Any

from music_hive.config import get_settings
from music_hive.db.schema import connect, row_to_dict, utcnow


def _job_row(row: Any) -> dict[str, Any] | None:
    d = row_to_dict(row)
    if d is None:
        return None
    if d.get("payload_json"):
        try:
            d["payload"] = json.loads(d["payload_json"])
        except json.JSONDecodeError:
            d["payload"] = None
    else:
        d["payload"] = None
    if d.get("result_json"):
        try:
            d["result"] = json.loads(d["result_json"])
        except json.JSONDecodeError:
            d["result"] = None
    else:
        d["result"] = None
    return d


def enqueue_job(kind: str, payload: dict[str, Any] | None = None) -> dict[str, Any]:
    now = utcnow()
    payload_json = json.dumps(payload or {}, ensure_ascii=False)
    with connect() as conn:
        cur = conn.execute(
            """
            INSERT INTO jobs(kind, status, payload_json, created_at, updated_at)
            VALUES (?, 'pending', ?, ?, ?)
            """,
            (kind, payload_json, now, now),
        )
        job_id = int(cur.lastrowid)
        row = conn.execute("SELECT * FROM jobs WHERE id = ?", (job_id,)).fetchone()
    job = _job_row(row)
    assert job is not None
    return job


def enqueue_job_once(
    kind: str,
    payload: dict[str, Any],
) -> dict[str, Any] | None:
    """Atomically enqueue an exact kind/payload pair unless it already exists."""
    now = utcnow()
    payload_json = json.dumps(payload, ensure_ascii=False, sort_keys=True)
    with connect() as conn:
        existing = conn.execute(
            """
            SELECT * FROM jobs
            WHERE kind = ? AND payload_json = ?
            ORDER BY id DESC LIMIT 1
            """,
            (kind, payload_json),
        ).fetchone()
        if existing is not None:
            return None
        cur = conn.execute(
            """
            INSERT INTO jobs(kind, status, payload_json, created_at, updated_at)
            VALUES (?, 'pending', ?, ?, ?)
            """,
            (kind, payload_json, now, now),
        )
        row = conn.execute(
            "SELECT * FROM jobs WHERE id = ?", (int(cur.lastrowid),)
        ).fetchone()
    job = _job_row(row)
    assert job is not None
    return job


def find_active_job(kind: str) -> dict[str, Any] | None:
    """Последняя job kind в статусе pending/running или None (#57).

    Дешёвый SELECT-зеркало Go PGStore.FindActiveJob: постановщикам на
    стороне воркера (цепочка scan→embed) нужно исключить дубли без HTTP
    к плееру. Статусы двигают claim_next/reaper — как у Go-варианта.
    """
    with connect() as conn:
        row = conn.execute(
            """
            SELECT * FROM jobs
            WHERE kind = ? AND status IN ('pending','running')
            ORDER BY id DESC LIMIT 1
            """,
            (kind,),
        ).fetchone()
    return _job_row(row)


def claim_next() -> dict[str, Any] | None:
    # §5.3: SKIP LOCKED — безопасный claim для будущего второго воркера.
    # #40: attempts инкрементирует только reaper (счётчик истечений
    # аренды), claim лишь ставит fresh claimed_at — иначе повторный счёт.
    with connect() as conn:
        row = conn.execute(
            """
            UPDATE jobs
            SET status = 'running', claimed_at = now(), updated_at = now()
            WHERE id = (
                SELECT id FROM jobs
                WHERE status = 'pending'
                ORDER BY id
                LIMIT 1
                FOR UPDATE SKIP LOCKED
            )
            RETURNING *
            """
        ).fetchone()
    return _job_row(row)


def heartbeat_job(job_id: int) -> None:
    """#40: продлить аренду исполняемой job'ы (claimed_at = now).

    Зовётся фоновым тредом из runner.run_one каждые ~lease/10 секунд:
    reaper трогает только running-job'ы с claimed_at старше lease_sec,
    так что живой воркер свои job'ы не теряет. Часы — now() сервера PG
    (одни часы с claim/reaper).
    """
    with connect() as conn:
        conn.execute(
            """
            UPDATE jobs
            SET claimed_at = now(), updated_at = now()
            WHERE id = ? AND status = 'running'
            """,
            (job_id,),
        )


def reap_stale_jobs(
    *,
    lease_sec: float | None = None,
    max_attempts: int | None = None,
) -> list[dict[str, Any]]:
    """#40 reaper: зомби-job'ы (running с протухшей арендой) → pending/failed.

    Умерший воркер оставляет job в status='running' навсегда. Reaper
    возвращает такие job'ы в pending (attempts+1); когда попыток исчерпаны
    (attempts+1 >= max_attempts) — в failed с ошибкой «lease expired N
    times» («ядовитая» задача). Живые воркеры не затронуты: heartbeat
    обновляет claimed_at каждые ~30с, lease по умолчанию 300с.

    Вызывается в начале каждого poll-цикла воркера. Возвращает список
    затронутых job'ов (для логов/тестов).
    """
    settings = get_settings()
    lease = float(lease_sec if lease_sec is not None else settings.lease_sec)
    max_att = int(
        max_attempts if max_attempts is not None else settings.job_max_attempts
    )
    # Случай «attempts+1 >= max» помечает failed прямо в этом UPDATE —
    # один атомарный statement на оба исхода (гонок со свежим claim нет:
    # фильтр по claimed_at < cutoff). Часы PG — now(), те же, что ставят
    # claim/heartbeat.
    poison_error = "'lease expired ' || (attempts + 1)::text || ' times'"
    sql = f"""
        UPDATE jobs
        SET status = CASE WHEN attempts + 1 >= ? THEN 'failed' ELSE 'pending' END,
            attempts = attempts + 1,
            error = CASE WHEN attempts + 1 >= ?
                         THEN {poison_error}
                         ELSE error END,
            updated_at = now()
        WHERE status = 'running'
          AND claimed_at < now() - (? * INTERVAL '1 second')
        RETURNING id, kind, status, attempts, error
    """
    params: tuple[Any, ...] = (max_att, max_att, lease)
    with connect() as conn:
        rows = conn.execute(sql, params).fetchall()
    return [dict(r) for r in rows]


def finish_job(job_id: int, result: dict[str, Any] | None = None) -> None:
    now = utcnow()
    result_json = json.dumps(result or {}, ensure_ascii=False)
    with connect() as conn:
        conn.execute(
            """
            UPDATE jobs
            SET status = 'done', result_json = ?, error = NULL, updated_at = ?
            WHERE id = ?
            """,
            (result_json, now, job_id),
        )


def fail_job(job_id: int, error: str) -> None:
    now = utcnow()
    with connect() as conn:
        conn.execute(
            """
            UPDATE jobs
            SET status = 'failed', error = ?, updated_at = ?
            WHERE id = ?
            """,
            (error[:4000], now, job_id),
        )


def update_job_progress(job_id: int, progress: dict[str, Any]) -> None:
    """Write live progress into result_json while status=running (pollable via API)."""
    now = utcnow()
    body = {"progress": progress}
    result_json = json.dumps(body, ensure_ascii=False)
    with connect() as conn:
        conn.execute(
            """
            UPDATE jobs
            SET result_json = ?, updated_at = ?
            WHERE id = ? AND status = 'running'
            """,
            (result_json, now, job_id),
        )


def get_job(job_id: int) -> dict[str, Any] | None:
    with connect() as conn:
        row = conn.execute("SELECT * FROM jobs WHERE id = ?", (job_id,)).fetchone()
    return _job_row(row)


def list_recent(limit: int = 30) -> list[dict[str, Any]]:
    with connect() as conn:
        rows = conn.execute(
            "SELECT * FROM jobs ORDER BY id DESC LIMIT ?",
            (max(1, int(limit)),),
        ).fetchall()
    out: list[dict[str, Any]] = []
    for r in rows:
        job = _job_row(r)
        if job is not None:
            out.append(job)
    return out
