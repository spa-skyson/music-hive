"""Общие фикстуры тестов.

pg_env — единственная PG-фикстура проекта (раньше дублировалась в
test_pg_worker.py и test_migrate_sqlite.py, test_pg_users брала её
импортом из test_pg_worker).

Приводит общую тестовую БД к «свежей схеме»:

- TRUNCATE бизнес-таблиц (+ реестр embedding_models); users не трогаем —
  владелец и тестовые пользователи живут между прогонами (F1-базлайн),
  goose_db_version не трогаем — тесты ждут мигрированную БД (goose-гейт
  ensure_db);
- DROP emb_<slug> CASCADE — таблицы создаёт воркер при первом эмбеддинге
  (единственное санкционированное DDL, §5.2), миграция о них не знает;
  без этого `goose down` в go-тестах не может дропнуть tracks из-за FK
  emb_* → tracks (SQLSTATE 2BP01);
- реестр embedding_models восстанавливается сам при первой записи вектора
  (_ensure_model в db.store).

Запуск: подними PG (docker-compose.postgres.yml), примени goose-миграции
бинарником player (`music-hive-player migrate`), затем:

    MUSIC_HIVE_TEST_DATABASE_URL=postgres://music_hive:music_hive@127.0.0.1:5545/music_hive?sslmode=disable \
        uv run pytest tests/ -q

Без переменной PG-тесты пропускаются (module-level skipif).
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

PG_URL = os.environ.get("MUSIC_HIVE_TEST_DATABASE_URL", "").strip()

# embedding_models — в одном TRUNCATE с user_taste_profiles /
# user_feature_weights (FK → embedding_models). users и goose_db_version
# вне списка (см. докстринг модуля).
_TRUNCATE_TABLES = [
    "tracks",
    "artists",
    "albums",
    "genres",
    "track_genres",
    "track_audio_features",
    "embedding_models",
    "jobs",
    "playlists",
    "playlist_tracks",
    "discover_tips",
    "scan_state",
    "lyrics",
    "listening_history",
    "user_track_stats",
    "user_listen_later",
    "user_favorites",
    "recommendation_impressions",
    "transitions",
    "play_sessions",
    "radio_shares",
    "user_feature_weights",
    "user_taste_profiles",
]


def _reset_shared_db() -> None:
    """Чистит общую тестовую БД: данные, реестр моделей, воркер-таблицы emb_<slug>.

    Зовётся до и после теста: артефакты последнего теста тоже не должны
    переживать прогон (иначе `goose down` в go-тестах падает на FK
    emb_* → tracks, SQLSTATE 2BP01).
    """
    from music_hive.config import get_settings
    from music_hive.db import backend
    from music_hive.db.schema import connect

    get_settings.cache_clear()
    backend.reset_pool()
    backend.reset_owner_cache()
    with connect() as conn:
        emb_tables = [
            str(r["table_name"])
            for r in conn.execute(
                r"""
                SELECT table_name FROM information_schema.tables
                WHERE table_schema = 'public' AND table_name LIKE 'emb\_%'
                """
            ).fetchall()
        ]
        conn.execute(
            f"TRUNCATE TABLE {', '.join([*_TRUNCATE_TABLES, *emb_tables])} CASCADE"
        )
        for table in emb_tables:
            conn.execute(f"DROP TABLE IF EXISTS {table} CASCADE")


@pytest.fixture
def pg_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path):
    """Переключает воркер в PG-режим на время теста, изолируя пути."""
    if not PG_URL:
        pytest.skip("MUSIC_HIVE_TEST_DATABASE_URL не задан — PG-тесты пропущены")
    monkeypatch.setenv("MUSIC_HIVE_DATABASE_URL", PG_URL)
    monkeypatch.setenv("MUSIC_HIVE_LIBRARY", str(tmp_path / "lib"))
    monkeypatch.setenv("MUSIC_HIVE_DATA_DIR", str(tmp_path / "data"))
    monkeypatch.setenv(
        "MUSIC_HIVE_ARTWORK_CACHE", str(tmp_path / "data" / "cache" / "artwork")
    )
    _reset_shared_db()
    yield
    _reset_shared_db()
