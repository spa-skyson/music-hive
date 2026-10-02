from __future__ import annotations

from typing import Any

import numpy as np

from music_hive.config import get_settings
from music_hive.db.schema import connect, init_db, row_to_dict, utcnow


def ensure_db() -> None:
    init_db()


# ---------------------------------------------------------------- emb-слой
# Реестр embedding_models + физические таблицы emb_<slug> (§6 контракта).
# Активация модели — единственное санкционированное DDL воркера (§5.2).


def model_key() -> str:
    """Ключ текущего энкодера (фабрика get_encoder по MUSIC_HIVE_EMBEDDING_MODEL).

    Для дефолтного 'clap' — 'clap:laion/larger_clap_music_and_speech',
    ровно как до F4.1 (ключ читался из clap-модуля напрямую).
    """
    from music_hive.embed.base import get_encoder

    return get_encoder().model_key()


def _model_slug(key: str) -> str:
    """sanitize(model_key): 'clap:laion/…' → 'clap_laion_…' (шаблон §6.2)."""
    import re

    return re.sub(r"[^a-z0-9]+", "_", key.lower()).strip("_")


def emb_table_name(key: str) -> str:
    return f"emb_{_model_slug(key)}"


def active_model() -> dict[str, Any] | None:
    """Активная модель реестра: {'model_key', 'dim'} | None."""
    with connect() as conn:
        return row_to_dict(
            conn.execute(
                "SELECT model_key, dim FROM embedding_models WHERE is_active LIMIT 1"
            ).fetchone()
        )


def activate_model(key: str) -> None:
    """Атомарная смена активной модели реестра: ровно одна активная (§6.2).

    Модель должна быть зарегистрирована (появляется при первой записи
    векторов, _ensure_model). Чужие emb_* не трогаем — таблицы моделей
    изолированы; несуществующий ключ → ValueError (транзакция откатится,
    прежняя активная останется).
    """
    with connect() as conn:
        conn.execute("UPDATE embedding_models SET is_active = FALSE WHERE is_active")
        row = conn.execute(
            """
            UPDATE embedding_models SET is_active = TRUE, activated_at = now()
            WHERE model_key = %s
            RETURNING model_key
            """,
            (key,),
        ).fetchone()
        if row is None:
            raise ValueError(
                f"модель {key!r} не зарегистрирована — сначала job embed "
                "(строка создаётся первой записью векторов)"
            )


def list_models() -> list[dict[str, Any]]:
    """Реестр моделей для model_status/CLI: [{key, dim, active, vectors}] (F4.3).

    Реестр + счётчик готовых векторов в emb_<slug> каждой модели
    (таблицы ещё нет → vectors=0).
    """
    with connect() as conn:
        rows = conn.execute(
            """
            SELECT model_key, dim, is_active FROM embedding_models
            ORDER BY is_active DESC, model_key
            """
        ).fetchall()
        out: list[dict[str, Any]] = []
        for r in rows:
            table = emb_table_name(r["model_key"])
            # Харденинг (F4.3): строки реестра без физической таблицы не должны
            # ронять запрос в UndefinedTable (/api/admin/models → 502); как в
            # pending_count — проверяем существование, нет таблицы → 0 векторов.
            has_table = conn.execute(
                "SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = %s",
                (table,),
            ).fetchone()
            vectors = (
                int(
                    conn.execute(
                        f"SELECT COUNT(*) AS n FROM {table} WHERE status = 'ready'"
                    ).fetchone()["n"]
                )
                if has_table
                else 0
            )
            out.append(
                {
                    "key": r["model_key"],
                    "dim": int(r["dim"]),
                    "active": bool(r["is_active"]),
                    "vectors": vectors,
                }
            )
        return out


