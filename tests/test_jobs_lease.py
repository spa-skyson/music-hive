"""#40: lease/heartbeat/reaper для очереди job'ов + чекпоинт full_rescan.

Сценарии общие для очереди (PG — фикстура pg_env из conftest). Kill-симуляция:
worker=1 делает обработку детерминированной (FIFO-порядок completions),
«смерть» — исключение из upsert_track на третьем файле, job остаётся running
(без fail_job, как при SIGKILL), затем reaper → pending → возобновление.
"""

from __future__ import annotations

import os
import time
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

from music_hive.db import counts
from music_hive.db.schema import connect
from music_hive.jobs.queue import (
    claim_next,
    enqueue_job,
    get_job,
    heartbeat_job,
    reap_stale_jobs,
)
from music_hive.jobs.runner import process_job, run_one
from music_hive.scanner import pipeline, scan_library

# ------------------------------------------------------------------ helpers


def _make_wav(path: Path, seconds: float = 1.5, freq: float = 440.0) -> None:
    sr = 22050
    t = np.linspace(0, seconds, int(sr * seconds), endpoint=False)
    y = (0.2 * np.sin(2 * np.pi * freq * t)).astype(np.float32)
    sf.write(path, y, sr)


def _backdate(job_id: int, seconds: float) -> None:
    """claimed_at = now - seconds (часы PG)."""
    with connect() as conn:
        conn.execute(
            "UPDATE jobs SET claimed_at = now() - (? * INTERVAL '1 second') "
            "WHERE id = ?",
            (seconds, job_id),
        )


def _set_attempts(job_id: int, n: int) -> None:
    with connect() as conn:
        conn.execute("UPDATE jobs SET attempts = ? WHERE id = ?", (n, job_id))


# ------------------------------------------------------- сценарии lease (#40)


def _scenario_claim_sets_fresh_lease() -> None:
    enqueue_job("clusters", {"probe": 1})
    job = claim_next()
    assert job is not None
    assert job["status"] == "running"
    assert job["claimed_at"] is not None
    assert int(job["attempts"]) == 0
    assert claim_next() is None  # очередь пуста


def _scenario_heartbeat_keeps_job_alive() -> None:
    job = enqueue_job("clusters", {"probe": 2})
    claimed = claim_next()
    assert claimed is not None
    jid = int(claimed["id"])
    _backdate(jid, 10 * 300)  # аренда давно протухла…
    heartbeat_job(jid)  # …но воркер жив и продлил её
    assert reap_stale_jobs() == []
    fresh = get_job(jid)
    assert fresh is not None and fresh["status"] == "running"
    assert int(fresh["attempts"]) == 0
    assert job is not None


def _scenario_reaper_requeues_expired() -> None:
    enqueue_job("clusters", {"probe": 3})
    claimed = claim_next()
    assert claimed is not None
    jid = int(claimed["id"])
    _backdate(jid, 2 * 300)
    reaped = reap_stale_jobs()
    assert [(r["id"], r["status"], int(r["attempts"])) for r in reaped] == [
        (jid, "pending", 1)
    ]
    after = get_job(jid)
    assert after is not None
    assert after["status"] == "pending"
    assert int(after["attempts"]) == 1


def _scenario_reaper_poisons_after_max_attempts() -> None:
    enqueue_job("clusters", {"probe": 4})
    claimed = claim_next()
    assert claimed is not None
    jid = int(claimed["id"])
    _backdate(jid, 2 * 300)
    _set_attempts(jid, 2)  # аренда уже истекала дважды
    reaped = reap_stale_jobs()
    assert [(r["id"], r["status"]) for r in reaped] == [(jid, "failed")]
    after = get_job(jid)
    assert after is not None
    assert after["status"] == "failed"
    assert after["error"] == "lease expired 3 times"
    assert int(after["attempts"]) == 3


def test_claim_sets_fresh_lease(pg_env):
    _scenario_claim_sets_fresh_lease()


def test_heartbeat_keeps_job_alive(pg_env):
    _scenario_heartbeat_keeps_job_alive()


def test_reaper_requeues_expired(pg_env):
    _scenario_reaper_requeues_expired()


def test_reaper_poisons_after_max_attempts(pg_env):
    _scenario_reaper_poisons_after_max_attempts()


# ------------------------------------------------- сценарии чекпоинта (#40 B)


