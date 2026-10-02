"""Album discovery tips: new releases and resurfaced catalog gems."""

from __future__ import annotations

import json
from collections import defaultdict
from datetime import datetime, timedelta, timezone
from typing import Any

import numpy as np

from music_hive.db import backend
from music_hive.db.schema import connect, utcnow
from music_hive.index.brute import EmbeddingIndex, load_index
from music_hive.listen.profile import resolve_taste


def _parse_ts(ts: object) -> datetime:
    """PG отдаёт datetime; приводим к общему виду."""
    if isinstance(ts, datetime):
        return ts if ts.tzinfo else ts.replace(tzinfo=timezone.utc)
    return datetime.fromisoformat(str(ts).replace("Z", "+00:00"))


def _taste_vector(index: EmbeddingIndex) -> np.ndarray:
    vec, _src = resolve_taste(index)
    return vec


def _cosine(a: np.ndarray, b: np.ndarray) -> float:
    return float(np.dot(a, b))


# Ключ группы альбома (F1.2): ("id", album_id), fallback при пустом FK —
# ("name", artist, album) по строкам трека (переходный период до backfill).
AlbumKey = tuple[str, int] | tuple[str, str, str]


def _album_groups(
    index: EmbeddingIndex,
) -> tuple[dict[AlbumKey, list[int]], dict[AlbumKey, tuple[str, str]]]:
    """Группировка по album_id (fallback — строки) → (groups, имена для типсов)."""
    track_ids = [int(m["id"]) for m in index.meta]
    key_by_id: dict[int, AlbumKey] = {}
    names: dict[AlbumKey, tuple[str, str]] = {}
    if track_ids:
        placeholders = ",".join("?" * len(track_ids))
        with connect() as conn:
            rows = conn.execute(
                f"""
                SELECT t.id, t.artist, t.album, t.album_id,
                       ar.name AS album_artist_name, al.title AS album_title
                FROM tracks t
                LEFT JOIN albums al ON al.id = t.album_id
                LEFT JOIN artists ar ON ar.id = al.artist_id
                WHERE t.id IN ({placeholders})
                """,
                track_ids,
            ).fetchall()
        for r in rows:
            artist = (r["artist"] or "").strip()
            album = (r["album"] or "").strip()
            if r["album_id"] is not None:
                key: AlbumKey = ("id", int(r["album_id"]))
                # имена — из сущностей альбома, строки трека как запасной вариант
                display = (
                    (r["album_artist_name"] or artist) or "",
                    (r["album_title"] or album) or "",
                )
            else:
                key = ("name", artist, album)
                display = (artist, album)
            key_by_id[int(r["id"])] = key
            names.setdefault(key, display)

    groups: dict[AlbumKey, list[int]] = defaultdict(list)
    for i, meta in enumerate(index.meta):
        tid = int(meta["id"])
        key = key_by_id.get(tid)
        if key is None or (key[0] == "name" and not key[2]):
            continue
        groups[key].append(i)
    return groups, names


def _track_created_at() -> dict[int, str]:
    with connect() as conn:
        rows = conn.execute(
            """
            SELECT id, created_at FROM tracks
            WHERE is_active = TRUE AND is_duplicate_of IS NULL
            """
        ).fetchall()
    return {int(r["id"]): r["created_at"] for r in rows}


def _rec_completed() -> dict[int, int]:
    with connect() as conn:
        rows = conn.execute("SELECT track_id, completed FROM rec_stats").fetchall()
    return {int(r["track_id"]): int(r["completed"]) for r in rows}


def _album_dates(
    row_indices: list[int], index: EmbeddingIndex, created: dict[int, str]
) -> tuple[datetime | None, datetime | None]:
    dates: list[datetime] = []
    for i in row_indices:
        tid = int(index.meta[i]["id"])
        ts = created.get(tid)
        if ts:
            dates.append(_parse_ts(ts))
    if not dates:
        return None, None
    return min(dates), max(dates)


def _album_mean_embedding(index: EmbeddingIndex, row_indices: list[int]) -> np.ndarray:
    return index.centroid(row_indices)


def _top_track_ids(
    index: EmbeddingIndex, row_indices: list[int], taste: np.ndarray, *, limit: int = 3
) -> list[int]:
    sims = [(i, _cosine(index.matrix[i], taste)) for i in row_indices]
    sims.sort(key=lambda x: -x[1])
    return [int(index.meta[i]["id"]) for i, _ in sims[:limit]]


