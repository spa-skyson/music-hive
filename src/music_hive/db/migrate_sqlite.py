"""F1.5: одношаговый импортёр SQLite → PostgreSQL (дизайн §10, GitLab #16).

Источник — легаси-файл ``music-hive.db``, читается НАПРЯМУЮ через sqlite3
(``mode=ro``: файл неприкосновенен, он и есть план отката). Цель — PG через
``MUSIC_HIVE_DATABASE_URL``; goose-схему применяет player
(``music-hive-player migrate``), импортёр DDL схемы не делает (кроме
санкционированной активации модели через ``save_embedding`` → ``_ensure_model``).

Режимы (CLI ``music-hive migrate-sqlite``):
- ``--dry-run``: счётчики источника + план переноса, без записи;
- ``--apply``:   перенос в порядке FK-зависимостей, батчами по 500;
- ``--verify``:  счётчики + контрольные суммы источник vs цель + проверка
  векторов (count/dims/L2-нормы/cosine-семпл); exit 1 при расхождении.

Идемпотентность apply (контракт, продублирован в --help):
- id треков/плейлистов/истории/impressions/tips сохраняются
  (``OVERRIDING SYSTEM VALUE``) + ``setval`` секвенций; вставки идут с
  ``ON CONFLICT DO NOTHING`` → повторный apply не создаёт дубликатов
  (повторно выполняются только UPDATE FK-ссылок треков и обложек — теми же
  значениями);
- артисты/альбомы/жанры схлопываются по естественным ключам (name_norm, …);
- для полного повторного переноса поверх «грязной» цели — ``--truncate-target``
  (очищает данные CASCADE, НЕ трогая users/sessions/api_tokens/jobs).

Не переносится:
- ``jobs`` — очередь в PG стартует чистой;
- ``scan_state['clap_model']`` — модель живёт в реестре embedding_models
  (регистрируется/активируется при переносе векторов);
- неизвестные таблицы (не из легаси-схемы) — WARN в dry-run.
"""

from __future__ import annotations

import hashlib
import logging
import os
import sqlite3
from collections.abc import Iterable, Iterator, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import numpy as np

from music_hive.config import get_settings
from music_hive.db import backend
from music_hive.db.schema import connect, utcnow
from music_hive.db.store import (
    _ensure_model,
    _upsert_album,
    _upsert_artist,
    emb_table_name,
    ensure_db,
    model_key,
    norm_name,
    save_embedding,
)

log = logging.getLogger(__name__)

BATCH = 500

# Таблицы легаси-схемы (db.schema.SCHEMA — источник истины по shape источника).
KNOWN_TABLES = (
    "tracks",
    "artists",
    "albums",
    "genres",
    "track_genres",
    "features",
    "listening_history",
    "rec_stats",
    "recommendation_impressions",
    "transitions",
    "playlists",
    "playlist_tracks",
    "feature_weights",
    "user_profile_snapshots",
    "scan_state",
    "listen_later",
    "jobs",
    "discover_tips",
    "favorites",
    "favorite_artists",
    "favorite_albums",
    "radio_shares",
    "play_sessions",
    "lyrics",
)

PLAN = {
    "tracks": "tracks (id сохраняются, FK artist/album вторым проходом)",
    "artists": "artists (upsert по name_norm, id перемапливаются)",
    "albums": "albums (upsert по (artist_id, title_norm), cover после tracks)",
    "features": "→ track_audio_features + emb_<model> (векторы через save_embedding)",
    "genres": "genres (upsert по name)",
    "track_genres": "track_genres (genre_id перемапливаются)",
    "listening_history": "listening_history (+user_id=owner, id сохраняются)",
    "rec_stats": "→ user_track_stats (+user_id)",
    "recommendation_impressions": "recommendation_impressions (+user_id, id сохраняются)",
    "transitions": "transitions (id треков сохранены)",
    "playlists": "playlists (+owner_user_id, visibility='private', id сохраняются)",
    "playlist_tracks": "playlist_tracks (PK playlist_id+position)",
    "feature_weights": "→ user_feature_weights (+user_id, model_key источника)",
    "user_profile_snapshots": "→ user_taste_profiles (+user_id, model_key; latest на context)",
    "scan_state": "scan_state (кроме clap_model — он уходит в реестр моделей)",
    "listen_later": "→ user_listen_later (+user_id)",
    "discover_tips": "discover_tips (+user_id, id сохраняются)",
    "favorites": "→ user_favorites(kind='track')",
    "favorite_artists": "→ user_favorites(kind='artist', lookup по name_norm; нет цели — skip+WARN)",
    "favorite_albums": "→ user_favorites(kind='album', lookup по (artist, title); нет цели — skip+WARN)",
    "radio_shares": "radio_shares (+user_id)",
    "play_sessions": "play_sessions (+user_id; id треков сохранены → JSON-очереди валидны)",
    "lyrics": "lyrics (глобальный кеш, как есть)",
}

SKIPPED = {
    "jobs": "очередь заданий не переносится — PG стартует с чистой очередью",
}

# REAL в PG = float4: контрольные суммы сравниваем с точностью float4 (7 знаков).
_NORM_FMT = "%.7g"


# ------------------------------------------------------------------ источник


def open_source(sqlite_path: Path) -> sqlite3.Connection:
    """Read-only соединение с легаси-базой (файл не изменяется никогда)."""
    conn = sqlite3.connect(f"{sqlite_path.resolve().as_uri()}?mode=ro", uri=True)
    conn.row_factory = sqlite3.Row
    return conn


def _src_tables(src: sqlite3.Connection) -> set[str]:
    return {
        str(r["name"])
        for r in src.execute("SELECT name FROM sqlite_master WHERE type = 'table'")
    }


def _count(src: sqlite3.Connection, table: str) -> int:
    if table not in _src_tables(src):
        return 0
    row = src.execute(f"SELECT COUNT(*) AS n FROM {table}").fetchone()
    return int(row["n"]) if row else 0


def _ts(value: Any) -> datetime | None:
    """ISO-TEXT легаси → datetime (naive трактуем как UTC)."""
    if value is None or value == "":
        return None
    if isinstance(value, datetime):
        return value if value.tzinfo else value.replace(tzinfo=UTC)
    try:
        parsed = datetime.fromisoformat(str(value))
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=UTC)


def _bool(value: Any) -> bool | None:
    return None if value is None else bool(int(value))


def _json(value: Any) -> str | None:
    """Легаси TEXT-JSON → параметр для jsonb ('' → NULL)."""
    if value is None:
        return None
    return str(value) or None


def resolve_model_key(src: sqlite3.Connection) -> tuple[str, bool]:
    """model_key модели источника: scan_state['clap_model'] → 'clap:<repo>'.

    Нет записи/таблицы → текущая из config. → (model_key, взят_из_источника).
    """
    repo: str | None = None
    if "scan_state" in _src_tables(src):
        row = src.execute(
            "SELECT value FROM scan_state WHERE key = 'clap_model'"
        ).fetchone()
        repo = str(row["value"]) if row else None
    if not repo:
        return model_key(), False
    return (repo if repo.startswith("clap:") else f"clap:{repo}"), True


@dataclass
class SourceInfo:
    path: Path
    tables: dict[str, int] = field(default_factory=dict)
    unknown_tables: list[str] = field(default_factory=list)
    model_key: str = ""
    model_from_source: bool = False
    emb_dims: dict[int, int] = field(default_factory=dict)  # dim → векторов
    emb_broken: int = 0  # длина blob != dim*4
    ready_without_vector: int = 0  # ready, а вектора нет/бит → станет pending

    @property
    def emb_total(self) -> int:
        return sum(self.emb_dims.values())


