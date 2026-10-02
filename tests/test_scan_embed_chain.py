"""#57: быстрое обновление библиотеки — цепочка scan → embed.

«Обновить» из UI ставит scan с payload-маркером chain_embed (плеер,
api/jobs.go); воркер после успешного scan ставит следующую ступень —
инкрементальную embed-джобу. Здесь: цепочка ставится при успехе, не
ставится при падении scan, не дублируется при живой embed-джобе и
проходит полный путь до векторов через мок model-сервера (#31-связка).

PG (pg_env из conftest) — #11 F5.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace

import numpy as np
import pytest
import soundfile as sf

from music_hive.config import get_settings
from music_hive.db.store import pending_count
from music_hive.jobs.queue import (
    enqueue_job,
    fail_job,
    find_active_job,
    finish_job,
    get_job,
    list_recent,
)
from music_hive.jobs.runner import run_one
from music_hive.scanner import pipeline

SERVER_KEY = "test:mock-model"
VEC4 = [0.5, 0.5, 0.5, 0.5]  # L2=1 — проходит валидацию §6.1 без ренорма


def _make_wav(path: Path, seconds: float = 1.0, freq: float = 440.0) -> None:
    sr = 22_050
    t = np.linspace(0, seconds, int(sr * seconds), endpoint=False)
    y = (0.2 * np.sin(2 * np.pi * freq * t)).astype(np.float32)
    sf.write(path, y, sr)


def _make_lib(tmp_path: Path, n: int = 2) -> Path:
    lib = tmp_path / "lib"
    lib.mkdir(parents=True, exist_ok=True)
    for i in range(n):
        # Разные длительность и частота — иначе fingerprint-детектор
        # размечает короткие синусы дубликатами (embed тогда берёт
        # только активные недубликатные треки).
        _make_wav(
            lib / f"track{i}_artist_song{i}.wav",
            seconds=1.0 + i * 0.5,
            freq=280.0 * (2**i),
        )
    return lib


# ------------------------------------------------------------ мок-сервер #32


class _Handler(BaseHTTPRequestHandler):
    """Контракт model-сервера #32: GET /v1/models, POST /v1/embeddings."""

    def log_message(self, *args: object) -> None:  # тише в выводе pytest
        pass

    def _json(self, obj: object, status: int = 200) -> None:
        body = json.dumps(obj).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        if self.path == "/v1/models":
            self._json(
                {
                    "models": [
                        {
                            "name": "clap",
                            "model_key": SERVER_KEY,
                            "dim": 4,
                            "loaded": True,
                        }
                    ]
                }
            )
        else:
            self._json({"error": "not found"}, 404)

    def do_POST(self) -> None:
        _ = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self._json({"model_key": SERVER_KEY, "dim": 4, "embedding": VEC4})


@pytest.fixture
def mock_model_server():
    srv = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=srv.serve_forever, daemon=True)
    thread.start()
    try:
        yield SimpleNamespace(url=f"http://127.0.0.1:{srv.server_address[1]}")
    finally:
        srv.shutdown()
        srv.server_close()


# ------------------------------------------------------------------ сценарии


def test_chain_enqueued_on_scan_success(pg_env, tmp_path: Path):
    """Успешный standalone scan с chain_embed → pending embed-джоба следом."""
    lib = _make_lib(tmp_path)
    enqueue_job("scan", {"library": str(lib), "workers": 1, "chain_embed": True})

    summary = run_one()
    assert summary is not None and summary["kind"] == "scan"
    assert summary["status"] == "done"

    embed_id = summary["result"]["embed_job_id"]
    assert embed_id is not None
    job = get_job(int(embed_id))
    assert job is not None
    assert job["kind"] == "embed"
    assert job["status"] == "pending"
    assert job["payload"] == {"trigger": "scan_chain"}


def test_chain_skipped_on_scan_failure(
    pg_env, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    """Упавший scan эмбеддинги за собой не тянет: embed-джобы нет."""
    lib = _make_lib(tmp_path)

    def boom(payload: dict) -> int:
        raise RuntimeError("scan failed")

    monkeypatch.setattr(pipeline, "upsert_track", boom)
    enqueue_job("scan", {"library": str(lib), "workers": 1, "chain_embed": True})

    summary = run_one()
    assert summary is not None and summary["status"] == "failed"
    assert summary["kind"] == "scan"
    assert all(j["kind"] != "embed" for j in list_recent())


def test_chain_no_duplicate_while_embed_active(pg_env, tmp_path: Path):
    """Живая (pending) embed-джоба → новая не ставится, в результате None."""
    lib = _make_lib(tmp_path, n=1)
    # scan первым — FIFO claim_next заберёт именно его (ORDER BY id).
    enqueue_job("scan", {"library": str(lib), "workers": 1, "chain_embed": True})
    existing = enqueue_job("embed", {"trigger": "manual"})

    summary = run_one()
    assert summary is not None and summary["kind"] == "scan"
    assert summary["status"] == "done"
    assert summary["result"]["embed_job_id"] is None

    embeds = [j for j in list_recent() if j["kind"] == "embed"]
    assert [j["id"] for j in embeds] == [existing["id"]]
    assert get_job(int(existing["id"]))["status"] == "pending"


def test_chain_without_marker_is_plain_scan(pg_env, tmp_path: Path):
    """Без маркера (например, CLI) — scan как раньше, ничего не ставится."""
    lib = _make_lib(tmp_path, n=1)
    enqueue_job("scan", {"library": str(lib), "workers": 1})
    summary = run_one()
    assert summary is not None and summary["status"] == "done"
    assert "embed_job_id" not in summary["result"]
    assert all(j["kind"] != "embed" for j in list_recent())


def test_find_active_job_sees_only_pending_running(pg_env):
    """find_active_job (зеркало Go FindActiveJob): done/failed не активны."""
    assert find_active_job("embed") is None
    job = enqueue_job("embed", {})
    active = find_active_job("embed")
    assert active is not None and int(active["id"]) == int(job["id"])
    finish_job(int(job["id"]), {"computed": 0})
    assert find_active_job("embed") is None

    failed = enqueue_job("embed", {})
    fail_job(int(failed["id"]), "encoder down")
    assert find_active_job("embed") is None


def test_chain_full_path_computes_embeddings(
    pg_env, tmp_path: Path, mock_model_server, monkeypatch: pytest.MonkeyPatch
):
    """Полный путь: scan → embed до готовых векторов (мок-энкодер #31)."""
    monkeypatch.setenv("MUSIC_HIVE_EMBEDDING_MODEL", f"remote:{mock_model_server.url}")
    monkeypatch.setenv("MUSIC_HIVE_REMOTE_TOKEN", "tok")
    get_settings.cache_clear()

    lib = _make_lib(tmp_path)
    enqueue_job("scan", {"library": str(lib), "workers": 1, "chain_embed": True})

    scan = run_one()
    assert scan is not None and scan["kind"] == "scan" and scan["status"] == "done"
    embed_id = scan["result"]["embed_job_id"]
    assert embed_id is not None  # цепочка переживает закрытую вкладку: джоба в БД

    embed = run_one()
    assert embed is not None and embed["kind"] == "embed"
    assert embed["status"] == "done"
    assert embed["result"]["computed"] == 2
    assert int(embed["result"]["total"]) == 2
    # Векторы записаны под ключом мок-модели — недостающих больше нет.
    assert pending_count(SERVER_KEY) == 0
