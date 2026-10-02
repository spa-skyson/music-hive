from __future__ import annotations

import json
from datetime import datetime, timezone

import numpy as np

from music_hive.brain import generators, mixes
from music_hive.brain.store import latest_playlist
from music_hive.config import get_settings
from music_hive.db import backend
from music_hive.db.schema import connect, init_db, utcnow
from music_hive.index.brute import EmbeddingIndex


def _catalog(size: int = 36) -> EmbeddingIndex:
    rng = np.random.default_rng(7)
    half = size // 2
    left = np.column_stack(
        [
            np.ones(half),
            rng.normal(0, 0.15, half),
            rng.normal(0, 0.08, half),
            rng.normal(0, 0.08, half),
        ]
    )
    right_size = size - half
    right = np.column_stack(
        [
            rng.normal(0, 0.15, right_size),
            np.ones(right_size),
            rng.normal(0, 0.08, right_size),
            rng.normal(0, 0.08, right_size),
        ]
    )
    matrix = np.vstack([left, right]).astype(np.float32)
    matrix /= np.linalg.norm(matrix, axis=1, keepdims=True)
    return EmbeddingIndex(
        track_ids=np.arange(1, size + 1, dtype=np.int64),
        matrix=matrix,
        meta=[
            {
                "id": i + 1,
                "artist": f"artist-{i % 9}",
                "title": f"track-{i + 1}",
                "album": f"album-{i % 6}",
                "cluster_id": 0 if i < half else 1,
                "created_at": utcnow(),
            }
            for i in range(size)
        ],
        md5s=[f"md5-{i}" for i in range(size)],
    )


def _ensure_model() -> str:
    """Реестр + активная модель dim=4 (под FakeIndex-каталог); возвращает ключ."""
    key = "clap:test/integration"
    with connect() as conn:
        conn.execute(
            """
            INSERT INTO embedding_models(model_key, backend, repo, dim, is_active)
            VALUES (?, 'clap', 'test/integration', 4, TRUE)
            ON CONFLICT (model_key) DO UPDATE SET is_active = TRUE
            """,
            (key,),
        )
    return key


def _save_online_taste(vector: np.ndarray, model_key: str) -> None:
    """Go EMA пишет online-снимок 'global' → user_taste_profiles (PG)."""
    normalized = vector / (np.linalg.norm(vector) + 1e-12)
    with connect() as conn:
        conn.execute(
            """
            INSERT INTO user_taste_profiles(
                user_id, context, model_key, vec, n_positive, n_negative, updated_at
            ) VALUES (?, 'global', ?, ?, 0, 0, ?)
            ON CONFLICT (user_id, context, model_key) DO UPDATE SET
                vec = excluded.vec,
                updated_at = excluded.updated_at
            """,
            (backend.owner_id(), model_key, normalized.astype(np.float32), utcnow()),
        )


def _insert_tracks(index: EmbeddingIndex) -> None:
    now = datetime.now(timezone.utc).isoformat()
    with connect() as conn:
        for meta in index.meta:
            conn.execute(
                """
                INSERT INTO tracks(
                    id, path, title, artist, album, is_active, created_at, updated_at
                ) OVERRIDING SYSTEM VALUE VALUES (?, ?, ?, ?, ?, TRUE, ?, ?)
                """,
                (
                    meta["id"],
                    f"/music/{meta['id']}.flac",
                    meta["title"],
                    meta["artist"],
                    meta["album"],
                    now,
                    now,
                ),
            )


def test_feedback_changes_taste_and_next_mix_pack(pg_env, monkeypatch) -> None:
    get_settings.cache_clear()
    init_db()
    model_key = _ensure_model()
    index = _catalog()
    monkeypatch.setattr(mixes, "load_index", lambda: index)
    monkeypatch.setattr(generators, "load_index", lambda: index)

    _insert_tracks(index)

    taste_before = index.matrix[:6].mean(axis=0)
    _save_online_taste(taste_before, model_key)
    first_result = mixes.generate_mix_pack(
        daily_size=6,
        for_you_size=8,
        weekday_size=5,
        weekly_size=8,
        new_size=5,
    )
    first = latest_playlist("for_you")
    assert first is not None

    # Simulate the Go feedback path: listen + explicit like followed by the
    # persisted online EMA snapshot consumed by the Python mix worker.
    liked_track = 30
    with connect() as conn:
        for action in ("start", "like"):
            conn.execute(
                """
                INSERT INTO listening_history(
                    user_id, track_id, ts, source, action, weekday
                ) VALUES (?, ?, ?, 'integration-test', ?, 5)
                """,
                (backend.owner_id(), liked_track, utcnow(), action),
            )
    taste_after = 0.25 * taste_before + 0.75 * index.matrix[liked_track - 1]
    _save_online_taste(taste_after, model_key)

    second_result = mixes.generate_mix_pack(
        daily_size=6,
        for_you_size=8,
        weekday_size=5,
        weekly_size=8,
        new_size=5,
    )
    second = latest_playlist("for_you")
    assert second is not None

    before_ids = {track["track_id"] for track in first["tracks"]}
    after_ids = {track["track_id"] for track in second["tracks"]}
    overlap = len(before_ids & after_ids) / len(before_ids | after_ids)
    taste_cosine = float(
        np.dot(
            taste_before / np.linalg.norm(taste_before),
            taste_after / np.linalg.norm(taste_after),
        )
    )

    assert first_result["for_you"]["id"] != second_result["for_you"]["id"]
    assert first["id"] != second["id"]
    assert taste_cosine < 0.9
    assert overlap <= 0.5
    assert liked_track not in after_ids
    get_settings.cache_clear()


def test_mix_pack_has_distinct_shelf_openings_without_weekday_history(
    pg_env, monkeypatch
) -> None:
    get_settings.cache_clear()
    init_db()
    index = _catalog(220)
    monkeypatch.setattr(mixes, "load_index", lambda: index)
    monkeypatch.setattr(generators, "load_index", lambda: index)
    _insert_tracks(index)
    _save_online_taste(index.matrix[:20].mean(axis=0), _ensure_model())

    mixes.generate_mix_pack(
        daily_size=12,
        for_you_size=12,
        weekday_size=12,
        weekly_size=12,
        new_size=12,
        reserve_prefix=10,
    )

    kinds = [
        "for_you",
        "daily",
        "new_releases",
        "weekly",
        *(key for key, _title, _weekday in mixes.WEEKDAY_KEYS),
    ]
    openings: dict[str, set[int]] = {}
    full: dict[str, set[int]] = {}
    first_ids: dict[str, int] = {}
    for kind in kinds:
        playlist = latest_playlist(kind)
        assert playlist is not None
        ids = [int(track["track_id"]) for track in playlist["tracks"]]
        assert len(ids) == 12
        first_ids[kind] = ids[0]
        openings[kind] = set(ids[:10])
        full[kind] = set(ids)

    assert len(set(first_ids.values())) == len(kinds)
    for pos, left in enumerate(kinds):
        for right in kinds[pos + 1 :]:
            assert openings[left].isdisjoint(openings[right])
            overlap = len(full[left] & full[right]) / len(full[left] | full[right])
            assert overlap <= 0.2

    with connect() as conn:
        weekday_meta = conn.execute(
            """
            SELECT meta_json FROM playlists
            WHERE kind = 'weekday_tue'
            ORDER BY id DESC LIMIT 1
            """
        ).fetchone()
    assert weekday_meta is not None
    assert json.loads(weekday_meta["meta_json"])["weekday_signals"] == 0
    get_settings.cache_clear()
