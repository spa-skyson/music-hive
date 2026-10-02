"""PG-гейтнутые интеграционные тесты фазы F1.4.

Запуск: подними PG (docker-compose.postgres.yml), примени goose-миграции
бинарником player (`music-hive-player migrate`), затем:

    MUSIC_HIVE_TEST_DATABASE_URL=postgres://music_hive:music_hive@127.0.0.1:5545/music_hive?sslmode=disable \
        uv run pytest tests/test_pg_worker.py -q

Без переменной весь модуль пропускается (паттерн проекта). CLAP-модель не
нужна: энкодер замокан (embed_file → детерминированные векторы).
"""

from __future__ import annotations

import os
import secrets
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

import numpy as np
import pytest
import soundfile as sf

PG_URL = os.environ.get("MUSIC_HIVE_TEST_DATABASE_URL", "").strip()

pytestmark = pytest.mark.skipif(
    not PG_URL, reason="MUSIC_HIVE_TEST_DATABASE_URL не задан — PG-тесты пропущены"
)


def _make_wav(path: Path, seconds: float = 1.0, freq: float = 440.0) -> None:
    sr = 22050
    t = np.linspace(0, seconds, int(sr * seconds), endpoint=False)
    y = (0.2 * np.sin(2 * np.pi * freq * t)).astype(np.float32)
    sf.write(path, y, sr)


def _make_flac(
    path: Path,
    *,
    title: str,
    artist: str,
    album: str | None = None,
    albumartist: str | None = None,
    year: int | None = None,
    freq: float = 440.0,
    art: bytes | None = None,
) -> None:
    """FLAC с настоящими тегами (artist/albumartist/album/date)."""
    from mutagen.flac import FLAC, Picture

    sr = 22050
    t = np.linspace(0, 0.5, int(sr * 0.5), endpoint=False)
    y = (0.2 * np.sin(2 * np.pi * freq * t)).astype(np.float32)
    sf.write(path, y, sr, format="FLAC")
    f = FLAC(path)
    f["title"] = title
    f["artist"] = artist
    if album:
        f["album"] = album
    if albumartist:
        f["albumartist"] = albumartist
    if year:
        f["date"] = str(year)
    if art:
        pic = Picture()
        pic.type = 3  # front cover
        pic.mime = "image/jpeg"
        pic.data = art
        f.add_picture(pic)
    f.save()


