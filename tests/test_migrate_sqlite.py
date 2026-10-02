"""F1.5 migrate-sqlite: план на SQLite-фикстуре (без PG) + полный цикл за PG-гейтом.

PG-часть: подними PG (docker-compose.postgres.yml), примени goose-миграции
(``music-hive-player migrate``), затем:

    MUSIC_HIVE_TEST_DATABASE_URL=postgres://music_hive:music_hive@127.0.0.1:5548/music_hive?sslmode=disable \
        uv run pytest tests/test_migrate_sqlite.py -q
"""

from __future__ import annotations

import json
import os
import sqlite3
from datetime import UTC, datetime, timedelta
from pathlib import Path

import numpy as np
import pytest

PG_URL = os.environ.get("MUSIC_HIVE_TEST_DATABASE_URL", "").strip()

pg_gate = pytest.mark.skipif(
    not PG_URL, reason="MUSIC_HIVE_TEST_DATABASE_URL не задан — PG-тесты пропущены"
)


def _unit_vec(dim: int, seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    v = rng.standard_normal(dim).astype(np.float32)
    return v / np.linalg.norm(v)


# Легаси-SQLite DDL (историческая db.schema.SCHEMA, снята в #11 F5):
# нужна только здесь — создать входной файл для импортёра migrate-sqlite.
_LEGACY_SCHEMA = """
-- F1.2 (временно в SQLite-легаси до F5; контракт — player/migrations/00001_init.sql):
-- артисты/альбомы как сущности, треки линкуются artist_id/album_artist_id/album_id.
CREATE TABLE IF NOT EXISTS artists (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    name_norm  TEXT NOT NULL UNIQUE,
    sort_name  TEXT NOT NULL DEFAULT '',
    mbid       TEXT,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS albums (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    artist_id      INTEGER NOT NULL REFERENCES artists(id),
    title          TEXT NOT NULL,
    title_norm     TEXT NOT NULL,
    year           INTEGER,
    genre          TEXT NOT NULL DEFAULT '',
    cover_track_id INTEGER,
    mbid           TEXT,
    created_at     TEXT NOT NULL,
    UNIQUE (artist_id, title_norm)
);

CREATE TABLE IF NOT EXISTS tracks (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    path            TEXT NOT NULL UNIQUE,
    file_md5        TEXT,
    file_mtime      REAL,
    file_size       INTEGER,
    title           TEXT,
    artist          TEXT,
    album           TEXT,
    artist_id       INTEGER REFERENCES artists(id),
    album_artist_id INTEGER REFERENCES artists(id),
    album_id        INTEGER REFERENCES albums(id),
    year            INTEGER,
    track_number    INTEGER,
    duration        REAL,
    bitrate         INTEGER,
    sample_rate     INTEGER,
    channels        INTEGER,
    fingerprint     TEXT,
    lufs            REAL,
    is_duplicate_of INTEGER REFERENCES tracks(id),
    is_active       INTEGER NOT NULL DEFAULT 1,
    artwork_path    TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS genres (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS track_genres (
    track_id INTEGER NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    genre_id INTEGER NOT NULL REFERENCES genres(id) ON DELETE CASCADE,
    PRIMARY KEY (track_id, genre_id)
);

CREATE TABLE IF NOT EXISTS features (
    track_id    INTEGER PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    embedding   BLOB,
    embedding_dim INTEGER,
    bpm         REAL,
    key_name    TEXT,
    mode        TEXT,
    lufs        REAL,
    cluster_id  INTEGER,
    status      TEXT NOT NULL DEFAULT 'pending',
    -- pending | ready | failed | retry
    error       TEXT,
    computed_at TEXT
);

CREATE TABLE IF NOT EXISTS listening_history (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    track_id     INTEGER NOT NULL REFERENCES tracks(id),
    ts           TEXT NOT NULL,
    source       TEXT,
    action       TEXT NOT NULL,
    -- start|finish|skip|like|dislike|progress|track_end
    daypart      TEXT,
    weekday      INTEGER,
    position_sec REAL,
    duration_sec REAL,
    listened_sec REAL,
    session_id   TEXT,
    reason       TEXT
    -- completed|skipped|next|NULL
);

CREATE TABLE IF NOT EXISTS rec_stats (
    track_id       INTEGER PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    shown          INTEGER NOT NULL DEFAULT 0,
    skipped_early  INTEGER NOT NULL DEFAULT 0,
    completed      INTEGER NOT NULL DEFAULT 0,
    updated_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS recommendation_impressions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT NOT NULL,
    track_id        INTEGER NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    position        INTEGER NOT NULL,
    score           REAL NOT NULL DEFAULT 0,
    cosine_taste    REAL NOT NULL DEFAULT 0,
    cosine_current  REAL NOT NULL DEFAULT 0,
    explore         INTEGER NOT NULL DEFAULT 0,
    new_boost       INTEGER NOT NULL DEFAULT 0,
    maturity        TEXT NOT NULL DEFAULT '',
    mode            TEXT NOT NULL DEFAULT '',
    shown_at        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS transitions (
    from_id INTEGER NOT NULL REFERENCES tracks(id),
    to_id   INTEGER NOT NULL REFERENCES tracks(id),
    weight  REAL NOT NULL DEFAULT 1.0,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (from_id, to_id)
);

CREATE TABLE IF NOT EXISTS playlists (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL,
    name        TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    meta_json   TEXT
);

CREATE TABLE IF NOT EXISTS playlist_tracks (
    playlist_id INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    track_id    INTEGER NOT NULL REFERENCES tracks(id),
    explanation TEXT,
    PRIMARY KEY (playlist_id, position)
);

CREATE TABLE IF NOT EXISTS feature_weights (
    week_key TEXT NOT NULL,
    dim      INTEGER NOT NULL,
    weight   REAL NOT NULL,
    PRIMARY KEY (week_key, dim)
);

CREATE TABLE IF NOT EXISTS user_profile_snapshots (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    context    TEXT NOT NULL,
    -- global|morning|evening|weekday|weekend
    embedding  BLOB NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS scan_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS listen_later (
    track_id INTEGER PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    added_at TEXT NOT NULL,
    position INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    payload_json TEXT,
    result_json TEXT,
    error TEXT,
    -- #40 lease: attempts = сколько раз аренда истекла (reaper), claimed_at —
    -- момент последнего claim/heartbeat; контракт PG 00001_init.sql.
    attempts INTEGER NOT NULL DEFAULT 0,
    claimed_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS discover_tips (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,
    artist TEXT,
    album TEXT,
    score REAL NOT NULL DEFAULT 0,
    track_ids_json TEXT NOT NULL,
    explanation TEXT,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS favorites (
    track_id INTEGER PRIMARY KEY,
    added_at TEXT NOT NULL,
    position INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS favorite_artists (
    artist TEXT PRIMARY KEY,
    added_at TEXT NOT NULL,
    position INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS favorite_albums (
    artist TEXT NOT NULL,
    album TEXT NOT NULL,
    added_at TEXT NOT NULL,
    position INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (artist, album)
);

CREATE TABLE IF NOT EXISTS radio_shares (
    token TEXT PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    revoked_at TEXT,
    last_listen_at TEXT,
    listen_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS play_sessions (
    id TEXT PRIMARY KEY,
    mode TEXT NOT NULL DEFAULT '',
    current_id INTEGER NOT NULL DEFAULT 0,
    queue_json TEXT,
    exclude_json TEXT,
    rated_json TEXT,
    daily_ids_json TEXT,
    daily_pos INTEGER NOT NULL DEFAULT 0,
    playlist_name TEXT NOT NULL DEFAULT '',
    playlist_kind TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS lyrics (
    track_id INTEGER PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    plain_lyrics TEXT NOT NULL DEFAULT '',
    synced_lyrics TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT '',
    source_id TEXT NOT NULL DEFAULT '',
    instrumental INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending',
    -- pending | ready | missing | failed
    error TEXT,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tracks_md5 ON tracks(file_md5);
CREATE INDEX IF NOT EXISTS idx_tracks_fp ON tracks(fingerprint);
CREATE INDEX IF NOT EXISTS idx_tracks_active ON tracks(is_active);
CREATE INDEX IF NOT EXISTS idx_features_status ON features(status);
CREATE INDEX IF NOT EXISTS idx_history_ts ON listening_history(ts);
CREATE INDEX IF NOT EXISTS idx_history_weekday_action ON listening_history(weekday, action);
CREATE INDEX IF NOT EXISTS idx_impressions_shown_at ON recommendation_impressions(shown_at);
CREATE INDEX IF NOT EXISTS idx_impressions_session_track
    ON recommendation_impressions(session_id, track_id, shown_at);
CREATE INDEX IF NOT EXISTS idx_playlists_kind_id ON playlists(kind, id DESC);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_discover_kind ON discover_tips(kind, created_at);
"""


def _seed_legacy(path: Path, *, dim: int = 8) -> None:
    """Легаси-база по исторической схеме + представительские данные всех таблиц."""
    conn = sqlite3.connect(path)
    conn.executescript(_LEGACY_SCHEMA)
    now = datetime.now(UTC)
    iso = now.isoformat()

    conn.executemany(
        "INSERT INTO artists (name, name_norm, sort_name, created_at) VALUES (?, ?, ?, ?)",
        [("Alpha", "alpha", "", iso), ("Beta", "beta", "", iso)],
    )
    conn.execute(
        "INSERT INTO albums (artist_id, title, title_norm, year, cover_track_id, created_at)"
        " VALUES (1, 'Album One', 'album one', 2001, 1, ?)",
        (iso,),
    )
    tracks = [
        # (id, path, artist, album, fk или None → derivation из строк, year, bpm)
        (1, "/lib/a1.flac", "Alpha", "Album One", 1, 1, 1, 2001, 120.5),
        (2, "/lib/a2.flac", "Alpha", "Album One", 1, 1, 1, 2001, 100.0),
        (3, "/lib/b1.flac", "Beta, Гамма", "Album Two", None, None, None, 2002, 90.0),
    ]
    for tid, p, artist, album, aid, aaid, alid, year, bpm in tracks:
        conn.execute(
            """
            INSERT INTO tracks (id, path, file_md5, file_mtime, file_size, title,
                artist, album, artist_id, album_artist_id, album_id, year,
                track_number, duration, bitrate, sample_rate, channels, is_active,
                created_at, updated_at)
            VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
            """,
            (
                tid,
                p,
                f"md5{tid}",
                1.0,
                1000,
                f"t{tid}",
                artist,
                album,
                aid,
                aaid,
                alid,
                year,
                tid,
                bpm,
                320,
                44100,
                2,
                1,
                iso,
                iso,
            ),
        )
        vec = _unit_vec(dim, seed=tid) if tid <= 2 else None
        conn.execute(
            """
            INSERT INTO features (track_id, embedding, embedding_dim, bpm, key_name,
                mode, lufs, cluster_id, status, computed_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                tid,
                vec.tobytes() if vec is not None else None,
                dim if vec is not None else None,
                bpm,
                "C",
                "major",
                -14.0,
                tid % 2,
                "ready" if vec is not None else "pending",
                iso if vec is not None else None,
            ),
        )

    conn.executemany(
        """
        INSERT INTO listening_history (track_id, ts, source, action, daypart,
            weekday, position_sec, duration_sec, listened_sec, session_id, reason)
        VALUES (?, ?, 'cli', ?, 'evening', 3, 30.0, 120.0, 110.5, ?, ?)
        """,
        [
            (1, (now - timedelta(hours=2)).isoformat(), "finish", "s1", "completed"),
            (2, (now - timedelta(hours=1)).isoformat(), "skip", "s1", "skipped"),
        ],
    )
    conn.execute(
        "INSERT INTO rec_stats (track_id, shown, skipped_early, completed, updated_at)"
        " VALUES (1, 3, 1, 2, ?)",
        (iso,),
    )
    conn.execute(
        """
        INSERT INTO recommendation_impressions (session_id, track_id, position,
            score, cosine_taste, cosine_current, explore, new_boost, maturity,
            mode, shown_at)
        VALUES ('s1', 1, 0, 0.5, 0.1, 0.2, 0, 0, 'core', 'daily', ?)
        """,
        (iso,),
    )
    conn.execute(
        "INSERT INTO transitions (from_id, to_id, weight, updated_at) VALUES (1, 2, 4.0, ?)",
        (iso,),
    )
    conn.execute(
        "INSERT INTO playlists (id, kind, name, created_at, meta_json)"
        " VALUES (7, 'daily', 'Daily Mix', ?, ?)",
        (iso, json.dumps({"k": 1})),
    )
    conn.executemany(
        "INSERT INTO playlist_tracks (playlist_id, position, track_id, explanation)"
        " VALUES (?, ?, ?, ?)",
        [(7, 0, 1, "seed"), (7, 1, 2, None)],
    )
    for d in range(4):
        conn.execute(
            "INSERT INTO feature_weights (week_key, dim, weight) VALUES ('2026-W01', ?, ?)",
            (d, 0.25),
        )
    conn.executemany(
        "INSERT INTO user_profile_snapshots (context, embedding, created_at) VALUES (?, ?, ?)",
        [
            (
                "global",
                _unit_vec(dim, seed=10).tobytes(),
                (now - timedelta(days=1)).isoformat(),
            ),
            ("global", _unit_vec(dim, seed=11).tobytes(), iso),  # latest
            ("morning", _unit_vec(dim, seed=12).tobytes(), iso),
        ],
    )
    conn.execute(
        "INSERT INTO scan_state (key, value) VALUES ('clap_model', ?)",
        ("laion/larger_clap_music_and_speech",),
    )
    conn.execute(
        "INSERT INTO scan_state (key, value) VALUES ('last_scan', '2026-09-01')"
    )
    conn.execute(
        "INSERT INTO listen_later (track_id, added_at, position) VALUES (2, ?, 0)",
        (iso,),
    )
    conn.execute(
        "INSERT INTO jobs (kind, status, created_at, updated_at) VALUES ('scan', 'done', ?, ?)",
        (iso, iso),
    )
    conn.execute(
        """
        INSERT INTO discover_tips (id, kind, artist, album, score, track_ids_json,
            explanation, created_at)
        VALUES (3, 'new_album', 'Beta', 'Album Two', 1.5, ?, 'tip', ?)
        """,
        (json.dumps([3]), iso),
    )
    conn.execute(
        "INSERT INTO favorites (track_id, added_at, position) VALUES (1, ?, 0)", (iso,)
    )
    conn.execute(
        "INSERT INTO favorite_artists (artist, added_at, position) VALUES (?, ?, 0)",
        ("alpha", iso),
    )
    conn.execute(
        "INSERT INTO favorite_artists (artist, added_at, position) VALUES (?, ?, 1)",
        ("Ghost Artist", iso),  # нет в каталоге → skip + WARN
    )
    conn.execute(
        "INSERT INTO favorite_albums (artist, album, added_at, position) VALUES (?, ?, ?, 0)",
        ("Alpha", "Album One", iso),
    )
    conn.execute(
        "INSERT INTO favorite_albums (artist, album, added_at, position) VALUES (?, ?, ?, 1)",
        ("X", "Y", iso),  # нет в каталоге → skip + WARN
    )
    conn.execute(
        "INSERT INTO radio_shares (token, name, created_at, listen_count) VALUES ('tok1', 'r', ?, 5)",
        (iso,),
    )
    conn.execute(
        """
        INSERT INTO play_sessions (id, mode, current_id, queue_json, daily_ids_json,
            daily_pos, playlist_name, playlist_kind, updated_at)
        VALUES ('sess-1', 'radio', 1, ?, ?, 0, 'Daily Mix', 'daily', ?)
        """,
        (json.dumps([1, 2]), json.dumps([3]), iso),
    )
    conn.execute(
        """
        INSERT INTO lyrics (track_id, plain_lyrics, synced_lyrics, source, source_id,
            instrumental, status, updated_at)
        VALUES (1, 'la-la', '', 'lrclib', 'sid1', 0, 'ready', ?)
        """,
        (iso,),
    )
    conn.executemany(
        "INSERT INTO genres (id, name) VALUES (?, ?)", [(1, "Rock"), (2, "Jazz")]
    )
    conn.executemany(
        "INSERT INTO track_genres (track_id, genre_id) VALUES (?, ?)",
        [(1, 1), (1, 2), (3, 1)],
    )
    conn.commit()
    conn.close()


# ------------------------------------------------------------ unit (без PG)


def test_dry_run_plan(tmp_path: Path):
    from music_hive.db.migrate_sqlite import dry_run

    db = tmp_path / "legacy.db"
    _seed_legacy(db)
    rows, info = dry_run(db)

    by_table = {r.table: r for r in rows}
    assert by_table["tracks"].rows == 3
    assert by_table["listening_history"].rows == 2
    assert by_table["jobs"].target.startswith("— ПРОПУСК")
    assert "clap_model" in by_table["scan_state"].target  # ключ уходит в реестр моделей
    assert info.emb_dims == {8: 2}
    assert info.emb_broken == 0
    assert info.ready_without_vector == 0
    assert info.unknown_tables == []
    assert info.model_from_source is True  # в seed есть scan_state.clap_model


def test_dry_run_model_default_when_scan_state_empty(tmp_path: Path):
    from music_hive.db import store
    from music_hive.db.migrate_sqlite import dry_run

    db = tmp_path / "legacy.db"
    _seed_legacy(db)
    conn = sqlite3.connect(db)
    conn.execute("DELETE FROM scan_state WHERE key = 'clap_model'")
    conn.commit()
    conn.close()
    _, info = dry_run(db)
    assert info.model_from_source is False
    assert info.model_key == store.model_key()


def test_resolve_model_key_variants(tmp_path: Path):
    from music_hive.db import store
    from music_hive.db.migrate_sqlite import open_source, resolve_model_key

    db = tmp_path / "mini.db"
    conn = sqlite3.connect(db)
    conn.executescript(
        "CREATE TABLE scan_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)"
    )
    conn.commit()
    conn.close()

    src = open_source(db)
    assert resolve_model_key(src) == (store.model_key(), False)
    src.close()

    conn = sqlite3.connect(db)
    conn.execute("INSERT INTO scan_state VALUES ('clap_model', 'custom/repo')")
    conn.commit()
    conn.close()
    src = open_source(db)
    assert resolve_model_key(src) == ("clap:custom/repo", True)
    src.close()

    conn = sqlite3.connect(db)
    conn.execute("UPDATE scan_state SET value = 'clap:x/y' WHERE key = 'clap_model'")
    conn.commit()
    conn.close()
    src = open_source(db)
    assert resolve_model_key(src) == ("clap:x/y", True)
    src.close()


def test_dry_run_broken_blobs(tmp_path: Path):
    from music_hive.db.migrate_sqlite import dry_run

    db = tmp_path / "legacy.db"
    _seed_legacy(db)
    conn = sqlite3.connect(db)
    # битый blob: длина не кратна 4 у ready-строки
    conn.execute(
        "UPDATE features SET embedding = X'0102', embedding_dim = 8 WHERE track_id = 1"
    )
    conn.commit()
    conn.close()
    _, info = dry_run(db)
    assert info.emb_broken == 1
    assert info.emb_dims == {8: 1}
    assert info.ready_without_vector == 1


# ------------------------------------------------------------------ PG-гейт
# Фикстура pg_env — общая, из tests/conftest.py.


def _one(conn, sql: str, params: tuple = ()) -> object:
    row = conn.execute(sql, params).fetchone()
    return None if row is None else next(iter(row.values()))


@pg_gate
def test_pg_migrate_full_cycle(pg_env, tmp_path: Path):
    from music_hive.db import backend
    from music_hive.db.migrate_sqlite import apply_migration, verify_migration
    from music_hive.db.schema import connect
    from music_hive.db.store import emb_table_name, model_key

    db = tmp_path / "legacy.db"
    _seed_legacy(db)

    stats = apply_migration(db)
    assert stats["model_key"] == model_key()
    assert stats["embeddings"]["saved"] == 2
    assert stats["track_audio_features"]["demoted_to_pending"] == 0
    warn_text = " ".join(stats["warnings"])
    assert "Ghost Artist" in warn_text and "X / Y" in warn_text

    with connect() as conn:
        owner = backend.owner_id()
        # id треков сохранены
        assert sorted(
            int(r["id"]) for r in conn.execute("SELECT id FROM tracks").fetchall()
        ) == [1, 2, 3]
        # артисты: alpha, beta + выведенный из строк «Beta, Гамма»
        norms = {
            str(r["name_norm"])
            for r in conn.execute("SELECT name_norm FROM artists").fetchall()
        }
        assert {"alpha", "beta", "beta, гамма"} <= norms
        # derivation: у трека 3 проставлены FK
        t3 = conn.execute(
            "SELECT artist_id, album_id FROM tracks WHERE id = 3"
        ).fetchone()
        assert t3["artist_id"] is not None and t3["album_id"] is not None
        album2 = conn.execute(
            "SELECT a.title_norm, a.year, ar.name_norm FROM albums a"
            " JOIN artists ar ON ar.id = a.artist_id WHERE a.id = %s",
            (t3["album_id"],),
        ).fetchone()
        assert album2["title_norm"] == "album two" and int(album2["year"]) == 2002
        assert album2["name_norm"] == "beta, гамма"
        # обложка
        assert (
            _one(
                conn, "SELECT cover_track_id FROM albums WHERE title_norm = 'album one'"
            )
            == 1
        )
        # история → владелец, id/track_id сохранены
        hist = conn.execute(
            "SELECT id, user_id, track_id, action FROM listening_history ORDER BY id"
        ).fetchall()
        assert [(h["user_id"], h["track_id"], h["action"]) for h in hist] == [
            (owner, 1, "finish"),
            (owner, 2, "skip"),
        ]
        # избранное: 3 перенесено, 2 skip
        assert (
            _one(conn, "SELECT COUNT(*) FROM user_favorites WHERE kind = 'track'") == 1
        )
        assert (
            _one(conn, "SELECT COUNT(*) FROM user_favorites WHERE kind = 'artist'") == 1
        )
        assert (
            _one(conn, "SELECT COUNT(*) FROM user_favorites WHERE kind = 'album'") == 1
        )
        # плейлисты: id сохранён, владелец, приватность
        pl = conn.execute(
            "SELECT owner_user_id, visibility, meta_json FROM playlists WHERE id = 7"
        ).fetchone()
        assert pl["owner_user_id"] == owner and pl["visibility"] == "private"
        assert json.loads(str(pl["meta_json"])) == {"k": 1}
        assert (
            _one(conn, "SELECT COUNT(*) FROM playlist_tracks WHERE playlist_id = 7")
            == 2
        )
        # векторы: в emb-таблице активной модели, id сохранены
        table = emb_table_name(model_key())
        assert _one(conn, f"SELECT COUNT(*) FROM {table}") == 2
        # жанры: id перемаплены по имени
        assert (
            _one(
                conn,
                """
            SELECT COUNT(*) FROM track_genres tg JOIN genres g ON g.id = tg.genre_id
            WHERE tg.track_id = 1 AND g.name = 'Rock'
            """,
            )
            == 1
        )
        # профиль вкуса: latest на context
        assert _one(conn, "SELECT COUNT(*) FROM user_taste_profiles") == 2
        # play_sessions: id треков внутри JSON валидны
        assert (
            _one(conn, "SELECT current_id FROM play_sessions WHERE id = 'sess-1'") == 1
        )
        # jobs не переносятся, clap_model не переносится
        assert _one(conn, "SELECT COUNT(*) FROM jobs") == 0
        assert _one(conn, "SELECT COUNT(*) FROM scan_state") == 1
        assert (
            _one(conn, "SELECT value FROM scan_state WHERE key = 'last_scan'")
            == "2026-09-01"
        )

    rows, ok = verify_migration(db)
    assert ok, [r for r in rows if not r.ok]

    # --- повторный apply: идемпотентен
    apply_migration(db)
    with connect() as conn:
        assert _one(conn, "SELECT COUNT(*) FROM tracks") == 3
        assert _one(conn, "SELECT COUNT(*) FROM listening_history") == 2
        assert _one(conn, "SELECT COUNT(*) FROM playlists") == 1
        assert _one(conn, "SELECT COUNT(*) FROM user_favorites") == 3
        assert _one(conn, "SELECT COUNT(*) FROM track_genres") == 3
    rows, ok = verify_migration(db)
    assert ok, [r for r in rows if not r.ok]

    # --- «грязная» цель + truncate + apply: полный повторный перенос
    with connect() as conn:
        conn.execute("INSERT INTO tracks (path) VALUES ('/extra.flac')")
        conn.execute("INSERT INTO scan_state (key, value) VALUES ('worker', 'ran')")
    _, ok = verify_migration(db)
    assert not ok  # расхождение видно
    apply_migration(db, truncate=True)
    with connect() as conn:
        assert _one(conn, "SELECT COUNT(*) FROM tracks") == 3
        assert _one(conn, "SELECT COUNT(*) FROM scan_state") == 1  # 'worker' стёрт
    rows, ok = verify_migration(db)
    assert ok, [r for r in rows if not r.ok]
    with connect() as conn:
        # sequence за max(id): новый трек получает id > 3 (setval сработал)
        cur = conn.execute(
            "INSERT INTO tracks (path, title) VALUES ('/post-migrate.flac', 'x') RETURNING id"
        ).fetchone()
        assert int(cur["id"]) > 3


@pg_gate
def test_pg_migrate_broken_blob_demotes(pg_env, tmp_path: Path):
    """ready с битым blob → pending; вектор NaN → pending (дизайн §10.3)."""
    from music_hive.db.migrate_sqlite import apply_migration, verify_migration
    from music_hive.db.schema import connect

    db = tmp_path / "legacy.db"
    _seed_legacy(db)
    conn = sqlite3.connect(db)
    # битая длина у ready-строки + NaN-вектор
    conn.execute("UPDATE features SET embedding = X'0102' WHERE track_id = 1")
    nan_vec = _unit_vec(8, seed=1)
    nan_vec[0] = np.float32("nan")
    conn.execute(
        "UPDATE features SET embedding = ? WHERE track_id = 2", (nan_vec.tobytes(),)
    )
    conn.commit()
    conn.close()

    stats = apply_migration(db)
    assert stats["track_audio_features"]["demoted_to_pending"] == 1  # битая длина
    assert stats["embeddings"]["saved"] == 0
    assert stats["embeddings"]["demoted"] == 1  # NaN
    with connect() as conn:
        statuses = {
            int(r["track_id"]): str(r["status"])
            for r in conn.execute(
                "SELECT track_id, status FROM track_audio_features"
            ).fetchall()
        }
    assert statuses == {1: "pending", 2: "pending", 3: "pending"}
    rows, ok = verify_migration(db)
    assert ok, [r for r in rows if not r.ok]


@pg_gate
def test_pg_migrate_foreign_model_from_scan_state(pg_env, tmp_path: Path, monkeypatch):
    """Модель из scan_state: векторы идут в её emb-таблицу; env восстанавливается."""
    import os

    from music_hive.config import get_settings
    from music_hive.db.migrate_sqlite import apply_migration, verify_migration
    from music_hive.db.schema import connect
    from music_hive.db.store import emb_table_name, model_key

    # гарантируем, что активной уже является модель config: тогда модель
    # источника регистрируется, но НЕ активируется (переключение — job F4)
    with connect() as conn:
        conn.execute(
            """
            INSERT INTO embedding_models (model_key, backend, repo, dim, is_active)
            VALUES (%s, 'clap', 'laion/larger_clap_music_and_speech', 8, TRUE)
            ON CONFLICT DO NOTHING
            """,
            (model_key(),),
        )

    before = os.environ.get("MUSIC_HIVE_CLAP_MODEL")
    db = tmp_path / "legacy.db"
    _seed_legacy(db)
    conn = sqlite3.connect(db)
    conn.execute("UPDATE scan_state SET value = 'custom/repo' WHERE key = 'clap_model'")
    conn.commit()
    conn.close()

    stats = apply_migration(db)
    assert stats["model_key"] == "clap:custom/repo"
    assert stats["model_from_source"] is True
    # env восстановлен, config не изменён
    assert os.environ.get("MUSIC_HIVE_CLAP_MODEL") == before
    get_settings.cache_clear()
    with connect() as conn:
        table = emb_table_name("clap:custom/repo")
        assert _one(conn, f"SELECT COUNT(*) FROM {table}") == 2
        # model_key содержит ':' — только параметром ( PgConn-адаптер переводит
        # ":word" в SQL-литерале в %(word)s placeholder)
        assert (
            _one(
                conn,
                "SELECT dim FROM embedding_models WHERE model_key = %s",
                ("clap:custom/repo",),
            )
            == 8
        )
        assert "clap:custom/repo" in " ".join(str(w) for w in stats["warnings"])
    rows, ok = verify_migration(db)
    assert ok, [r for r in rows if not r.ok]
