"""Taste profile helpers.

Authority rules:
- context ``global`` — Go EMA online only (player writes; Python must NOT overwrite).
- context ``offline_report`` — Python rebuild from history for CLI/reports.
- ``resolve_taste()`` prefers Go ``global``, then offline snapshot, then fresh rebuild
  without persisting into ``global``.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from music_hive.db import backend
from music_hive.db.schema import connect, utcnow
from music_hive.db.store import active_model, model_key
from music_hive.index.brute import EmbeddingIndex, load_index

CONTEXT_ONLINE = "global"
CONTEXT_OFFLINE = "offline_report"

# Weights for profile update
ACTION_WEIGHT = {
    "finish": 1.0,
    "like": 1.5,
    "start": 0.15,
    "skip": -1.0,
    "dislike": -1.5,
    "track_end": 0.0,  # handled via reason in Go; ignore bare action here
}


@dataclass
class TasteProfile:
    embedding: np.ndarray  # unit vector
    n_positive: int
    n_negative: int
    context: str = CONTEXT_OFFLINE

    @property
    def ready(self) -> bool:
        return self.embedding.size > 0 and self.n_positive > 0


def _uid(user_id: int | None) -> int:
    """Явный user_id или владелец (F2.4). Вызывать только в PG-ветках."""
    return user_id if user_id is not None else backend.owner_id()


def build_profile(
    *,
    context: str = CONTEXT_OFFLINE,
    persist: bool = True,
    user_id: int | None = None,
) -> TasteProfile:
    """
    Weighted average of track embeddings by listen actions.
    Never persists into the online ``global`` context (reserved for Go EMA).

    Профиль считается и пишется per-user (user_id=None → владелец).
    """
    write_context = (
        CONTEXT_OFFLINE if context in (CONTEXT_ONLINE, "", "global") else context
    )

    index = load_index()
    if index.size == 0:
        return TasteProfile(np.zeros(0, dtype=np.float32), 0, 0, write_context)

    with connect() as conn:
        rows = conn.execute(
            """
            SELECT track_id, action FROM listening_history
            WHERE user_id = ?
            ORDER BY id DESC
            LIMIT 500
            """,
            (_uid(user_id),),
        ).fetchall()

    if not rows:
        return TasteProfile(index.centroid(), 0, 0, write_context)

    acc = np.zeros(index.dim, dtype=np.float64)
    w_sum = 0.0
    n_pos = 0
    n_neg = 0
    for r in rows:
        w = ACTION_WEIGHT.get(r["action"], 0.0)
        if w == 0.0:
            continue
        row = index.row_of(int(r["track_id"]))
        if row is None:
            continue
        acc += w * index.matrix[row]
        w_sum += abs(w)
        if w > 0:
            n_pos += 1
        else:
            n_neg += 1

    if w_sum < 1e-9 or n_pos == 0:
        return TasteProfile(index.centroid(), n_pos, n_neg, write_context)

    vec = acc.astype(np.float32)
    n = float(np.linalg.norm(vec))
    if n > 1e-12:
        vec = vec / n
    else:
        vec = index.centroid()

    if persist:
        # user_profile_snapshots → user_taste_profiles (живая строка,
        # PK (user_id, context, model_key); §4.2/§6.4 контракта).
        # Ключ — АКТИВНАЯ модель: профиль считается из её векторов
        # (load_index) и читается (latest_profile) по ней же; config-
        # ключ воркера при расхождении давал бы нечитаемую строку
        # (и FK-отказ на незарегистрированном ключе).
        am = active_model()
        with connect() as conn:
            conn.execute(
                """
                INSERT INTO user_taste_profiles(
                    user_id, context, model_key, vec, n_positive, n_negative, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(user_id, context, model_key) DO UPDATE SET
                    vec = excluded.vec,
                    n_positive = excluded.n_positive,
                    n_negative = excluded.n_negative,
                    updated_at = excluded.updated_at
                """,
                (
                    _uid(user_id),
                    write_context,
                    am["model_key"] if am is not None else model_key(),
                    vec,
                    n_pos,
                    n_neg,
                    utcnow(),
                ),
            )
    return TasteProfile(vec, n_pos, n_neg, write_context)


def latest_profile(
    context: str = CONTEXT_ONLINE, user_id: int | None = None
) -> TasteProfile | None:
    am = active_model()
    if am is None:
        return None
    with connect() as conn:
        row = conn.execute(
            """
            SELECT vec, n_positive FROM user_taste_profiles
            WHERE user_id = ? AND context = ? AND model_key = ?
            ORDER BY updated_at DESC LIMIT 1
            """,
            (_uid(user_id), context, am["model_key"]),
        ).fetchone()
    if not row or row["vec"] is None:
        return None
    vec = np.asarray(row["vec"], dtype=np.float32).reshape(-1).copy()
    n = float(np.linalg.norm(vec))
    if n > 1e-12:
        vec = vec / n
    return TasteProfile(vec, int(row["n_positive"] or 1), 0, context)


def resolve_taste(
    index: EmbeddingIndex | None = None, user_id: int | None = None
) -> tuple[np.ndarray, str]:
    """
    Taste vector for recommendations / tips (per-user).
    Prefer Go online snapshot, then offline report, then rebuild (no global write).
    """
    idx = index or load_index()
    online = latest_profile(CONTEXT_ONLINE, user_id=user_id)
    if online is not None and online.embedding.size == idx.dim:
        return online.embedding.astype(np.float32), "go_ema_global"
    offline = latest_profile(CONTEXT_OFFLINE, user_id=user_id)
    if offline is not None and offline.embedding.size == idx.dim:
        return offline.embedding.astype(np.float32), "offline_report"
    built = build_profile(context=CONTEXT_OFFLINE, persist=False, user_id=user_id)
    if built.embedding.size:
        return built.embedding.astype(np.float32), "history_rebuild"
    return idx.centroid(), "library_centroid"


def epsilon_explore_mask(
    size: int, explore_ratio: float, rng: np.random.Generator
) -> np.ndarray:
    """Boolean mask of length `size`: True = exploration slot."""
    n_ex = int(round(size * explore_ratio))
    n_ex = max(0, min(size, n_ex))
    if size >= 5:
        n_ex = max(1, n_ex) if explore_ratio > 0 else 0
    mask = np.zeros(size, dtype=bool)
    if n_ex == 0:
        return mask
    positions = np.linspace(2, size - 1, num=n_ex, dtype=int)
    mask[positions] = True
    if n_ex > 1:
        extras = rng.choice(size, size=min(n_ex, size), replace=False)
        mask[:] = False
        mask[extras] = True
    return mask