def _unit_vec(dim: int, seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    v = rng.standard_normal(dim).astype(np.float32)
    return v / np.linalg.norm(v)


def test_pg_full_cycle(pg_env: pytest.FixtureRequest, tmp_path: Path):
    """scan → embed(фейк) → clusters → mix_pack → jobs claim — по контракту F1.4."""
    from music_hive.db import counts, ensure_db, save_embedding
    from music_hive.db.schema import connect
    from music_hive.db.store import (
        active_model,
        emb_table_name,
        get_embedding,
        model_key,
    )
    from music_hive.index import assign_clusters
    from music_hive.index.brute import load_index
    from music_hive.jobs.queue import enqueue_job, get_job
    from music_hive.jobs.runner import run_one
    from music_hive.scanner import scan_library

    lib = tmp_path / "lib"
    lib.mkdir()
    _make_wav(lib / "sine_a.wav", freq=440)
    _make_wav(lib / "sine_b.wav", freq=880)
    _make_wav(lib / "sine_c.wav", freq=1320)

    ensure_db()  # goose-гейт + owner-bootstrap
    result = scan_library(lib, extract_audio=True, workers=2)
    assert result.upserted == 3
    assert result.failed == 0
    st = counts()
    assert st["tracks_total"] == 3
    assert st["features_pending"] == 3
    assert st["embeddings_ready"] == 0

    # фейковый энкодер: детерминированные единичные векторы
    with connect() as conn:
        ids = [
            int(r["id"])
            for r in conn.execute(
                "SELECT id FROM tracks ORDER BY id LIMIT 3"
            ).fetchall()
        ]
    assert len(ids) == 3
    for i, tid in enumerate(ids):
        save_embedding(tid, _unit_vec(8, seed=i))

    # реестр: модель по умолчанию, ровно одна активная, dim из фактического вектора
    with connect() as conn:
        rows = conn.execute(
            "SELECT model_key, dim, is_active FROM embedding_models"
        ).fetchall()
    assert len(rows) == 1
    assert (
        rows[0]["model_key"] == model_key() == "clap:laion/larger_clap_music_and_speech"
    )
    assert int(rows[0]["dim"]) == 8
    assert rows[0]["is_active"] is True
    am = active_model()
    assert am is not None and am["model_key"] == model_key()

    # векторы легли в emb_<slug>, читаются назад
    st = counts()
    assert st["embeddings_ready"] == 3
    assert st["features_ready"] == 3
    table = emb_table_name(model_key())
    assert table == "emb_clap_laion_larger_clap_music_and_speech"
    vec0 = get_embedding(ids[0])
    assert vec0 is not None and vec0.shape == (8,)
    assert np.allclose(vec0, _unit_vec(8, seed=0), atol=1e-5)

    index = load_index()
    assert index.size == 3 and index.dim == 8

    # кластеры пишутся в track_audio_features.cluster_id
    assign_clusters(k=2)
    with connect() as conn:
        clustered = int(
            conn.execute(
                "SELECT COUNT(*) AS n FROM track_audio_features WHERE cluster_id IS NOT NULL"
            ).fetchone()["n"]
        )
    assert clustered == 3

    # полные jobs по очереди: clusters + mix_pack
    j1 = enqueue_job("clusters", {"k": 2})
    j2 = enqueue_job(
        "mix_pack",
        {
            "daily_size": 2,
            "for_you_size": 3,
            "weekday_size": 2,
            "weekly_size": 3,
            "new_size": 2,
        },
    )
    assert j1["status"] == "pending" and j2["status"] == "pending"
    for _ in range(2):
        summary = run_one()
        assert summary is not None and summary["status"] == "done"
    assert get_job(int(j1["id"]))["status"] == "done"
    assert get_job(int(j2["id"]))["status"] == "done"

    from music_hive.brain.store import latest_playlist

    pl = latest_playlist("for_you")
    assert pl is not None and len(pl["tracks"]) >= 1

    # user-scoped записи: история и статистика владельца
    from music_hive.listen.history import bump_rec_stats, record_listen

    hid = record_listen(ids[0], "finish")
    assert hid > 0
    bump_rec_stats(ids[0], shown=1)
    with connect() as conn:
        row = conn.execute(
            "SELECT shown, completed FROM user_track_stats WHERE track_id = %s",
            (ids[0],),
        ).fetchone()
    assert int(row["shown"]) == 1

    # идемпотентность scan: без изменений на диске ничего не перезаписывается
    again = scan_library(lib, extract_audio=True, workers=2)
    assert again.upserted == 0
    assert again.skipped_unchanged == 3


def test_pg_scan_idempotent_and_inactive(pg_env, tmp_path: Path):
    from music_hive.db import counts, ensure_db
    from music_hive.scanner import scan_library

    lib = tmp_path / "lib"
    lib.mkdir()
    _make_wav(lib / "one.wav", freq=300)
    _make_wav(lib / "two.wav", freq=500)

    ensure_db()
    scan_library(lib, extract_audio=False, workers=1)
    assert counts()["tracks_total"] == 2

    second = scan_library(lib, extract_audio=False, workers=1)
    assert second.upserted == 0
    assert second.skipped_unchanged == 2

    (lib / "two.wav").unlink()
    third = scan_library(lib, extract_audio=False, workers=1)
    assert third.inactivated == 1
    st = counts()
    assert st["tracks_total"] == 2 and st["tracks_active"] == 1


def test_pg_model_registry_invariants(pg_env, tmp_path: Path):
    from music_hive.db import ensure_db, save_embedding, upsert_track

    ensure_db()
    tid = upsert_track(
        {
            "path": str(tmp_path / "x.wav"),
            "file_md5": "0" * 32,
            "file_mtime": 1.0,
            "file_size": 1,
            "title": "x",
            "artist": "y",
            "album": "z",
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
    )
    save_embedding(tid, _unit_vec(8, seed=1))
    save_embedding(tid, _unit_vec(8, seed=2))  # тот же dim — идемпотентно

    from music_hive.db.schema import connect
    from music_hive.db.store import model_key

    with connect() as conn:
        rows = conn.execute(
            "SELECT model_key, dim, is_active FROM embedding_models"
        ).fetchall()
    assert len(rows) == 1
    assert rows[0]["model_key"] == model_key()
    assert rows[0]["is_active"] is True

    # dim-mismatch (конфиг/реестр расходятся) — отказ, а не тихая порча данных
    with pytest.raises(ValueError, match="dim"):
        save_embedding(tid, _unit_vec(16, seed=3))

    # не-единичный вектор ренормализуется молча
    tid2_vec = _unit_vec(8, seed=4) * 3.0
    save_embedding(tid, tid2_vec)
    from music_hive.db.store import get_embedding

    got = get_embedding(tid)
    assert got is not None
    assert abs(float(np.linalg.norm(got)) - 1.0) < 1e-4


def test_pg_jobs_claim_skip_locked(pg_env):
    from music_hive.db import ensure_db
    from music_hive.jobs.queue import claim_next, enqueue_job, get_job

    ensure_db()
    j1 = enqueue_job("clusters", {"probe": 1})
    j2 = enqueue_job("clusters", {"probe": 2})
    c1 = claim_next()
    c2 = claim_next()
    assert c1 is not None and c2 is not None
    assert c1["id"] != c2["id"]
    assert {c1["id"], c2["id"]} == {j1["id"], j2["id"]}
    for c in (c1, c2):
        assert c["status"] == "running"
        # #40: attempts считает истечения аренды (инкрементирует только
        # reaper), claim лишь ставит fresh claimed_at.
        assert int(c["attempts"]) == 0
        assert c["claimed_at"] is not None
    assert claim_next() is None  # очередь пуста
    # честность статусов в таблице
    assert get_job(int(j1["id"]))["status"] == "running"


def test_pg_migrate_gate(monkeypatch: pytest.MonkeyPatch):
    """Без goose-миграций ensure_db падает сразу с подсказкой про migrate."""
    psycopg = pytest.importorskip("psycopg")
    from music_hive.config import get_settings
    from music_hive.db import backend, ensure_db

    parts = urlsplit(PG_URL)
    admin_url = urlunsplit(parts._replace(path="/postgres"))
    dbname = f"mh_gate_{secrets.token_hex(4)}"
    admin = psycopg.connect(admin_url)
    admin.autocommit = True
    try:
        admin.execute(f'CREATE DATABASE "{dbname}"')
    finally:
        admin.close()
    try:
        fresh_url = urlunsplit(parts._replace(path=f"/{dbname}"))
        monkeypatch.setenv("MUSIC_HIVE_DATABASE_URL", fresh_url)
        get_settings.cache_clear()
        backend.reset_pool()
        with pytest.raises(RuntimeError, match="music-hive-player migrate"):
            ensure_db()
    finally:
        get_settings.cache_clear()
        backend.reset_pool()
        admin = psycopg.connect(admin_url)
        admin.autocommit = True
        try:
            admin.execute(f'DROP DATABASE IF EXISTS "{dbname}" WITH (FORCE)')
        finally:
            admin.close()


def test_pg_embed_job_with_mocked_encoder(pg_env, tmp_path: Path, monkeypatch):
    """job embed без CLAP-модели: энкодер замокан через фабрику."""
    from music_hive.db import counts, ensure_db
    from music_hive.embed import pipeline as embed_pipeline
    from music_hive.jobs.queue import enqueue_job
    from music_hive.jobs.runner import run_one
    from music_hive.scanner import scan_library

    lib = tmp_path / "lib"
    lib.mkdir()
    _make_wav(lib / "m1.wav", freq=410)
    _make_wav(lib / "m2.wav", freq=830)

    ensure_db()
    scan_library(lib, extract_audio=False, workers=1)
    assert counts()["tracks_total"] == 2

    class FakeEncoder:
        """Детерминированный «выход модели» из контента первого сегмента."""

        sample_rate = 22_050

        def model_key(self):
            from music_hive.db.store import model_key

            return model_key()

        def encode(self, segments):
            y = segments[0]
            v = np.zeros(8, dtype=np.float32)
            v[: min(8, y.size)] = y[:8]
            return v / np.linalg.norm(v)

    monkeypatch.setattr(embed_pipeline, "get_encoder", lambda name=None: FakeEncoder())

    enqueue_job("embed", {})
    summary = run_one()
    assert summary is not None and summary["status"] == "done", summary
    assert summary["result"]["computed"] == 2
    assert counts()["embeddings_ready"] == 2


# ----------------------------------------------------------- F1.2: artists/albums


def _one(conn, sql: str, params: tuple = ()) -> object:
    row = conn.execute(sql, params).fetchone()
    return None if row is None else list(row.values())[0]


def test_pg_artists_albums_upserts(pg_env):
    """Схлопывание: artist — name_norm, album — (artist_id, title_norm); год дозаполняется."""
    from music_hive.db import ensure_db, upsert_album, upsert_artist
    from music_hive.db.schema import connect

    ensure_db()
    a1 = upsert_artist("The Beatles")
    assert upsert_artist("  the BEATLES ") == a1
    a2 = upsert_artist("Queen")
    assert a2 != a1

    al = upsert_album(a1, "Greatest Hits", year=2001)
    assert upsert_album(a1, "greatest  HITS") == al
    assert upsert_album(a2, "Greatest Hits") != al  # другой артист → другой альбом

    with connect() as conn:
        assert _one(conn, "SELECT COUNT(*) FROM artists") == 2
        assert _one(conn, "SELECT COUNT(*) FROM albums") == 2
        # год не в ключе: NULL дозаполняется, существующий не перезатирается
        al2 = upsert_album(a2, "Later")
        assert upsert_album(a2, "Later", year=2010) == al2
        assert _one(conn, "SELECT year FROM albums WHERE id = %s", (al2,)) == 2010
        assert upsert_album(a2, "Later", year=1999) == al2
        assert _one(conn, "SELECT year FROM albums WHERE id = %s", (al2,)) == 2010
        # denorm-строки треков не тронуты (источник истины — FK)
        assert _one(conn, "SELECT name_norm FROM artists WHERE id = %s", (a1,)) == "the beatles"


def test_pg_artists_backfill(pg_env, tmp_path: Path):
    """Backfill по строковым полям: создаёт сущности, ставит FK, идемпотентен."""
    from music_hive.db import artists_backfill, ensure_db, upsert_track
    from music_hive.db.schema import connect

    ensure_db()
    base = {
        "file_mtime": 1.0,
        "file_size": 1,
        "title": "t",
        "track_number": None,
        "duration": 1.0,
        "bitrate": None,
        "sample_rate": None,
        "channels": None,
        "fingerprint": None,
        "lufs": None,
        "artwork_path": None,
        "year": None,
    }
    upsert_track({**base, "path": str(tmp_path / "1.flac"), "file_md5": "1", "artist": "Alpha", "album": "Album One"})
    upsert_track({**base, "path": str(tmp_path / "2.flac"), "file_md5": "2", "artist": "alpha", "album": "album one"})
    upsert_track({**base, "path": str(tmp_path / "3.flac"), "file_md5": "3", "artist": "Beta", "album": "Album Two"})

    first = artists_backfill()
    assert first["artists_created"] == 2
    assert first["albums_created"] == 2
    assert first["tracks_linked"] == 3
    assert first["tracks_left"] == 0

    with connect() as conn:
        unlinked = _one(
            conn,
            "SELECT COUNT(*) FROM tracks WHERE artist_id IS NULL OR album_id IS NULL",
        )
        wrong = _one(
            conn,
            """
            SELECT COUNT(*) FROM tracks t JOIN albums a ON a.id = t.album_id
            WHERE t.album_id IS NOT NULL AND t.artist_id != a.artist_id
            """,
        )
    assert unlinked == 0
    assert wrong == 0  # трек-артист совпадает с артистом альбома (строк не было разных)

    second = artists_backfill()
    assert second["tracks_to_link"] == 0
    assert second["artists_created"] == 0 and second["albums_created"] == 0


def test_pg_scan_artists_albums_e2e(pg_env, tmp_path: Path):
    """E2E-мини: 3 файла (2 одного артиста, 1 другого) → artists=2; повтор — идемпотентно."""
    from music_hive.db import ensure_db
    from music_hive.db.schema import connect
    from music_hive.scanner import scan_library

    lib = tmp_path / "lib"
    lib.mkdir()
    _make_flac(lib / "a1.flac", title="Song A1", artist="Alpha", album="Album One")
    _make_flac(lib / "a2.flac", title="Song A2", artist="Alpha", album="Album One", albumartist="alpha", year=2005)
    _make_flac(lib / "b1.flac", title="Song B1", artist="Beta", album="Album Two")

    ensure_db()
    result = scan_library(lib, extract_audio=False, workers=2)
    assert result.upserted == 3 and result.failed == 0

    with connect() as conn:
        assert _one(conn, "SELECT COUNT(*) FROM artists") == 2
        assert _one(conn, "SELECT COUNT(*) FROM albums") == 2
        alpha = _one(conn, "SELECT id FROM artists WHERE name_norm = 'alpha'")
        beta = _one(conn, "SELECT id FROM artists WHERE name_norm = 'beta'")
        assert _one(conn, "SELECT artist_id FROM albums WHERE title_norm = 'album one'") == alpha
        assert _one(conn, "SELECT artist_id FROM albums WHERE title_norm = 'album two'") == beta
        assert _one(conn, "SELECT year FROM albums WHERE title_norm = 'album one'") == 2005
        unlinked = _one(
            conn,
            "SELECT COUNT(*) FROM tracks WHERE album_id IS NULL OR artist_id IS NULL",
        )
    assert unlinked == 0

    again = scan_library(lib, extract_audio=False, workers=2)
    assert again.upserted == 0 and again.skipped_unchanged == 3
    with connect() as conn:
        assert _one(conn, "SELECT COUNT(*) FROM artists") == 2
        assert _one(conn, "SELECT COUNT(*) FROM albums") == 2


def test_pg_album_cover_first_artwork_wins(pg_env, tmp_path):
    """#34 (PG): albums.cover_track_id — первый трек с артом при scan."""
    from music_hive.db import ensure_db
    from music_hive.db.schema import connect
    from music_hive.scanner import scan_library

    lib = tmp_path / "lib"
    lib.mkdir()
    art_a = b"\xff\xd8\xff" + b"A" * 32
    art_b = b"\xff\xd8\xff" + b"B" * 32

    ensure_db()
    _make_flac(lib / "01.flac", title="One", artist="Alpha", album="Hits", art=art_a)
    r1 = scan_library(lib, extract_audio=False, workers=1)
    assert r1.upserted == 1 and r1.failed == 0

    with connect() as conn:
        track1 = _one(conn, "SELECT id FROM tracks WHERE path LIKE '%01.flac'")
        assert _one(conn, "SELECT artwork_path FROM tracks WHERE id = %s", (track1,)) is not None
        assert (
            _one(conn, "SELECT cover_track_id FROM albums WHERE title_norm = 'hits'")
            == track1
        )

        # второй трек альбома с другим артом — обложка НЕ меняется
        _make_flac(lib / "02.flac", title="Two", artist="Alpha", album="Hits", art=art_b)
        scan_library(lib, extract_audio=False, workers=1)
        assert (
            _one(conn, "SELECT cover_track_id FROM albums WHERE title_norm = 'hits'")
            == track1
        )

        # трек без арта в альбоме без покрытия — cover остаётся NULL
        _make_flac(lib / "03.flac", title="Three", artist="Alpha", album="B-Sides")
        scan_library(lib, extract_audio=False, workers=1)
        assert (
            _one(conn, "SELECT cover_track_id FROM albums WHERE title_norm = 'b-sides'")
            is None
        )