def _save_tips(kind: str, tips: list[dict[str, Any]]) -> int:
    now = utcnow()
    with connect() as conn:
        conn.execute("DELETE FROM discover_tips WHERE kind = ?", (kind,))
        for tip in tips:
            # discover_tips.user_id NOT NULL: подсказки считаются от вкуса
            # → per-user (§4.2); F1 — от владельца.
            conn.execute(
                """
                INSERT INTO discover_tips(
                    user_id, kind, artist, album, score, track_ids_json,
                    explanation, created_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    backend.owner_id(),
                    kind,
                    tip.get("artist"),
                    tip.get("album"),
                    float(tip["score"]),
                    json.dumps(tip["track_ids"], ensure_ascii=False),
                    tip.get("explanation"),
                    now,
                ),
            )
    return len(tips)


def rebuild_discover_tips(
    *,
    new_album_days: int = 14,
    limit_new: int = 10,
    limit_old: int = 10,
) -> dict[str, Any]:
    """
    Recompute discover_tips for new_album and resurfaced kinds.
    Clears previous tips of each kind before insert.
    """
    index = load_index()
    if index.size == 0:
        with connect() as conn:
            conn.execute("DELETE FROM discover_tips")
        return {"new_album": 0, "resurfaced": 0, "reason": "empty index"}

    taste = _taste_vector(index)
    created = _track_created_at()
    completed = _rec_completed()
    groups, names = _album_groups(index)
    cutoff = datetime.now(timezone.utc) - timedelta(days=new_album_days)

    # --- new_album ---
    new_candidates: list[dict[str, Any]] = []
    new_keys: set[AlbumKey] = set()
    for key, rows in groups.items():
        artist, album = names.get(key, ("", ""))
        if not album:
            continue
        oldest, newest = _album_dates(rows, index, created)
        if newest is None or newest < cutoff:
            continue
        mean_emb = _album_mean_embedding(index, rows)
        score = _cosine(mean_emb, taste)
        track_ids = _top_track_ids(index, rows, taste)
        new_candidates.append(
            {
                "artist": artist or None,
                "album": album,
                "score": score,
                "track_ids": track_ids,
                "explanation": f"Новый альбом · cosine {score:.2f} к вкусу",
                "_key": key,
            }
        )
    new_candidates.sort(key=lambda x: -x["score"])
    new_tips = new_candidates[:limit_new]
    n_new = _save_tips("new_album", new_tips)

    # --- resurfaced ---
    # Prefer under-listened albums with high taste match. If the whole library
    # was scanned recently (created_at fresh), still surface "forgotten" albums
    # that are not already featured as new_album tips.
    new_keys = {t["_key"] for t in new_tips}
    old_candidates: list[dict[str, Any]] = []
    for key, rows in groups.items():
        if key in new_keys:
            continue
        artist, album = names.get(key, ("", ""))
        if not album:
            continue
        oldest, newest = _album_dates(rows, index, created)
        track_ids_in_album = [int(index.meta[i]["id"]) for i in rows]
        total_completed = sum(completed.get(tid, 0) for tid in track_ids_in_album)
        if total_completed > 2:
            continue
        mean_emb = _album_mean_embedding(index, rows)
        score = _cosine(mean_emb, taste)
        # slight bonus for truly older scan dates when available
        age_days = 0.0
        if oldest:
            age_days = max(
                0.0, (datetime.now(timezone.utc) - oldest).total_seconds() / 86400.0
            )
        is_fresh_scan = newest is not None and newest >= cutoff
        rank = score * (1.0 - 0.08 * total_completed) + min(age_days, 365) * 0.0005
        if is_fresh_scan:
            rank *= 0.95  # still allow when whole lib is "new" in DB
        track_ids = _top_track_ids(index, rows, taste)
        old_candidates.append(
            {
                "artist": artist or None,
                "album": album,
                "score": score,
                "track_ids": track_ids,
                "explanation": (
                    f"Из старого каталога · cosine {score:.2f} к вкусу"
                    f" · listens {total_completed}"
                ),
                "_rank": rank,
            }
        )
    old_candidates.sort(key=lambda x: -x["_rank"])
    resurfaced_tips = [
        {k: v for k, v in tip.items() if not k.startswith("_")}
        for tip in old_candidates[:limit_old]
    ]
    n_old = _save_tips("resurfaced", resurfaced_tips)

    return {
        "new_album": n_new,
        "resurfaced": n_old,
        "new_album_days": new_album_days,
    }