def pending_count(model_key: str | None = None) -> int:
    """Активные недубликатные треки без готового вектора модели (F4.3).

    model_key=None → активная модель.
    Таблицы модели ещё нет (модель без векторов) → все такие треки.
    """
    key = model_key
    if key is None:
        am = active_model()
        key = am["model_key"] if am is not None else None
    ready_filter = ""
    with connect() as conn:
        if key is not None:
            table = emb_table_name(key)
            has_table = conn.execute(
                "SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = %s",
                (table,),
            ).fetchone()
            if has_table:
                ready_filter = f"""
              AND NOT EXISTS (
                  SELECT 1 FROM {table} e WHERE e.track_id = t.id AND e.status = 'ready'
              )
            """
        row = conn.execute(
            f"""
            SELECT COUNT(*) AS n FROM tracks t
            JOIN track_audio_features f ON f.track_id = t.id
            WHERE t.is_active = TRUE AND t.is_duplicate_of IS NULL{ready_filter}
            """
        ).fetchone()
        return int(row["n"]) if row else 0


def _ensure_model(conn: Any, key: str, dim: int) -> None:
    """Регистрирует модель при первой записи; активирует, если активных нет.

    dim фиксируется фактическим вектором (§6.1: «конфиг может врать»).
    Идемпотентно; смена активной модели — отдельный job (F4), здесь только
    первая установка.
    """
    row = conn.execute(
        "SELECT dim FROM embedding_models WHERE model_key = %s", (key,)
    ).fetchone()
    if row is not None:
        if int(row["dim"]) != int(dim):
            raise ValueError(
                f"embedding dim {dim} != registry dim {int(row['dim'])} для {key}"
            )
        return

    table = emb_table_name(key)
    conn.execute(
        f"""
        CREATE TABLE IF NOT EXISTS {table} (
            track_id    BIGINT PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
            embedding   vector({int(dim)}) NOT NULL,
            status      TEXT NOT NULL DEFAULT 'ready',
            error       TEXT,
            computed_at TIMESTAMPTZ NOT NULL DEFAULT now()
        )
        """
    )
    conn.execute(
        f"""
        CREATE INDEX IF NOT EXISTS {table}_hnsw
        ON {table} USING hnsw (embedding vector_cosine_ops)
        WITH (m = 16, ef_construction = 64)
        """
    )
    import json

    from music_hive.embed.segments import SEGMENT_STRATEGY

    params = json.dumps(
        {
            "segment_strategy": SEGMENT_STRATEGY,
            "segment_sec": get_settings().embed_segment_sec,
        }
    )
    # backend/repo — из самого ключа ('clap:<repo>' → clap + <repo>): реестр
    # не зависит от того, какой конфиг включён сейчас.
    backend_name, _, repo = key.partition(":")
    conn.execute(
        """
        INSERT INTO embedding_models (model_key, backend, repo, dim, params)
        VALUES (%s, %s, %s, %s, %s)
        ON CONFLICT (model_key) DO NOTHING
        """,
        (key, backend_name, repo or key, int(dim), params),
    )
    # Атомарный флип «ровно одна активная»: поднимаем нашу модель только если
    # никакая другая не активна (частичный unique-индекс страхует от гонки).
    conn.execute(
        """
        UPDATE embedding_models SET is_active = TRUE, activated_at = now()
        WHERE model_key = %s
          AND NOT EXISTS (
              SELECT 1 FROM embedding_models WHERE is_active AND model_key <> %s
          )
        """,
        (key, key),
    )


def _validated_vector(embedding: np.ndarray) -> np.ndarray:
    """§6.1: без NaN/Inf, L2-норма ∈ [0.98, 1.02] (иначе ренормализация + лог)."""
    vec = np.asarray(embedding, dtype=np.float32).reshape(-1)
    if not np.all(np.isfinite(vec)):
        raise ValueError("embedding содержит NaN/Inf")
    n = float(np.linalg.norm(vec))
    if n < 1e-12:
        raise ValueError("нулевая норма эмбеддинга")
    if not 0.98 <= n <= 1.02:
        import logging

        logging.getLogger(__name__).warning(
            "embedding L2-норма %.4f вне [0.98, 1.02] — ренормализуем", n
        )
        vec = vec / n
    return vec.astype(np.float32)