def _kill_sim(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """Прерывание посреди scan-job'ы → reaper-reset → возобновление с чекпоинта."""
    lib = tmp_path / "lib"
    lib.mkdir(parents=True, exist_ok=True)
    files = [lib / f"{i}_song.wav" for i in range(6)]
    for i, p in enumerate(files):
        _make_wav(p, freq=300.0 + 100.0 * i)

    monkeypatch.setattr(pipeline, "_CHECKPOINT_EVERY", 1)
    real_upsert = pipeline.upsert_track
    kill_switch = [True]  # крашимся один раз — при возобновлении файл уже не падает

    def boom(payload: dict) -> int:
        if kill_switch[0] and payload["path"] == str(files[2].resolve()):
            kill_switch[0] = False
            raise RuntimeError("worker killed mid-scan")
        return real_upsert(payload)

    monkeypatch.setattr(pipeline, "upsert_track", boom)

    # workers=1: completions строго в порядке sorted walk — счётчики и
    # last_path в момент «смерти» детерминированы.
    enqueue_job("scan", {"library": str(lib), "workers": 1})
    claimed = claim_next()
    assert claimed is not None
    jid = int(claimed["id"])

    # «Смерть» воркера: исключение уходит наверх, fail_job НЕ вызывается —
    # job остаётся running (зомби), чекпоинт остаётся в scan_state.
    with pytest.raises(RuntimeError):
        process_job(claimed)

    cp = pipeline._checkpoint_load()
    assert cp is not None
    assert cp["last_path"] == str(files[1].resolve())
    assert int(cp["counts"]["upserted"]) == 2

    # reaper поднимает зомби: pending + attempts++
    _backdate(jid, 10_000)
    reaped = reap_stale_jobs()
    assert [(r["id"], r["status"], int(r["attempts"])) for r in reaped] == [
        (jid, "pending", 1)
    ]

    # Меняем mtime файла из префикса: если бы возобновление шло по mtime,
    # файл был бы переанализирован; чекпоинт обязан его пропустить.
    os.utime(files[0], (time.time() + 500.0,) * 2)

    processed: list[str] = []
    real_proc = pipeline._process_one

    def counting(path: Path, *, extract_audio: bool):
        processed.append(str(path))
        return real_proc(path, extract_audio=extract_audio)

    monkeypatch.setattr(pipeline, "_process_one", counting)

    summary = run_one()
    assert summary is not None and summary["status"] == "done"
    # В БД только префикс до краха (upsert идёт в главном потоке), поэтому
    # возобновление анализирует ровно файлы после last_path (wav0 пропущен
    # чекпоинтом, несмотря на изменённый mtime).
    assert processed == [str(p.resolve()) for p in files[2:]]
    assert int(summary["result"]["upserted"]) == 6  # 2 из чекпоинта + 4 докрученных
    assert counts()["tracks_total"] == 6  # полный охват

    job = get_job(jid)
    assert job is not None
    assert job["status"] == "done"
    assert int(job["attempts"]) == 1  # claim не инкрементирует — только reaper
    assert pipeline._checkpoint_load() is None  # по завершении чекпоинт удалён


def test_checkpoint_resume_after_kill(pg_env, tmp_path, monkeypatch):
    _kill_sim(tmp_path, monkeypatch)


def test_checkpoint_disabled_with_limit(pg_env, tmp_path):
    lib = tmp_path / "lib2"
    lib.mkdir()
    _make_wav(lib / "a.wav")
    _make_wav(lib / "b.wav")
    result = scan_library(lib, extract_audio=False, limit=1, checkpoint=True)
    assert result.upserted == 1
    # Частичные выборки не чекпоинтятся — иначе возобновление исказит охват.
    assert pipeline._checkpoint_load() is None


def test_unchanged_files_skip_heavy_analysis(pg_env, tmp_path, monkeypatch):
    """B.1: mtime+size short-circuit — тяжёлое аудио не считается повторно."""
    lib = tmp_path / "lib3"
    lib.mkdir()
    _make_wav(lib / "a.wav", freq=440)
    _make_wav(lib / "b.wav", freq=880)
    first = scan_library(lib, extract_audio=True, workers=2)
    assert first.upserted == 2

    def _must_not_run(*args: object, **kwargs: object) -> float:
        raise AssertionError("heavy LUFS analysis must be skipped for unchanged files")

    monkeypatch.setattr(pipeline, "compute_lufs", _must_not_run)
    again = scan_library(lib, extract_audio=True, workers=2)
    assert again.upserted == 0
    assert again.skipped_unchanged == 2
