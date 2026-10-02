"""Listening history + transitions (step 5 / realtime contract)."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from music_hive.db import backend
from music_hive.db.schema import connect, utcnow

ACTIONS = frozenset(
    {"start", "finish", "skip", "like", "dislike", "progress", "track_end"}
)


def _daypart(hour: int) -> str:
    if 5 <= hour < 12:
        return "morning"
    if 12 <= hour < 17:
        return "afternoon"
    if 17 <= hour < 23:
        return "evening"
    return "night"


def record_listen(
    track_id: int,
    action: str,
    *,
    source: str = "cli",
    position_sec: float | None = None,
    duration_sec: float | None = None,
    listened_sec: float | None = None,
    session_id: str | None = None,
    reason: str | None = None,
    prev_track_id: int | None = None,
    transition_weight: float | None = None,
    user_id: int | None = None,
) -> int:
    """
    action: start|finish|skip|like|dislike|progress|track_end
    If prev_track_id given and action in (start, finish, track_end), bump transition.
    user_id=None → владелец (§4.2).
    """
    action = action.lower().strip()
    if action not in ACTIONS:
        raise ValueError(f"unknown action: {action}")

    now = datetime.now(UTC)
    ts = now.isoformat()
    daypart = _daypart(now.hour)
    weekday = now.weekday()

    with connect() as conn:
        # listening_history.user_id NOT NULL; None → владелец (§4.2)
        cur = conn.execute(
            """
            INSERT INTO listening_history(
                user_id, track_id, ts, source, action, daypart, weekday,
                position_sec, duration_sec, listened_sec, session_id, reason
            ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
            """,
            (
                user_id if user_id is not None else backend.owner_id(),
                track_id,
                ts,
                source,
                action,
                daypart,
                weekday,
                position_sec,
                duration_sec,
                listened_sec,
                session_id,
                reason,
            ),
        )
        hid = int(cur.lastrowid)

        if (
            prev_track_id is not None
            and prev_track_id != track_id
            and action
            in {
                "start",
                "finish",
                "track_end",
            }
        ):
            w = float(transition_weight) if transition_weight is not None else 1.0
            conn.execute(
                """
                INSERT INTO transitions(from_id, to_id, weight, updated_at)
                VALUES (?,?,?,?)
                ON CONFLICT(from_id, to_id) DO UPDATE SET
                    weight = weight + excluded.weight,
                    updated_at = excluded.updated_at
                """,
                (prev_track_id, track_id, w, utcnow()),
            )
        return hid


def bump_rec_stats(
    track_id: int,
    *,
    shown: int = 0,
    skipped_early: int = 0,
    completed: int = 0,
    user_id: int | None = None,
) -> None:
    now = utcnow()
    with connect() as conn:
        # rec_stats → user_track_stats: PK (user_id, track_id)
        conn.execute(
            """
            INSERT INTO user_track_stats(
                user_id, track_id, shown, skipped_early, completed, updated_at
            ) VALUES (?, ?, ?, ?, ?, ?)
            ON CONFLICT(user_id, track_id) DO UPDATE SET
                shown = user_track_stats.shown + excluded.shown,
                skipped_early = user_track_stats.skipped_early + excluded.skipped_early,
                completed = user_track_stats.completed + excluded.completed,
                updated_at = excluded.updated_at
            """,
            (
                user_id if user_id is not None else backend.owner_id(),
                track_id,
                shown,
                skipped_early,
                completed,
                now,
            ),
        )


def recent_history(limit: int = 50, user_id: int | None = None) -> list[dict[str, Any]]:
    """Последние события; user_id=None → владелец."""
    uid = user_id if user_id is not None else backend.owner_id()
    with connect() as conn:
        rows = conn.execute(
            """
            SELECT h.id, h.track_id, h.ts, h.action, h.daypart, h.weekday,
                   h.listened_sec, h.duration_sec, h.reason, h.session_id,
                   t.artist, t.title
            FROM listening_history h
            JOIN tracks t ON t.id = h.track_id
            WHERE h.user_id = ?
            ORDER BY h.id DESC
            LIMIT ?
            """,
            (uid, limit),
        ).fetchall()
        return [dict(r) for r in rows]


def history_counts(user_id: int | None = None) -> dict[str, int]:
    with connect() as conn:
        rows = conn.execute(
            "SELECT action, COUNT(*) AS n FROM listening_history WHERE user_id = ? GROUP BY action",
            (user_id if user_id is not None else backend.owner_id(),),
        ).fetchall()
    out = {r["action"]: int(r["n"]) for r in rows}
    out["total"] = sum(out.values())
    return out


def top_transitions(limit: int = 20) -> list[dict[str, Any]]:
    with connect() as conn:
        rows = conn.execute(
            """
            SELECT tr.from_id, tr.to_id, tr.weight,
                   a.artist AS from_artist, a.title AS from_title,
                   b.artist AS to_artist, b.title AS to_title
            FROM transitions tr
            JOIN tracks a ON a.id = tr.from_id
            JOIN tracks b ON b.id = tr.to_id
            ORDER BY tr.weight DESC
            LIMIT ?
            """,
            (limit,),
        ).fetchall()
        return [dict(r) for r in rows]