def upsert_track(data: dict[str, Any]) -> int:
    """Insert or update track by path. Returns track id."""
    now = utcnow()
    # Один statement-upsert (ON CONFLICT DO UPDATE ... RETURNING).
    with connect() as conn:
        cur = conn.execute(
            """
            INSERT INTO tracks (
                path, file_md5, file_mtime, file_size, title, artist, album, year,
                track_number, duration, bitrate, sample_rate, channels,
                fingerprint, lufs, artwork_path, is_active, created_at, updated_at
            ) VALUES (
                %(path)s, COALESCE(%(file_md5)s, ''),
                %(file_mtime)s, %(file_size)s,
                COALESCE(%(title)s, ''), COALESCE(%(artist)s, ''), COALESCE(%(album)s, ''),
                %(year)s,
                %(track_number)s, %(duration)s, %(bitrate)s, %(sample_rate)s,
                %(channels)s, %(fingerprint)s, %(lufs)s, %(artwork_path)s,
                TRUE, %(created_at)s, %(updated_at)s
            )
            ON CONFLICT (path) DO UPDATE SET
                file_md5 = excluded.file_md5,
                file_mtime = excluded.file_mtime,
                file_size = excluded.file_size,
                title = excluded.title,
                artist = excluded.artist,
                album = excluded.album,
                year = excluded.year,
                track_number = excluded.track_number,
                duration = excluded.duration,
                bitrate = excluded.bitrate,
                sample_rate = excluded.sample_rate,
                channels = excluded.channels,
                fingerprint = excluded.fingerprint,
                lufs = excluded.lufs,
                artwork_path = excluded.artwork_path,
                is_active = TRUE,
                updated_at = excluded.updated_at
            RETURNING id
            """,
            {**data, "created_at": now, "updated_at": now},
        )
        tid = int(cur.fetchone()["id"])
        conn.execute(
            """
            INSERT INTO track_audio_features (track_id, status)
            VALUES (%s, 'pending')
            ON CONFLICT (track_id) DO NOTHING
            """,
            (tid,),
        )
        return tid


# --------------------------------------------- artists / albums (F1.2)
# Схлопывание дубликатов (контракт 00001_init.sql): артист — name_norm =
# lower/casefold + trim + collapse ws; альбом — (artist_id, title_norm).
# Год в ключ НЕ входит: дозаполняет NULL у существующего альбома.


def norm_name(name: str) -> str:
    """Ключ схлопывания имён: trim + collapse whitespace + casefold."""
    return " ".join(name.split()).casefold()


def _mbid_value(mbid: str | None) -> Any:
    """PG ждёт uuid.UUID (нативный адаптер). Мусор → NULL."""
    if not mbid:
        return None
    import uuid

    try:
        parsed = uuid.UUID(mbid)
    except ValueError:
        return None
    return parsed


def _upsert_artist(
    conn: Any, name: str, *, mbid: str | None = None
) -> tuple[int, bool]:
    """Вставка/поиск артиста по name_norm. → (id, created)."""
    name = name.strip()
    norm = norm_name(name)
    if not norm:
        raise ValueError("пустое имя артиста")
    row = conn.execute(
        "SELECT id FROM artists WHERE name_norm = ?", (norm,)
    ).fetchone()
    if row is not None:
        return int(row["id"]), False
    conn.execute(
        """
        INSERT OR IGNORE INTO artists (name, name_norm, mbid, created_at)
        VALUES (?, ?, ?, ?)
        """,
        (name, norm, _mbid_value(mbid), utcnow()),
    )
    row = conn.execute(
        "SELECT id FROM artists WHERE name_norm = ?", (norm,)
    ).fetchone()
    assert row is not None, "INSERT OR IGNORE прошёл, а строки нет"
    return int(row["id"]), True