_EMB_VALID_SRC_WHERE = """
status = 'ready' AND embedding IS NOT NULL
  AND length(embedding) > 0 AND length(embedding) % 4 = 0
  AND (embedding_dim IS NULL OR embedding_dim * 4 = length(embedding))
"""


def inspect_source(sqlite_path: Path) -> SourceInfo:
    src = open_source(sqlite_path)
    try:
        info = SourceInfo(path=sqlite_path)
        present = _src_tables(src)
        known = [t for t in KNOWN_TABLES if t in present]
        info.unknown_tables = sorted(
            t for t in present if t not in KNOWN_TABLES and not t.startswith("sqlite_")
        )
        info.tables = {t: _count(src, t) for t in [*known, *info.unknown_tables]}
        info.model_key, info.model_from_source = resolve_model_key(src)
        if "features" in present:
            for row in src.execute(
                """
                SELECT embedding_dim, length(embedding) AS len, COUNT(*) AS n
                FROM features WHERE embedding IS NOT NULL
                GROUP BY embedding_dim, length(embedding)
                """
            ):
                dim, length, n = (
                    int(row["embedding_dim"] or 0),
                    int(row["len"] or 0),
                    int(row["n"]),
                )
                if length > 0 and length % 4 == 0 and dim in (0, length // 4):
                    key = dim or length // 4
                    info.emb_dims[key] = info.emb_dims.get(key, 0) + n
                else:
                    info.emb_broken += n
            ready = int(
                src.execute(
                    "SELECT COUNT(*) AS n FROM features WHERE status = 'ready'"
                ).fetchone()["n"]
            )
            ready_valid = int(
                src.execute(
                    f"SELECT COUNT(*) AS n FROM features WHERE {_EMB_VALID_SRC_WHERE}"
                ).fetchone()["n"]
            )
            info.ready_without_vector = ready - ready_valid
        return info
    finally:
        src.close()


# --------------------------------------------------------------------- план


@dataclass
class PlanRow:
    table: str
    rows: int
    target: str
    note: str = ""


def dry_run(sqlite_path: Path) -> tuple[list[PlanRow], SourceInfo]:
    """План переноса: по каждой таблице источника — цель/действие/WARN."""
    info = inspect_source(sqlite_path)
    rows: list[PlanRow] = []
    for table in KNOWN_TABLES:
        n = info.tables.get(table, 0)
        if table in SKIPPED:
            rows.append(PlanRow(table, n, "— ПРОПУСК", SKIPPED[table]))
            continue
        rows.append(PlanRow(table, n, PLAN[table]))
    if len(info.emb_dims) > 1:
        rows.append(
            PlanRow(
                "features: векторы",
                info.emb_total,
                "— ОШИБКА",
                f"несколько размерностей {dict(info.emb_dims)} — перенос невозможен",
            )
        )
    elif info.emb_total:
        dim = next(iter(info.emb_dims))
        rows.append(
            PlanRow(
                "features: векторы",
                info.emb_total,
                f"emb-таблица {emb_table_name(info.model_key)} (dim={dim}, "
                f"модель из {'scan_state' if info.model_from_source else 'config'})",
            )
        )
    if info.emb_broken:
        rows.append(
            PlanRow(
                "features: битые BLOB",
                info.emb_broken,
                "status='pending'",
                "длина blob не кратна dim*4 — воркер пересчитает",
            )
        )
    if info.ready_without_vector:
        rows.append(
            PlanRow(
                "features: ready без вектора",
                info.ready_without_vector,
                "status='pending'",
                "воркер пересчитает (дизайн §10.3)",
            )
        )
    for t in info.unknown_tables:
        rows.append(
            PlanRow(t, info.tables.get(t, 0), "— ПРОПУСК", "не входит в легаси-схему")
        )
    return rows, info


def pg_counts() -> dict[str, int] | None:
    """Текущие счётчики цели (dry-run: показать, что цель не пустая)."""
    if not backend.is_pg():
        return None
    try:
        with connect() as conn:
            return {
                t: int(conn.execute(f"SELECT COUNT(*) AS n FROM {t}").fetchone()["n"])
                for t in (
                    "tracks",
                    "artists",
                    "albums",
                    "listening_history",
                    "playlists",
                )
            }
    except Exception as exc:  # noqa: BLE001 — dry-run не падает на недоступной цели
        log.warning("счётчики цели недоступны: %s", exc)
        return None


# ------------------------------------------------------------- PG-хелперы

_TRUNCATE_TABLES = (
    "tracks",
    "albums",
    "artists",
    "genres",
    "track_genres",
    "track_audio_features",
    "embedding_models",
    "listening_history",
    "user_track_stats",
    "recommendation_impressions",
    "transitions",
    "playlists",
    "playlist_tracks",
    "user_favorites",
    "user_listen_later",
    "play_sessions",
    "radio_shares",
    "lyrics",
    "discover_tips",
    "scan_state",
    "user_feature_weights",
    "user_taste_profiles",
)
# НЕ входят: users/sessions/api_tokens (владелец не трогается), jobs (очередь),
# goose_db_version.

_SETVAL_TABLES = (
    "tracks",
    "playlists",
    "listening_history",
    "recommendation_impressions",
    "discover_tips",
)


def truncate_target(conn: Any) -> list[str]:
    """TRUNCATE данных цели (CASCADE); emb_% находятся динамически."""
    emb = [
        str(r["table_name"])
        for r in conn.execute(
            """
            SELECT table_name FROM information_schema.tables
            WHERE table_schema = 'public' AND table_name LIKE 'emb%'
            """
        ).fetchall()
    ]
    tables = [*_TRUNCATE_TABLES, *emb]
    conn.execute(f"TRUNCATE TABLE {', '.join(tables)} CASCADE")
    return tables


def _insert_many(
    conn: Any,
    prefix: str,
    n_cols: int,
    rows: Sequence[tuple],
    *,
    conflict: str = "",
    batch: int = BATCH,
) -> int:
    """Мульти-VALUES INSERT батчами (один roundtrip на батч)."""
    row_ph = "(" + ", ".join(["%s"] * n_cols) + ")"
    done = 0
    for i in range(0, len(rows), batch):
        chunk = rows[i : i + batch]
        sql = f"{prefix} VALUES {', '.join([row_ph] * len(chunk))}"
        if conflict:
            sql = f"{sql} {conflict}"
        conn.execute(sql, [v for row in chunk for v in row])
        done += len(chunk)
    return done


def _setval(conn: Any, table: str) -> None:
    """Секвенция за max(id) после вставок с OVERRIDING SYSTEM VALUE."""
    conn.execute(
        f"""
        SELECT setval(pg_get_serial_sequence('{table}', 'id'),
                      COALESCE((SELECT MAX(id) FROM {table}), 0) + 1, false)
        """
    )


def _iter_src(
    src: sqlite3.Connection, table: str, where: str = "", order: str = ""
) -> Iterator[sqlite3.Row]:
    if table not in _src_tables(src):
        return
    sql = f"SELECT * FROM {table}"
    if where:
        sql += f" WHERE {where}"
    if order:
        sql += f" ORDER BY {order}"
    yield from src.execute(sql)


def _valid_blobs(src: sqlite3.Connection) -> Iterator[tuple[int, np.ndarray]]:
    """Готовые векторы источника: (track_id, float32-массив)."""
    if "features" not in _src_tables(src):
        return
    for r in src.execute(
        f"SELECT track_id, embedding, embedding_dim FROM features WHERE {_EMB_VALID_SRC_WHERE}"
    ):
        blob = r["embedding"]
        yield int(r["track_id"]), np.frombuffer(blob, dtype=np.float32)


# ------------------------------------------------------------------- apply


def apply_migration(sqlite_path: Path, *, truncate: bool = False) -> dict[str, Any]:
    """Перенос SQLite → PG. Идемпотентен (см. docstring модуля)."""
    if not backend.is_pg():
        raise SystemExit(
            "MUSIC_HIVE_DATABASE_URL не задан — apply работает только в PG-режиме"
        )
    ensure_db()  # goose-гейт + owner-bootstrap (существующего владельца не меняет)
    owner = backend.owner_id()
    src = open_source(sqlite_path)
    try:
        info = inspect_source(sqlite_path)
        if len(info.emb_dims) > 1:
            raise SystemExit(
                f"в источнике несколько размерностей векторов {dict(info.emb_dims)} — "
                "перенос невозможен (оставьте одну модель)"
            )
        stats: dict[str, Any] = {
            "warnings": [],
            "owner_user_id": owner,
            "model_key": info.model_key,
            "model_from_source": info.model_from_source,
        }
        if truncate:
            with connect() as conn:
                stats["truncated"] = truncate_target(conn)

        # save_embedding берёт model_key из config → на время переноса
        # фиксируем в env модель источника, в finally возвращаем как было.
        env_changed = False
        old_env = os.environ.get("MUSIC_HIVE_CLAP_MODEL")
        try:
            if info.model_key != model_key():
                repo = info.model_key.split(":", 1)[-1]
                os.environ["MUSIC_HIVE_CLAP_MODEL"] = repo
                get_settings.cache_clear()
                env_changed = True

            with connect() as conn:
                _apply_artists_albums(conn, src, stats)
                _apply_tracks(conn, src, stats)
                _apply_features(conn, src, stats)
            _apply_embeddings(src, stats)
            with connect() as conn:
                _apply_genres(conn, src, stats)
                _apply_user_scoped(conn, src, owner, stats)
                for t in _SETVAL_TABLES:
                    _setval(conn, t)
                conn.execute("ANALYZE")
        finally:
            if env_changed:
                if old_env is None:
                    os.environ.pop("MUSIC_HIVE_CLAP_MODEL", None)
                else:
                    os.environ["MUSIC_HIVE_CLAP_MODEL"] = old_env
                get_settings.cache_clear()
        return stats
    finally:
        src.close()


def _apply_artists_albums(conn: Any, src: sqlite3.Connection, stats: dict) -> None:
    """artists/albums через store-upsert'ы (name_norm / (artist_id, title_norm))."""
    artist_map: dict[int, int] = {}
    a_created = a_reused = 0
    for row in _iter_src(src, "artists", order="id"):
        pg_id, created = _upsert_artist(conn, row["name"], mbid=row["mbid"])
        artist_map[int(row["id"])] = pg_id
        a_created += int(created)
        a_reused += int(not created)

    album_map: dict[int, int] = {}
    al_created = al_reused = 0
    covers: list[tuple[int, int]] = []
    for row in _iter_src(src, "albums", order="id"):
        pg_artist = artist_map.get(int(row["artist_id"]))
        if pg_artist is None:
            stats["warnings"].append(
                f"albums: артист id={row['artist_id']} не найден — альбом "
                f"«{row['title']}» пропущен"
            )
            continue
        pg_id, created = _upsert_album(
            conn, pg_artist, row["title"], year=row["year"], mbid=row["mbid"]
        )
        album_map[int(row["id"])] = pg_id
        al_created += int(created)
        al_reused += int(not created)
        if row["cover_track_id"] is not None:
            covers.append((pg_id, int(row["cover_track_id"])))
    stats["artists"] = {"created": a_created, "reused": a_reused}
    stats["albums"] = {"created": al_created, "reused": al_reused}
    # для следующих этапов (в _apply_tracks)
    stats["_artist_map"] = artist_map
    stats["_album_map"] = album_map
    stats["_covers"] = covers


def _apply_tracks(conn: Any, src: sqlite3.Connection, stats: dict) -> None:
    artist_map: dict[int, int] = stats.pop("_artist_map")
    album_map: dict[int, int] = stats["_album_map"]

    rows = [
        (
            int(r["id"]),
            r["path"],
            r["file_md5"] or "",
            r["file_mtime"],
            r["file_size"],
            r["title"] or "",
            r["artist"] or "",
            r["album"] or "",
            r["year"],
            r["track_number"],
            r["duration"],
            r["bitrate"],
            r["sample_rate"],
            r["channels"],
            r["fingerprint"],
            r["lufs"],
            _bool(r["is_active"]),
            r["artwork_path"],
            _ts(r["created_at"]) or _ts(utcnow()),
            _ts(r["updated_at"]) or _ts(utcnow()),
        )
        for r in _iter_src(src, "tracks", order="id")
    ]
    inserted = _insert_many(
        conn,
        """
        INSERT INTO tracks (id, path, file_md5, file_mtime, file_size, title,
            artist, album, year, track_number, duration, bitrate, sample_rate,
            channels, fingerprint, lufs, is_active, artwork_path, created_at,
            updated_at)
        OVERRIDING SYSTEM VALUE
        """,
        20,
        rows,
        conflict="ON CONFLICT (path) DO NOTHING",
    )

    # is_duplicate_of — само-FK: только когда все строки уже вставлены
    dups = [
        (int(r["id"]), int(r["is_duplicate_of"]))
        for r in src.execute(
            "SELECT id, is_duplicate_of FROM tracks WHERE is_duplicate_of IS NOT NULL"
        )
    ]
    for i in range(0, len(dups), BATCH):
        chunk = dups[i : i + BATCH]
        values = ", ".join(["(%s::bigint, %s::bigint)"] * len(chunk))
        conn.execute(
            f"""
            UPDATE tracks t SET is_duplicate_of = v.dup
            FROM (VALUES {values}) v(id, dup)
            WHERE t.id = v.id AND EXISTS (SELECT 1 FROM tracks d WHERE d.id = v.dup)
            """,
            [v for pair in chunk for v in pair],
        )

    # FK artist/album: перемапливание готовых id; для треков без FK (базы до
    # F1.2) — derivation из строковых полей, как в artists_backfill.
    updates: list[tuple[int, int | None, int | None, int | None]] = []
    derived = 0
    for r in src.execute(
        "SELECT id, artist, album, year, artist_id, album_artist_id, album_id FROM tracks"
    ):
        tid = int(r["id"])
        if (
            r["artist_id"] is not None
            or r["album_artist_id"] is not None
            or r["album_id"] is not None
        ):
            updates.append(
                (
                    tid,
                    artist_map.get(int(r["artist_id"]))
                    if r["artist_id"] is not None
                    else None,
                    artist_map.get(int(r["album_artist_id"]))
                    if r["album_artist_id"] is not None
                    else None,
                    album_map.get(int(r["album_id"]))
                    if r["album_id"] is not None
                    else None,
                )
            )
            continue
        artist = (r["artist"] or "").strip()
        album = (r["album"] or "").strip()
        artist_id = album_artist_id = album_id = None
        if artist:
            artist_id, _ = _upsert_artist(conn, artist)
            album_artist_id = artist_id
        if album and album_artist_id is not None:
            album_id, _ = _upsert_album(conn, album_artist_id, album, year=r["year"])
        if artist_id is not None or album_id is not None:
            updates.append((tid, artist_id, album_artist_id, album_id))
            derived += 1
    for i in range(0, len(updates), BATCH):
        chunk = updates[i : i + BATCH]
        values = ", ".join(
            ["(%s::bigint, %s::bigint, %s::bigint, %s::bigint)"] * len(chunk)
        )
        conn.execute(
            f"""
            UPDATE tracks t
            SET artist_id = v.a, album_artist_id = v.aa, album_id = v.al
            FROM (VALUES {values}) v(id, a, aa, al)
            WHERE t.id = v.id
            """,
            [v for row in chunk for v in row],
        )

    # обложки альбомов (tracks уже на месте, id треков сохранены)
    covers: list[tuple[int, int]] = stats.pop("_covers")
    for album_id, cover in covers:
        conn.execute(
            """
            UPDATE albums SET cover_track_id = %s WHERE id = %s
              AND EXISTS (SELECT 1 FROM tracks WHERE id = %s)
            """,
            (cover, album_id, cover),
        )
    stats.pop("_album_map")
    stats["tracks"] = {
        "source": len(rows),
        "inserted": inserted,
        "fk_linked": len(updates) - derived,
        "fk_derived": derived,
        "covers_set": len(covers),
        "duplicates_linked": len(dups),
    }


def _apply_features(conn: Any, src: sqlite3.Connection, stats: dict) -> None:
    """features → track_audio_features. Векторы — отдельным этапом."""
    rows: list[tuple] = []
    demoted = 0
    for r in _iter_src(src, "features"):
        blob, dim = r["embedding"], int(r["embedding_dim"] or 0)
        if blob is not None and len(blob) > 0 and len(blob) % 4 == 0:
            dim = dim or len(blob) // 4
        valid = blob is not None and dim > 0 and len(blob) == dim * 4
        status = r["status"] or "pending"
        if status == "ready" and not valid:
            status = "pending"
            demoted += 1
        rows.append(
            (
                int(r["track_id"]),
                r["bpm"],
                r["key_name"],
                r["mode"],
                r["lufs"],
                r["cluster_id"],
                status,
                r["error"],
                _ts(r["computed_at"]),
            )
        )
    inserted = _insert_many(
        conn,
        """
        INSERT INTO track_audio_features (track_id, bpm, key_name, mode, lufs,
            cluster_id, status, error, computed_at)
        """,
        9,
        rows,
        conflict="ON CONFLICT (track_id) DO NOTHING",
    )
    stats["track_audio_features"] = {
        "source": len(rows),
        "inserted": inserted,
        "demoted_to_pending": demoted,
    }


def _apply_embeddings(src: sqlite3.Connection, stats: dict) -> None:
    """Векторы — через store.save_embedding: реестр + активация + валидация."""
    saved = demoted = 0
    for tid, vec in _valid_blobs(src):
        try:
            save_embedding(tid, vec)
            saved += 1
        except ValueError as exc:
            # NaN/нулевая норма: pending, воркер пересчитает
            with connect() as conn:
                conn.execute(
                    """
                    UPDATE track_audio_features
                    SET status = 'pending', error = %s
                    WHERE track_id = %s
                    """,
                    (f"migrate: {exc}"[:2000], tid),
                )
            demoted += 1
            stats["warnings"].append(f"emb: track {tid} — {exc}")
    stats["embeddings"] = {"saved": saved, "demoted": demoted}
    with connect() as conn:
        active = conn.execute(
            "SELECT model_key FROM embedding_models WHERE is_active"
        ).fetchone()
    if active is not None and str(active["model_key"]) != stats["model_key"]:
        stats["warnings"].append(
            f"активной остаётся модель {active['model_key']}; переключение на "
            f"{stats['model_key']} — job model_activate (F4)"
        )


def _apply_genres(conn: Any, src: sqlite3.Connection, stats: dict) -> None:
    legacy = {
        int(r["id"]): str(r["name"]) for r in _iter_src(src, "genres", order="id")
    }
    _insert_many(
        conn,
        "INSERT INTO genres (name)",
        1,
        [(n,) for n in legacy.values()],
        conflict="ON CONFLICT (name) DO NOTHING",
    )
    pg_genres = {
        str(r["name"]): int(r["id"])
        for r in conn.execute("SELECT id, name FROM genres").fetchall()
    }
    mapping = {lid: pg_genres[n] for lid, n in legacy.items() if n in pg_genres}
    rows = [
        (int(r["track_id"]), mapping[int(r["genre_id"])])
        for r in _iter_src(src, "track_genres")
        if int(r["genre_id"]) in mapping
    ]
    linked = _insert_many(
        conn,
        "INSERT INTO track_genres (track_id, genre_id)",
        2,
        rows,
        conflict="ON CONFLICT DO NOTHING",
    )
    stats["genres"] = {"genres": len(legacy), "track_genres": linked}


_FAV_INSERT = """
INSERT INTO user_favorites (user_id, kind, track_id, artist_id, album_id,
    position, created_at)
"""


def _apply_user_scoped(
    conn: Any, src: sqlite3.Connection, owner: int, stats: dict
) -> None:
    """Всё остальное: user-scoped таблицы + глобальные кеши/состояния."""
    ins = _insert_many

    rows = [
        (
            int(r["id"]),
            owner,
            int(r["track_id"]),
            _ts(r["ts"]),
            r["source"],
            r["action"],
            r["daypart"],
            r["weekday"],
            r["position_sec"],
            r["duration_sec"],
            r["listened_sec"],
            r["session_id"],
            r["reason"],
        )
        for r in _iter_src(src, "listening_history", order="id")
    ]
    ins(
        conn,
        """
        INSERT INTO listening_history (id, user_id, track_id, ts, source, action,
            daypart, weekday, position_sec, duration_sec, listened_sec,
            session_id, reason)
        OVERRIDING SYSTEM VALUE
        """,
        13,
        rows,
        conflict="ON CONFLICT (id) DO NOTHING",
    )
    stats["listening_history"] = len(rows)

    rows = [
        (
            owner,
            int(r["track_id"]),
            int(r["shown"] or 0),
            int(r["skipped_early"] or 0),
            int(r["completed"] or 0),
            _ts(r["updated_at"]),
        )
        for r in _iter_src(src, "rec_stats")
    ]
    ins(
        conn,
        """
        INSERT INTO user_track_stats (user_id, track_id, shown, skipped_early,
            completed, updated_at)
        """,
        6,
        rows,
        conflict="ON CONFLICT (user_id, track_id) DO NOTHING",
    )
    stats["user_track_stats"] = len(rows)

    rows = [
        (
            int(r["id"]),
            owner,
            r["session_id"],
            int(r["track_id"]),
            int(r["position"]),
            float(r["score"] or 0),
            float(r["cosine_taste"] or 0),
            float(r["cosine_current"] or 0),
            int(r["explore"] or 0),
            int(r["new_boost"] or 0),
            r["maturity"] or "",
            r["mode"] or "",
            _ts(r["shown_at"]),
        )
        for r in _iter_src(src, "recommendation_impressions", order="id")
    ]
    ins(
        conn,
        """
        INSERT INTO recommendation_impressions (id, user_id, session_id, track_id,
            position, score, cosine_taste, cosine_current, explore, new_boost,
            maturity, mode, shown_at)
        OVERRIDING SYSTEM VALUE
        """,
        13,
        rows,
        conflict="ON CONFLICT (id) DO NOTHING",
    )
    stats["recommendation_impressions"] = len(rows)

    rows = [
        (
            int(r["from_id"]),
            int(r["to_id"]),
            float(r["weight"] or 1.0),
            _ts(r["updated_at"]),
        )
        for r in _iter_src(src, "transitions")
    ]
    ins(
        conn,
        "INSERT INTO transitions (from_id, to_id, weight, updated_at)",
        4,
        rows,
        conflict="ON CONFLICT (from_id, to_id) DO NOTHING",
    )
    stats["transitions"] = len(rows)

    rows = [
        (
            int(r["id"]),
            owner,
            r["kind"],
            r["name"],
            "private",
            _ts(r["created_at"]),
            _json(r["meta_json"]),
        )
        for r in _iter_src(src, "playlists", order="id")
    ]
    ins(
        conn,
        """
        INSERT INTO playlists (id, owner_user_id, kind, name, visibility,
            created_at, meta_json)
        OVERRIDING SYSTEM VALUE
        """,
        7,
        rows,
        conflict="ON CONFLICT (id) DO NOTHING",
    )
    stats["playlists"] = len(rows)

    rows = [
        (
            int(r["playlist_id"]),
            int(r["position"]),
            int(r["track_id"]),
            r["explanation"],
        )
        for r in _iter_src(src, "playlist_tracks")
    ]
    ins(
        conn,
        "INSERT INTO playlist_tracks (playlist_id, position, track_id, explanation)",
        4,
        rows,
        conflict="ON CONFLICT (playlist_id, position) DO NOTHING",
    )
    stats["playlist_tracks"] = len(rows)

    rows = [
        (
            owner,
            "track",
            int(r["track_id"]),
            None,
            None,
            int(r["position"] or 0),
            _ts(r["added_at"]),
        )
        for r in _iter_src(src, "favorites")
    ]
    ins(conn, _FAV_INSERT, 7, rows, conflict="ON CONFLICT DO NOTHING")
    stats["favorites_tracks"] = len(rows)

    # lookup-таблицы для artist/album-избранного (дизайн §10.3: исчезнувшие — skip+WARN)
    artist_ids = {
        str(r["name_norm"]): int(r["id"])
        for r in conn.execute("SELECT id, name_norm FROM artists").fetchall()
    }
    album_ids = {
        (str(r["name_norm"]), str(r["title_norm"])): int(r["id"])
        for r in conn.execute(
            """
            SELECT al.id, ar.name_norm, al.title_norm FROM albums al
            JOIN artists ar ON ar.id = al.artist_id
            """
        ).fetchall()
    }
    for table, kind in (("favorite_artists", "artist"), ("favorite_albums", "album")):
        rows, skipped = [], 0
        for r in _iter_src(src, table):
            if kind == "artist":
                aid = artist_ids.get(norm_name(str(r["artist"] or "")))
                row = (
                    owner,
                    "artist",
                    None,
                    aid,
                    None,
                    int(r["position"] or 0),
                    _ts(r["added_at"]),
                )
                label = f"«{r['artist']}»"
            else:
                aid = album_ids.get(
                    (
                        norm_name(str(r["artist"] or "")),
                        norm_name(str(r["album"] or "")),
                    )
                )
                row = (
                    owner,
                    "album",
                    None,
                    None,
                    aid,
                    int(r["position"] or 0),
                    _ts(r["added_at"]),
                )
                label = f"«{r['artist']} / {r['album']}»"
            if aid is None:
                skipped += 1
                stats["warnings"].append(
                    f"{table}: {label} не найден в каталоге — skip"
                )
                continue
            rows.append(row)
        ins(conn, _FAV_INSERT, 7, rows, conflict="ON CONFLICT DO NOTHING")
        stats[f"favorites_{kind}s"] = {"migrated": len(rows), "skipped": skipped}

    rows = [
        (owner, int(r["track_id"]), _ts(r["added_at"]), int(r["position"] or 0))
        for r in _iter_src(src, "listen_later")
    ]
    ins(
        conn,
        "INSERT INTO user_listen_later (user_id, track_id, added_at, position)",
        4,
        rows,
        conflict="ON CONFLICT (user_id, track_id) DO NOTHING",
    )
    stats["user_listen_later"] = len(rows)

    rows = [
        (
            int(r["track_id"]),
            r["plain_lyrics"] or "",
            r["synced_lyrics"] or "",
            r["source"] or "",
            r["source_id"] or "",
            _bool(r["instrumental"]),
            r["status"] or "pending",
            r["error"],
            _ts(r["updated_at"]),
        )
        for r in _iter_src(src, "lyrics")
    ]
    ins(
        conn,
        """
        INSERT INTO lyrics (track_id, plain_lyrics, synced_lyrics, source,
            source_id, instrumental, status, error, updated_at)
        """,
        9,
        rows,
        conflict="ON CONFLICT (track_id) DO NOTHING",
    )
    stats["lyrics"] = len(rows)

    key = str(stats["model_key"])
    snapshots = list(
        _iter_src(
            src,
            "user_profile_snapshots",
            order="context DESC, created_at DESC, id DESC",
        )
    )
    if snapshots:
        dim: int | None = None
        rows = []
        for r in snapshots:
            blob = r["embedding"]
            if dim is None and blob:
                dim = len(blob) // 4 or None
            rows.append(
                (
                    owner,
                    r["context"],
                    key,
                    np.frombuffer(blob, dtype=np.float32) if blob else None,
                    _ts(r["created_at"]),
                )
            )
        if dim is not None:
            _ensure_model(conn, key, int(dim))  # FK model_key должен существовать
        ins(
            conn,
            """
            INSERT INTO user_taste_profiles (user_id, context, model_key, vec,
                updated_at)
            """,
            5,
            rows,
            conflict="ON CONFLICT (user_id, context, model_key) DO NOTHING",
        )
    stats["user_taste_profiles"] = len({r["context"] for r in snapshots})

    rows = [
        (owner, key, r["week_key"], int(r["dim"]), float(r["weight"]))
        for r in _iter_src(src, "feature_weights")
    ]
    if rows:
        registered = conn.execute(
            "SELECT 1 FROM embedding_models WHERE model_key = %s", (key,)
        ).fetchone()
        if registered is None:
            stats["warnings"].append(
                f"feature_weights: модель {key} не зарегистрирована (нет векторов) — skip"
            )
        else:
            ins(
                conn,
                """
                INSERT INTO user_feature_weights (user_id, model_key, week_key, dim, weight)
                """,
                5,
                rows,
                conflict="ON CONFLICT DO NOTHING",
            )
    stats["user_feature_weights"] = len(rows)

    rows = [
        (
            int(r["id"]),
            owner,
            r["kind"],
            r["artist"],
            r["album"],
            float(r["score"] or 0),
            _json(r["track_ids_json"]) or "[]",
            r["explanation"],
            _ts(r["created_at"]),
        )
        for r in _iter_src(src, "discover_tips", order="id")
    ]
    ins(
        conn,
        """
        INSERT INTO discover_tips (id, user_id, kind, artist, album, score,
            track_ids_json, explanation, created_at)
        OVERRIDING SYSTEM VALUE
        """,
        9,
        rows,
        conflict="ON CONFLICT (id) DO NOTHING",
    )
    stats["discover_tips"] = len(rows)

    rows = [
        (str(r["key"]), str(r["value"]))
        for r in _iter_src(src, "scan_state")
        if r["key"] != "clap_model"
    ]
    ins(
        conn,
        "INSERT INTO scan_state (key, value)",
        2,
        rows,
        conflict="ON CONFLICT (key) DO NOTHING",
    )
    stats["scan_state"] = len(rows)

    rows = [
        (
            r["token"],
            owner,
            r["name"] or "",
            _ts(r["created_at"]),
            _ts(r["revoked_at"]),
            _ts(r["last_listen_at"]),
            int(r["listen_count"] or 0),
        )
        for r in _iter_src(src, "radio_shares")
    ]
    ins(
        conn,
        """
        INSERT INTO radio_shares (token, user_id, name, created_at, revoked_at,
            last_listen_at, listen_count)
        """,
        7,
        rows,
        conflict="ON CONFLICT (token) DO NOTHING",
    )
    stats["radio_shares"] = len(rows)

    rows = [
        (
            r["id"],
            owner,
            r["mode"] or "",
            int(r["current_id"] or 0),
            _json(r["queue_json"]),
            _json(r["exclude_json"]),
            _json(r["rated_json"]),
            _json(r["daily_ids_json"]),
            int(r["daily_pos"] or 0),
            r["playlist_name"] or "",
            r["playlist_kind"] or "",
            _ts(r["updated_at"]),
        )
        for r in _iter_src(src, "play_sessions")
    ]
    ins(
        conn,
        """
        INSERT INTO play_sessions (id, user_id, mode, current_id, queue_json,
            exclude_json, rated_json, daily_ids_json, daily_pos, playlist_name,
            playlist_kind, updated_at)
        """,
        12,
        rows,
        conflict="ON CONFLICT (id) DO NOTHING",
    )
    stats["play_sessions"] = len(rows)


# ------------------------------------------------------------------- verify


def _norm(value: Any) -> str:
    """Приведение значения sqlite/PG к сравнимой строке.

    REAL в PG — float4 (7 значащих цифр), в sqlite — float8: сравниваем
    с точностью float4, иначе честные различия форматов дадут ложные FAIL.
    """
    if value is None:
        return "∅"
    if isinstance(value, np.ndarray):
        return hashlib.md5(np.asarray(value, dtype=np.float32).tobytes()).hexdigest()
    if value == "":
        return "∅"
    if isinstance(value, datetime):
        return value.isoformat()
    if isinstance(value, bool):
        return str(int(value))
    if isinstance(value, (bytes, bytearray, memoryview)):
        return hashlib.md5(bytes(value)).hexdigest()
    if isinstance(value, float):
        return _NORM_FMT % value
    if isinstance(value, str):
        parsed = _ts(value)
        return parsed.isoformat() if parsed is not None else value
    if isinstance(value, int):
        return _NORM_FMT % value
    return str(value)


def _checksum(rows: Iterable[tuple]) -> str:
    h = hashlib.md5()
    for line in sorted("\x1f".join(_norm(v) for v in row) for row in rows):
        h.update(line.encode("utf-8", "replace"))
        h.update(b"\x1e")
    return h.hexdigest()


@dataclass(frozen=True)
class Check:
    name: str
    kind: str  # 'count' | 'md5'
    table: str  # исходная таблица для guard'а «таблицы нет»
    src: str
    tgt: str = ""  # пусто → SQL совпадает
    owner: bool = False  # tgt-запрос принимает owner_id параметром


_COUNT_CHECKS = (
    Check("tracks", "count", "tracks", "SELECT COUNT(*) FROM tracks"),
    Check("genres", "count", "genres", "SELECT COUNT(*) FROM genres"),
    Check("track_genres", "count", "track_genres", "SELECT COUNT(*) FROM track_genres"),
    Check(
        "features → track_audio_features",
        "count",
        "features",
        "SELECT COUNT(*) FROM features",
        "SELECT COUNT(*) FROM track_audio_features",
    ),
    Check(
        "listening_history",
        "count",
        "listening_history",
        "SELECT COUNT(*) FROM listening_history",
    ),
    Check(
        "rec_stats → user_track_stats",
        "count",
        "rec_stats",
        "SELECT COUNT(*) FROM rec_stats",
        "SELECT COUNT(*) FROM user_track_stats WHERE user_id = %s",
        owner=True,
    ),
    Check(
        "recommendation_impressions",
        "count",
        "recommendation_impressions",
        "SELECT COUNT(*) FROM recommendation_impressions",
        "SELECT COUNT(*) FROM recommendation_impressions WHERE user_id = %s",
        owner=True,
    ),
    Check("transitions", "count", "transitions", "SELECT COUNT(*) FROM transitions"),
    Check(
        "playlists",
        "count",
        "playlists",
        "SELECT COUNT(*) FROM playlists",
        "SELECT COUNT(*) FROM playlists WHERE owner_user_id = %s",
        owner=True,
    ),
    Check(
        "playlist_tracks",
        "count",
        "playlist_tracks",
        "SELECT COUNT(*) FROM playlist_tracks",
        """
        SELECT COUNT(*) FROM playlist_tracks pt
        JOIN playlists p ON p.id = pt.playlist_id WHERE p.owner_user_id = %s
        """,
        owner=True,
    ),
    Check(
        "favorites → user_favorites(track)",
        "count",
        "favorites",
        "SELECT COUNT(*) FROM favorites",
        "SELECT COUNT(*) FROM user_favorites WHERE user_id = %s AND kind = 'track'",
        owner=True,
    ),
    Check(
        "listen_later → user_listen_later",
        "count",
        "listen_later",
        "SELECT COUNT(*) FROM listen_later",
        "SELECT COUNT(*) FROM user_listen_later WHERE user_id = %s",
        owner=True,
    ),
    Check("lyrics", "count", "lyrics", "SELECT COUNT(*) FROM lyrics"),
    Check(
        "user_profile_snapshots → user_taste_profiles",
        "count",
        "user_profile_snapshots",
        "SELECT COUNT(DISTINCT context) FROM user_profile_snapshots",
        "SELECT COUNT(*) FROM user_taste_profiles WHERE user_id = %s",
        owner=True,
    ),
    Check(
        "feature_weights → user_feature_weights",
        "count",
        "feature_weights",
        "SELECT COUNT(*) FROM feature_weights",
        "SELECT COUNT(*) FROM user_feature_weights WHERE user_id = %s",
        owner=True,
    ),
    Check(
        "scan_state (без clap_model)",
        "count",
        "scan_state",
        "SELECT COUNT(*) FROM scan_state WHERE key != 'clap_model'",
    ),
    Check(
        "discover_tips",
        "count",
        "discover_tips",
        "SELECT COUNT(*) FROM discover_tips",
        "SELECT COUNT(*) FROM discover_tips WHERE user_id = %s",
        owner=True,
    ),
    Check(
        "radio_shares",
        "count",
        "radio_shares",
        "SELECT COUNT(*) FROM radio_shares",
        "SELECT COUNT(*) FROM radio_shares WHERE user_id = %s",
        owner=True,
    ),
    Check(
        "play_sessions",
        "count",
        "play_sessions",
        "SELECT COUNT(*) FROM play_sessions",
        "SELECT COUNT(*) FROM play_sessions WHERE user_id = %s",
        owner=True,
    ),
)

_MD5_CHECKS = (
    Check(
        "tracks", "md5", "tracks", "SELECT path, file_md5, duration, title FROM tracks"
    ),
    Check("genres", "md5", "genres", "SELECT name FROM genres"),
    Check(
        "track_genres",
        "md5",
        "track_genres",
        """
        SELECT tg.track_id, g.name FROM track_genres tg
        JOIN genres g ON g.id = tg.genre_id
        """,
    ),
    Check(
        "track_audio_features",
        "md5",
        "features",
        "SELECT track_id, bpm, key_name, mode, lufs, cluster_id FROM features",
        "SELECT track_id, bpm, key_name, mode, lufs, cluster_id FROM track_audio_features",
    ),
    Check(
        "listening_history",
        "md5",
        "listening_history",
        """
        SELECT track_id, ts, action, source, position_sec, listened_sec,
               session_id, reason
        FROM listening_history
        """,
    ),
    Check(
        "user_track_stats",
        "md5",
        "rec_stats",
        "SELECT track_id, shown, skipped_early, completed FROM rec_stats",
        "SELECT track_id, shown, skipped_early, completed FROM user_track_stats WHERE user_id = %s",
        owner=True,
    ),
    Check(
        "recommendation_impressions",
        "md5",
        "recommendation_impressions",
        "SELECT session_id, track_id, position, score, shown_at FROM recommendation_impressions",
        """
        SELECT session_id, track_id, position, score, shown_at
        FROM recommendation_impressions WHERE user_id = %s
        """,
        owner=True,
    ),
    Check(
        "transitions",
        "md5",
        "transitions",
        "SELECT from_id, to_id, weight FROM transitions",
    ),
    Check(
        "playlists", "md5", "playlists", "SELECT kind, name, created_at FROM playlists"
    ),
    Check(
        "playlist_tracks",
        "md5",
        "playlist_tracks",
        "SELECT playlist_id, position, track_id FROM playlist_tracks",
    ),
    Check(
        "favorites(track)",
        "md5",
        "favorites",
        "SELECT track_id, position FROM favorites",
        "SELECT track_id, position FROM user_favorites WHERE user_id = %s AND kind = 'track'",
        owner=True,
    ),
    Check(
        "user_listen_later",
        "md5",
        "listen_later",
        "SELECT track_id, position FROM listen_later",
        "SELECT track_id, position FROM user_listen_later WHERE user_id = %s",
        owner=True,
    ),
    Check(
        "lyrics",
        "md5",
        "lyrics",
        "SELECT track_id, source, source_id, status, instrumental FROM lyrics",
    ),
    Check(
        "user_feature_weights",
        "md5",
        "feature_weights",
        "SELECT week_key, dim, weight FROM feature_weights",
        "SELECT week_key, dim, weight FROM user_feature_weights WHERE user_id = %s",
        owner=True,
    ),
    Check(
        "user_taste_profiles (latest/context)",
        "md5",
        "user_profile_snapshots",
        """
        SELECT context, embedding FROM user_profile_snapshots s
        WHERE id = (SELECT MAX(id) FROM user_profile_snapshots s2
                    WHERE s2.context = s.context)
        """,
        "SELECT context, vec FROM user_taste_profiles WHERE user_id = %s",
        owner=True,
    ),
    Check(
        "discover_tips",
        "md5",
        "discover_tips",
        "SELECT kind, artist, album, score, created_at FROM discover_tips",
        "SELECT kind, artist, album, score, created_at FROM discover_tips WHERE user_id = %s",
        owner=True,
    ),
    Check(
        "scan_state",
        "md5",
        "scan_state",
        "SELECT key, value FROM scan_state WHERE key != 'clap_model'",
    ),
    Check(
        "radio_shares",
        "md5",
        "radio_shares",
        "SELECT token, name, listen_count FROM radio_shares",
        "SELECT token, name, listen_count FROM radio_shares WHERE user_id = %s",
        owner=True,
    ),
    Check(
        "play_sessions",
        "md5",
        "play_sessions",
        "SELECT id, mode, current_id, daily_pos FROM play_sessions",
    ),
)


@dataclass
class VerifyRow:
    name: str
    src: str
    tgt: str
    ok: bool


def _verify_vectors(
    src: sqlite3.Connection, conn: Any, key: str, rows: list[VerifyRow]
) -> None:
    """Векторы: count, dims, L2-нормы ∈ [0.98, 1.02], md5 путей, cosine-семпл."""
    table = emb_table_name(key)
    # переносимы только конечные ненулевые векторы (NaN/нулевая норма → pending
    # в apply через save_embedding) — источник считаем по тем же критериям
    src_vecs = [
        (tid, v)
        for tid, v in _valid_blobs(src)
        if np.all(np.isfinite(v)) and float(np.linalg.norm(v)) > 1e-12
    ]
    src_count = len(src_vecs)
    dims = {int(v.size) for _, v in src_vecs}
    paths = {
        int(r["id"]): str(r["path"])
        for r in src.execute("SELECT id, path FROM tracks").fetchall()
    }
    src_paths = sorted(paths[tid] for tid, _ in src_vecs if tid in paths)
    src_md5 = hashlib.md5("\x1e".join(src_paths).encode()).hexdigest()
    try:
        tgt_count = int(
            conn.execute(f"SELECT COUNT(*) AS n FROM {table}").fetchone()["n"]
        )
        tgt_paths = sorted(
            str(r["path"])
            for r in conn.execute(
                f"""
                SELECT t.path FROM {table} e JOIN tracks t ON t.id = e.track_id
                WHERE e.status = 'ready'
                """
            ).fetchall()
        )
        tgt_md5 = hashlib.md5("\x1e".join(tgt_paths).encode()).hexdigest()
        tgt_dims: set[int] = set()
        norm_min, norm_max, norm_bad = float("inf"), 0.0, 0
        last = -1  # keyset-пагинация по PK track_id: без fetchall на всю таблицу
        while True:
            page = conn.execute(
                f"SELECT track_id, embedding FROM {table} WHERE track_id > %s ORDER BY track_id LIMIT 1000",
                (last,),
            ).fetchall()
            if not page:
                break
            last = int(page[-1]["track_id"])
            for row in page:
                vec = row["embedding"]
                if vec is None:
                    norm_bad += 1
                    continue
                arr = np.asarray(vec, dtype=np.float32).reshape(-1)
                tgt_dims.add(int(arr.size))
                norm = float(np.linalg.norm(arr))
                norm_min = min(norm_min, norm)
                norm_max = max(norm_max, norm)
                if not 0.98 <= norm <= 1.02:
                    norm_bad += 1
    except Exception as exc:  # noqa: BLE001 — нет emb-таблицы = FAIL, не крэш
        rows.append(VerifyRow(f"emb: {table}", str(src_count), f"ошибка: {exc}", False))
        return

    rows.append(
        VerifyRow("emb: count", str(src_count), str(tgt_count), src_count == tgt_count)
    )
    rows.append(
        VerifyRow(
            "emb: dims",
            str(sorted(dims) or "∅"),
            str(sorted(tgt_dims) or "∅"),
            dims == tgt_dims,
        )
    )
    rows.append(
        VerifyRow(
            "emb: L2-нормы ∈ [0.98, 1.02]",
            f"{src_count} шт",
            f"min={norm_min:.4f} max={norm_max:.4f} bad={norm_bad}"
            if tgt_count
            else "∅",
            norm_bad == 0,
        )
    )
    rows.append(VerifyRow("emb: md5 путей", src_md5, tgt_md5, src_md5 == tgt_md5))

    sample_bad = sample_n = 0
    step = max(1, len(src_vecs) // 50)
    for tid, svec in src_vecs[::step][:50]:
        row = conn.execute(
            f"SELECT embedding FROM {table} WHERE track_id = %s AND status = 'ready'",
            (tid,),
        ).fetchone()
        sample_n += 1
        if row is None or row["embedding"] is None:
            sample_bad += 1
            continue
        tvec = np.asarray(row["embedding"], dtype=np.float32).reshape(-1)
        denom = float(np.linalg.norm(svec) * np.linalg.norm(tvec))
        if denom < 1e-12 or float(np.dot(svec, tvec)) / denom < 0.999:
            sample_bad += 1
    rows.append(
        VerifyRow(
            "emb: cosine-семпл (≥0.999)",
            f"{sample_n} шт",
            f"bad={sample_bad}",
            sample_bad == 0
            and (sample_n > 0 or src_count == 0),  # 0 векторов — вакуумно OK
        )
    )


def _verify_artists_albums(
    src: sqlite3.Connection, conn: Any, present: set[str], rows: list[VerifyRow]
) -> None:
    """artists/albums: цель — надмножество источника (derivation из строк
    треков для баз до F1.2 создаёт новые сущности), поэтому сравниваем
    подмножество по естественным ключам."""
    if "artists" in present:
        src_rows = [
            tuple(r)
            for r in src.execute("SELECT name_norm, sort_name, mbid FROM artists")
        ]
        norms = {r[0] for r in src_rows}
        tgt_sub = [
            tuple(r.values())
            for r in conn.execute(
                "SELECT name_norm, sort_name, mbid FROM artists"
            ).fetchall()
            if r["name_norm"] in norms
        ]
        s, t = _checksum(src_rows), _checksum(tgt_sub)
        rows.append(
            VerifyRow(
                f"artists (≥src) [md5 {len(src_rows)}→{len(tgt_sub)}]",
                s[:12],
                t[:12],
                s == t,
            )
        )
    if "albums" in present:
        src_rows = [
            tuple(r)
            for r in src.execute(
                """
                SELECT (SELECT name_norm FROM artists ar WHERE ar.id = a.artist_id),
                       a.title_norm, a.year
                FROM albums a
                """
            )
        ]
        keys = {(r[0], r[1]) for r in src_rows}
        tgt_sub = [
            (r["artist_norm"], r["title_norm"], r["year"])
            for r in conn.execute(
                """
                SELECT ar.name_norm AS artist_norm, a.title_norm, a.year
                FROM albums a JOIN artists ar ON ar.id = a.artist_id
                """
            ).fetchall()
            if (r["artist_norm"], r["title_norm"]) in keys
        ]
        s, t = _checksum(src_rows), _checksum(tgt_sub)
        rows.append(
            VerifyRow(
                f"albums (≥src) [md5 {len(src_rows)}→{len(tgt_sub)}]",
                s[:12],
                t[:12],
                s == t,
            )
        )


def _verify_favorites_lookup(
    src: sqlite3.Connection,
    conn: Any,
    owner: int,
    present: set[str],
    rows: list[VerifyRow],
) -> None:
    """favorite_artists/albums: переносятся только разрешимые цели (норм-имя
    есть в каталоге цели, дизайн §10.3 «исчезнувшие цели — skip с warning») —
    сравниваем с числом разрешимых, а не с общим счётчиком источника."""
    if "favorite_artists" not in present and "favorite_albums" not in present:
        return
    tgt_artists = {
        str(r["name_norm"])
        for r in conn.execute("SELECT name_norm FROM artists").fetchall()
    }
    tgt_albums = {
        (str(r["name_norm"]), str(r["title_norm"]))
        for r in conn.execute(
            """
            SELECT ar.name_norm, a.title_norm FROM albums a
            JOIN artists ar ON ar.id = a.artist_id
            """
        ).fetchall()
    }
    if "favorite_artists" in present:
        src_rows = [
            str(r["artist"]) for r in src.execute("SELECT artist FROM favorite_artists")
        ]
        resolvable = sum(1 for a in src_rows if norm_name(a) in tgt_artists)
        tgt = int(
            conn.execute(
                "SELECT COUNT(*) AS n FROM user_favorites WHERE user_id = %s AND kind = 'artist'",
                (owner,),
            ).fetchone()["n"]
        )
        rows.append(
            VerifyRow(
                f"favorite_artists → user_favorites(artist) [{len(src_rows) - resolvable} skip]",
                str(resolvable),
                str(tgt),
                resolvable == tgt,
            )
        )
    if "favorite_albums" in present:
        src_rows = [
            (str(r["artist"]), str(r["album"]))
            for r in src.execute("SELECT artist, album FROM favorite_albums")
        ]
        resolvable = sum(
            1 for a, b in src_rows if (norm_name(a), norm_name(b)) in tgt_albums
        )
        tgt = int(
            conn.execute(
                "SELECT COUNT(*) AS n FROM user_favorites WHERE user_id = %s AND kind = 'album'",
                (owner,),
            ).fetchone()["n"]
        )
        rows.append(
            VerifyRow(
                f"favorite_albums → user_favorites(album) [{len(src_rows) - resolvable} skip]",
                str(resolvable),
                str(tgt),
                resolvable == tgt,
            )
        )


def verify_migration(sqlite_path: Path) -> tuple[list[VerifyRow], bool]:
    """Сверка источник vs цель. → (строки отчёта, всё_ок)."""
    if not backend.is_pg():
        raise SystemExit(
            "MUSIC_HIVE_DATABASE_URL не задан — verify работает только в PG-режиме"
        )
    rows: list[VerifyRow] = []
    src = open_source(sqlite_path)
    try:
        present = _src_tables(src)
        with connect() as conn:
            owner_row = conn.execute(
                "SELECT id FROM users WHERE username = 'owner'"
            ).fetchone()
            owner = int(owner_row["id"]) if owner_row else -1

            def run(check: Check) -> VerifyRow:
                src_rows = [tuple(r) for r in src.execute(check.src)]
                # dict_row в PG: tuple(dict) дал бы ключи — берём values()
                tgt_rows = [
                    tuple(r.values())
                    for r in conn.execute(
                        check.tgt or check.src, (owner,) if check.owner else ()
                    ).fetchall()
                ]
                if check.kind == "count":
                    s, t = int(src_rows[0][0]), int(tgt_rows[0][0])
                    return VerifyRow(check.name, str(s), str(t), s == t)
                s, t = _checksum(src_rows), _checksum(tgt_rows)
                return VerifyRow(f"{check.name} [md5]", s[:12], t[:12], s == t)

            for check in _COUNT_CHECKS:
                if check.table not in present:
                    rows.append(VerifyRow(check.name, "нет таблицы", "0", True))
                    continue
                rows.append(run(check))
            for check in _MD5_CHECKS:
                if check.table not in present:
                    rows.append(
                        VerifyRow(f"{check.name} [md5]", "нет таблицы", "—", True)
                    )
                    continue
                rows.append(run(check))

            _verify_artists_albums(src, conn, present, rows)
            _verify_favorites_lookup(src, conn, owner, present, rows)

            key, _ = resolve_model_key(src)
            rows.append(VerifyRow("emb: модель", key, emb_table_name(key), True))
            _verify_vectors(src, conn, key, rows)
        return rows, all(r.ok for r in rows)
    finally:
        src.close()
