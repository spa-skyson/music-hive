"""F4.1 (PG): смена модели — реестр, policy re-embed, изолированные emb_*.

Запуск как у test_pg_worker: подними PG, прогони migrate, затем
MUSIC_HIVE_TEST_DATABASE_URL=... uv run pytest tests/test_pg_model_switch.py -q.
"""

from __future__ import annotations

import os
from pathlib import Path
from typing import Any

import numpy as np
import pytest

PG_URL = os.environ.get("MUSIC_HIVE_TEST_DATABASE_URL", "").strip()

pytestmark = pytest.mark.skipif(
    not PG_URL, reason="MUSIC_HIVE_TEST_DATABASE_URL не задан — PG-тесты пропущены"
)

MODEL_A_REPO = "laion/larger_clap_music_and_speech"
MODEL_B_REPO = "custom/clap_b"
KEY_A = f"clap:{MODEL_A_REPO}"
KEY_B = f"clap:{MODEL_B_REPO}"


def _unit(dim: int, seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    v = rng.standard_normal(dim).astype(np.float32)
    return v / np.linalg.norm(v)


def _upsert_track(tmp_path: Path, name: str, md5: str) -> int:
    from music_hive.db import upsert_track

    base: dict[str, Any] = {
        "path": str(tmp_path / name),
        "file_md5": md5,
        "file_mtime": 1.0,
        "file_size": 1,
        "title": name,
        "artist": "a",
        "album": "b",
        "year": None,
        "track_number": None,
        "duration": 1.0,
        "bitrate": None,
        "sample_rate": None,
        "channels": None,
        "fingerprint": None,
        "lufs": None,
        "artwork_path": None,
    }
    return upsert_track(base)


def _count(conn: Any, sql: str, params: tuple = ()) -> int:
    return int(conn.execute(sql, params).fetchone()["n"])


def test_pg_model_switch_isolated_tables(pg_env, tmp_path: Path, monkeypatch):
    """Активна A → векторы A; смена на B → emb_B создана, emb_A нетронута, активна одна (B)."""
    from music_hive.config import get_settings
    from music_hive.db import ensure_db, save_embedding
    from music_hive.db.schema import connect
    from music_hive.db.store import (
        activate_model,
        active_model,
        emb_table_name,
        get_embedding,
        list_tracks_needing_embedding,
        model_key,
    )

    ensure_db()
    tid1 = _upsert_track(tmp_path, "x.wav", "1" * 32)
    tid2 = _upsert_track(tmp_path, "y.wav", "2" * 32)

    # --- фаза A: дефолтная модель, векторы ложатся в emb_A, реестр — одна активная
    assert model_key() == KEY_A  # фабрика по MUSIC_HIVE_EMBEDDING_MODEL=clap
    for i, tid in enumerate((tid1, tid2)):
        save_embedding(tid, _unit(8, seed=i))
    am = active_model()
    assert am is not None and am["model_key"] == KEY_A and int(am["dim"]) == 8
    table_a = emb_table_name(KEY_A)
    with connect() as conn:
        assert _count(conn, f"SELECT COUNT(*) AS n FROM {table_a} WHERE status='ready'") == 2

    # --- смена модели: конфиг → фабрика → ключ (без правки кода)
    monkeypatch.setenv("MUSIC_HIVE_CLAP_MODEL", MODEL_B_REPO)
    get_settings.cache_clear()
    assert model_key() == KEY_B

    # policy re-embed: треки готовы в emb_A, но для B — не готовы (все)
    pending = list_tracks_needing_embedding()
    assert {t["id"] for t in pending} == {tid1, tid2}

    # job embed по новой модели считает недостающее
    for i, tid in enumerate((tid1, tid2)):
        save_embedding(tid, _unit(8, seed=100 + i))
    table_b = emb_table_name(KEY_B)
    assert table_b == "emb_clap_custom_clap_b"
    with connect() as conn:
        # emb_B создана и заполнена; emb_A нетронута (изолированные таблицы)
        assert _count(conn, f"SELECT COUNT(*) AS n FROM {table_b}") == 2
        assert _count(conn, f"SELECT COUNT(*) AS n FROM {table_a} WHERE status='ready'") == 2
        # реестр знает обе модели, но первая запись B НЕ флипает активность
        assert _count(conn, "SELECT COUNT(*) AS n FROM embedding_models") == 2
        assert _count(conn, "SELECT COUNT(*) AS n FROM embedding_models WHERE is_active") == 1
    assert active_model()["model_key"] == KEY_A

    # --- активация B: ровно одна активная, чтение идёт из emb_B
    activate_model(KEY_B)
    with connect() as conn:
        active_rows = conn.execute(
            "SELECT model_key FROM embedding_models WHERE is_active"
        ).fetchall()
    assert len(active_rows) == 1 and active_rows[0]["model_key"] == KEY_B
    assert active_model()["model_key"] == KEY_B

    got = get_embedding(tid1)
    assert got is not None and np.allclose(got, _unit(8, seed=100), atol=1e-5)

    # для B всё готово → job embed больше не находит работы
    assert list_tracks_needing_embedding() == []

    # emb_A жива нетронутой (изолирована, ничего не удаляли/не прятали)
    with connect() as conn:
        assert _count(conn, f"SELECT COUNT(*) AS n FROM {table_a} WHERE status='ready'") == 2


def test_pg_activate_unknown_model_rolls_back(pg_env, tmp_path: Path):
    """Активация незарегистрированной модели — отказ; прежняя активная остаётся."""
    from music_hive.db import ensure_db, save_embedding
    from music_hive.db.store import activate_model, active_model

    ensure_db()
    tid = _upsert_track(tmp_path, "x.wav", "3" * 32)
    save_embedding(tid, _unit(8, seed=1))
    before = active_model()
    assert before is not None

    with pytest.raises(ValueError, match="не зарегистрирована"):
        activate_model("clap:never/embedded")

    after = active_model()
    assert after is not None and after["model_key"] == before["model_key"]