def _upsert_album(
    conn: Any, artist_id: int, title: str, *, year: int | None = None, mbid: str | None = None
) -> tuple[int, bool]:
    """Вставка/поиск альбома по (artist_id, title_norm). → (id, created).

    Год не входит в ключ схлопывания: если у существующего альбома год NULL,
    дозаполняется переданным.
    """
    title = title.strip()
    norm = norm_name(title)
    if not norm:
        raise ValueError("пустое название альбома")
    row = conn.execute(
        "SELECT id, year FROM albums WHERE artist_id = ? AND title_norm = ?",
        (artist_id, norm),
    ).fetchone()
    if row is not None:
        if year is not None and row["year"] is None:
            conn.execute(
                "UPDATE albums SET year = ? WHERE id = ?", (year, row["id"])
            )
        return int(row["id"]), False
    conn.execute(
        """
        INSERT OR IGNORE INTO albums (artist_id, title, title_norm, year, mbid, created_at)
        VALUES (?, ?, ?, ?, ?, ?)
        """,
        (artist_id, title, norm, year, _mbid_value(mbid), utcnow()),
    )
    row = conn.execute(
        "SELECT id FROM albums WHERE artist_id = ? AND title_norm = ?",
        (artist_id, norm),
    ).fetchone()
    assert row is not None, "INSERT OR IGNORE прошёл, а строки нет"
    return int(row["id"]), True


def upsert_artist(name: str, *, mbid: str | None = None) -> int:
    """Создать/найти артиста (ключ — norm_name). Returns artist id."""
    with connect() as conn:
        artist_id, _created = _upsert_artist(conn, name, mbid=mbid)
        return artist_id


def upsert_album(
    artist_id: int, name: str, *, year: int | None = None, mbid: str | None = None
) -> int:
    """Создать/найти альбом (ключ — artist_id + norm_name(title)). Returns album id."""
    with connect() as conn:
        album_id, _created = _upsert_album(conn, artist_id, name, year=year, mbid=mbid)
        return album_id


def link_track_artist_album(
    track_id: int,
    *,
    artist: str | None,
    album: str | None,
    album_artist: str | None = None,
    year: int | None = None,
) -> tuple[int | None, int | None]:
    """Создать/найти artist+album трека и проставить ему FK. → (artist_id, album_id).

    Альбом-артист — тег albumartist с fallback на artist (сканер F1.2). Альбом
    без артиста не линкуется (albums.artist_id NOT NULL) — потребители таких
    треков остаются на строковой группировке.
    """
    artist = (artist or "").strip()
    album = (album or "").strip()
    album_artist = (album_artist or "").strip() or artist
    with connect() as conn:
        artist_id: int | None = None
        album_artist_id: int | None = None
        if artist:
            artist_id, _ = _upsert_artist(conn, artist)
        if album_artist:
            if artist_id is not None and norm_name(album_artist) == norm_name(artist):
                album_artist_id = artist_id
            else:
                album_artist_id, _ = _upsert_artist(conn, album_artist)
        album_id: int | None = None
        if album and album_artist_id is not None:
            album_id, _ = _upsert_album(conn, album_artist_id, album, year=year)
        conn.execute(
            "UPDATE tracks SET artist_id = ?, album_artist_id = ?, album_id = ? WHERE id = ?",
            (artist_id, album_artist_id, album_id, track_id),
        )
        if album_id is not None:
            # Обложка альбома — первый трек с артом побеждает (#34): guard
            # cover_track_id IS NULL делает идемпотентным; artwork_path уже
            # записан upsert_track до линковки, читаем его из tracks.
            conn.execute(
                """
                UPDATE albums SET cover_track_id = ?
                WHERE id = ? AND cover_track_id IS NULL
                  AND EXISTS (
                      SELECT 1 FROM tracks WHERE id = ? AND artwork_path IS NOT NULL
                  )
                """,
                (track_id, album_id, track_id),
            )
        return artist_id, album_id


def artists_backfill() -> dict[str, int]:
    """Разовый бэкфилл F1.2: artists/albums из строковых полей треков + FK.

    Проходит по линкуемым трекам (есть строки, но пустые FK), создаёт сущности,
    проставляет FK. Идемпотентно: повторный прогон не находит кандидатов и не
    создаёт ничего нового.
    """
    with connect() as conn:
        rows = conn.execute(
            """
            SELECT id, artist, album, year FROM tracks
            WHERE (artist_id IS NULL AND trim(COALESCE(artist, '')) != '')
               OR (album_id IS NULL AND trim(COALESCE(album, '')) != ''
                   AND trim(COALESCE(artist, '')) != '')
            """
        ).fetchall()
        stats = {
            "tracks_to_link": len(rows),
            "tracks_linked": 0,
            "artists_created": 0,
            "artists_reused": 0,
            "albums_created": 0,
            "albums_reused": 0,
        }
        for r in rows:
            artist = (r["artist"] or "").strip()
            album = (r["album"] or "").strip()
            # Строка album artist исторически не хранилась — берём artist трека.
            artist_id: int | None = None
            album_id: int | None = None
            if artist:
                artist_id, created = _upsert_artist(conn, artist)
                stats["artists_created" if created else "artists_reused"] += 1
            if album and artist_id is not None:
                album_id, created = _upsert_album(conn, artist_id, album, year=r["year"])
                stats["albums_created" if created else "albums_reused"] += 1
            if artist_id is not None or album_id is not None:
                conn.execute(
                    "UPDATE tracks SET artist_id = ?, album_artist_id = ?, album_id = ? WHERE id = ?",
                    (artist_id, artist_id, album_id, r["id"]),
                )
                stats["tracks_linked"] += 1
        # «Осталось»: треки с альбомом, который не с чем линковать (артиста нет)
        row = conn.execute(
            """
            SELECT COUNT(*) AS n FROM tracks
            WHERE album_id IS NULL AND trim(COALESCE(album, '')) != ''
            """
        ).fetchone()
        stats["tracks_left"] = int(row["n"]) if row else 0
        return stats


def set_genres(track_id: int, genre_names: list[str]) -> None:
    with connect() as conn:
        conn.execute("DELETE FROM track_genres WHERE track_id = ?", (track_id,))
        for name in genre_names:
            name = name.strip()
            if not name:
                continue
            conn.execute("INSERT OR IGNORE INTO genres (name) VALUES (?)", (name,))
            gid = conn.execute(
                "SELECT id FROM genres WHERE name = ?", (name,)
            ).fetchone()["id"]
            conn.execute(
                "INSERT OR IGNORE INTO track_genres (track_id, genre_id) VALUES (?, ?)",
                (track_id, gid),
            )


def mark_missing_inactive(seen_paths: set[str]) -> int:
    with connect() as conn:
        rows = conn.execute(
            "SELECT id, path FROM tracks WHERE is_active = TRUE"
        ).fetchall()
        n = 0
        for row in rows:
            if row["path"] not in seen_paths:
                conn.execute(
                    "UPDATE tracks SET is_active = FALSE, updated_at = ? WHERE id = ?",
                    (utcnow(), row["id"]),
                )
                n += 1
        return n


def update_fingerprint_and_lufs(
    track_id: int, *, fingerprint: str | None, lufs: float | None
) -> None:
    with connect() as conn:
        conn.execute(
            "UPDATE tracks SET fingerprint = COALESCE(?, fingerprint), lufs = COALESCE(?, lufs), updated_at = ? WHERE id = ?",
            (fingerprint, lufs, utcnow(), track_id),
        )
        if lufs is not None:
            conn.execute(
                "UPDATE features SET lufs = ? WHERE track_id = ?",
                (lufs, track_id),
            )


def update_audio_scalars(
    track_id: int,
    *,
    bpm: float | None = None,
    key_name: str | None = None,
    mode: str | None = None,
    lufs: float | None = None,
) -> None:
    with connect() as conn:
        conn.execute(
            """
            UPDATE features SET
                bpm = COALESCE(?, bpm),
                key_name = COALESCE(?, key_name),
                mode = COALESCE(?, mode),
                lufs = COALESCE(?, lufs)
            WHERE track_id = ?
            """,
            (bpm, key_name, mode, lufs, track_id),
        )
        if lufs is not None:
            conn.execute(
                "UPDATE tracks SET lufs = ?, updated_at = ? WHERE id = ?",
                (lufs, utcnow(), track_id),
            )


def mark_duplicates() -> int:
    """Mark duplicates: same MD5, then fingerprint, then artist+title.

    Keeps the highest-bitrate (then largest) copy; others get is_duplicate_of.
    """
    with connect() as conn:
        # Clear previous duplicate flags among active tracks so re-runs are idempotent.
        conn.execute(
            """
            UPDATE tracks SET is_duplicate_of = NULL
            WHERE is_active = TRUE AND is_duplicate_of IS NOT NULL
            """
        )
        marked = 0

        def _mark_groups(sql: str) -> int:
            rows = conn.execute(sql).fetchall()
            best: dict[str, int] = {}
            n = 0
            for row in rows:
                key = row["grp"]
                if not key:
                    continue
                if key not in best:
                    best[key] = row["id"]
                else:
                    conn.execute(
                        "UPDATE tracks SET is_duplicate_of = ?, updated_at = ? WHERE id = ?",
                        (best[key], utcnow(), row["id"]),
                    )
                    n += 1
            return n

        # 1) identical files
        marked += _mark_groups(
            """
            SELECT id,
                   file_md5 AS grp,
                   COALESCE(bitrate, 0) AS bitrate,
                   COALESCE(file_size, 0) AS file_size
            FROM tracks
            WHERE is_active = TRUE
              AND is_duplicate_of IS NULL
              AND file_md5 IS NOT NULL AND file_md5 != ''
            ORDER BY file_md5, bitrate DESC, file_size DESC, id ASC
            """
        )
        # 2) chromaprint (when present)
        marked += _mark_groups(
            """
            SELECT id,
                   fingerprint AS grp,
                   COALESCE(bitrate, 0) AS bitrate,
                   COALESCE(file_size, 0) AS file_size
            FROM tracks
            WHERE is_active = TRUE
              AND is_duplicate_of IS NULL
              AND fingerprint IS NOT NULL AND fingerprint != ''
            ORDER BY fingerprint, bitrate DESC, file_size DESC, id ASC
            """
        )
        # 3) same song metadata (different encodes / renames)
        marked += _mark_groups(
            """
            SELECT id,
                   lower(trim(artist)) || '|' || lower(trim(title)) AS grp,
                   COALESCE(bitrate, 0) AS bitrate,
                   COALESCE(file_size, 0) AS file_size
            FROM tracks
            WHERE is_active = TRUE
              AND is_duplicate_of IS NULL
              AND trim(COALESCE(artist, '')) != ''
              AND trim(COALESCE(title, '')) != ''
            ORDER BY lower(trim(artist)), lower(trim(title)),
                     bitrate DESC, file_size DESC, id ASC
            """
        )
        return marked


def counts() -> dict[str, int]:
    with connect() as conn:

        def _one(sql: str) -> int:
            row = conn.execute(sql).fetchone()
            return int(row["n"]) if row else 0

        total = _one("SELECT COUNT(*) AS n FROM tracks")
        active = _one(
            """
            SELECT COUNT(*) AS n FROM tracks
            WHERE is_active = TRUE AND is_duplicate_of IS NULL
            """
        )
        pending = _one(
            "SELECT COUNT(*) AS n FROM features WHERE status IN ('pending', 'retry')"
        )
        ready = _one("SELECT COUNT(*) AS n FROM features WHERE status = 'ready'")
        failed = _one("SELECT COUNT(*) AS n FROM features WHERE status = 'failed'")
        out = {
            "tracks_total": total,
            "tracks_active": active,
            "features_pending": pending,
            "features_ready": ready,
            "features_failed": failed,
        }
        # готовые эмбеддинги — из таблицы активной модели
        am = active_model()
        if am is not None:
            table = emb_table_name(am["model_key"])
            out["embeddings_ready"] = _one(
                f"SELECT COUNT(*) AS n FROM {table} WHERE status = 'ready'"
            )
        else:
            out["embeddings_ready"] = 0
        return out


def list_active_tracks(limit: int = 20) -> list[dict[str, Any]]:
    with connect() as conn:
        rows = conn.execute(
            """
            SELECT id, title, artist, album, path, bitrate, lufs, fingerprint
            FROM tracks
            WHERE is_active = TRUE AND is_duplicate_of IS NULL
            ORDER BY artist, album, track_number
            LIMIT ?
            """,
            (limit,),
        ).fetchall()
        return [dict(r) for r in rows]


def get_track_by_path(path: str) -> dict[str, Any] | None:
    with connect() as conn:
        return row_to_dict(
            conn.execute("SELECT * FROM tracks WHERE path = ?", (path,)).fetchone()
        )


def track_file_states() -> dict[str, dict[str, Any]]:
    """Return the lightweight file state needed by incremental scans."""
    with connect() as conn:
        rows = conn.execute(
            "SELECT path, file_mtime, file_size, is_active FROM tracks"
        ).fetchall()
        return {str(row["path"]): dict(row) for row in rows}


def list_tracks_needing_embedding(
    *, limit: int | None = None, force: bool = False, model: str | None = None
) -> list[dict[str, Any]]:
    """Active non-duplicate tracks without a ready embedding (or all if force).

    Policy re-embed (§6.2): готовность считается по таблице РАБОЧЕЙ модели
    (config-энкодер, model_key()), а не активной. model (F4.3, job
    model_activate) — явный ключ: добирает векторы конкретной модели,
    независимо от MUSIC_HIVE_EMBEDDING_MODEL воркера. Несовпадение активной
    с рабочей → треки, готовые в чужой emb_*, всё равно считаются заново в
    таблицу рабочей модели; чужие таблицы не трогаются (изолированы).
    Таблицы рабочей модели ещё нет → нужны все.
    """
    table = emb_table_name(model or model_key())
    with connect() as conn:
        has_table = conn.execute(
            "SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = %s",
            (table,),
        ).fetchone()
        ready_filter = (
            ""
            if force or not has_table
            else f"""
          AND NOT EXISTS (
              SELECT 1 FROM {table} e WHERE e.track_id = t.id AND e.status = 'ready'
          )
        """
        )
        sql = f"""
            SELECT t.id, t.path, t.file_md5, t.duration, t.title, t.artist, f.status
            FROM tracks t
            JOIN track_audio_features f ON f.track_id = t.id
            WHERE t.is_active = TRUE AND t.is_duplicate_of IS NULL{ready_filter}
            ORDER BY t.artist, t.album, t.track_number
        """
        if limit is not None:
            sql += f" LIMIT {int(limit)}"
        return [dict(r) for r in conn.execute(sql).fetchall()]


def save_embedding(
    track_id: int,
    embedding: np.ndarray,
    *,
    model: str | None = None,
) -> None:
    """Записать вектор трека.

    Векторы живут в emb_<slug> модели (§6); model (F4.3) — явный ключ
    (job model_activate пишет в таблицу активируемой модели), None — config-
    энкодер воркера. Долговечная копия вектора — сама база, файлового кеша нет.
    """
    vec = _validated_vector(embedding)
    key = model or model_key()
    now = utcnow()
    with connect() as conn:
        _ensure_model(conn, key, int(vec.shape[0]))
        table = emb_table_name(key)
        conn.execute(
            f"""
            INSERT INTO {table} (track_id, embedding, status, computed_at)
            VALUES (%s, %s, 'ready', %s)
            ON CONFLICT (track_id) DO UPDATE SET
                embedding = excluded.embedding,
                status = 'ready',
                error = NULL,
                computed_at = excluded.computed_at
            """,
            (track_id, vec, now),
        )
        conn.execute(
            """
            UPDATE track_audio_features
            SET status = 'ready', error = NULL, computed_at = %s
            WHERE track_id = %s
            """,
            (now, track_id),
        )


def mark_feature_failed(track_id: int, error: str) -> None:
    with connect() as conn:
        conn.execute(
            """
            UPDATE features SET status = 'failed', error = ?, computed_at = ?
            WHERE track_id = ?
            """,
            (error[:2000], utcnow(), track_id),
        )


def get_embedding(track_id: int) -> np.ndarray | None:
    am = active_model()
    if am is None:
        return None
    table = emb_table_name(am["model_key"])
    with connect() as conn:
        row = conn.execute(
            f"SELECT embedding FROM {table} WHERE track_id = %s AND status = 'ready'",
            (track_id,),
        ).fetchone()
    if not row or row["embedding"] is None:
        return None
    return np.asarray(row["embedding"], dtype=np.float32).reshape(-1)
