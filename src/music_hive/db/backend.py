"""PostgreSQL-бэкенд воркера (#11, F5): единственный режим, SQLite снят.

DSN берётся из ``MUSIC_HIVE_DATABASE_URL`` (обязателен, fail-fast).

Здесь живёт:
- ленивый пул ``psycopg_pool.ConnectionPool`` (min 1 / max 4, §5.1 контракта);
- адаптер ``PgConn``, приводящий исторический sqlite-стиль SQL
  (``?``/``:name``-плейсхолдеры, ``INSERT OR IGNORE``, чтение ``lastrowid``)
  к psycopg3, чтобы модули scanner/brain/listen/discover/lyrics/jobs
  работали без переписывания каждого запроса;
- fail-fast проверка goose-версии: воркер НИКОГДА не выполняет DDL схемы,
  кроме санкционированного исключения — активации модели эмбеддинга
  (таблицы ``emb_<slug>``, §5.2/§6.2; см. db/store.py).

Табличные переименования SQLite-эпохи → PG, не меняющие колонок, делает
адаптер (``features`` → ``track_audio_features``); структурные расхождения
(user_id, состав PK) обрабатываются ветками в вызывающих модулях.
"""

from __future__ import annotations

import logging
import re
import secrets
import threading
from collections.abc import Iterator
from contextlib import contextmanager
from typing import Any

from music_hive.config import get_settings

log = logging.getLogger(__name__)

# Таблицы, переименованные в PG без изменения колонок (см. §4.2 контракта).
# Структурно изменившиеся (user_id в PK, новые NOT NULL-колонки) здесь быть
# НЕ должны — они обрабатываются явными ветками в модулях.
_TABLE_RENAMES = {
    "features": "track_audio_features",
    "listen_later": "user_listen_later",
    "rec_stats": "user_track_stats",
}

_RE_NAMED = re.compile(r"(?<!:):([A-Za-z_][A-Za-z0-9_]*)")
_RE_IGNORE = re.compile(r"^\s*INSERT\s+OR\s+IGNORE\s+INTO\s+", re.IGNORECASE)


def database_url() -> str:
    """DSN из настроек; пустой — ошибка конфигурации (PG-only, #11 F5)."""
    url = get_settings().database_url.strip()
    if not url:
        raise RuntimeError(
            "MUSIC_HIVE_DATABASE_URL не задан: воркер работает только с "
            "PostgreSQL (SQLite-режим снят, #11). Пример: "
            "postgres://music_hive:music_hive@postgres:5432/music_hive?sslmode=disable"
        )
    return url


def is_pg() -> bool:
    """Совместимость с вызывающими (db/migrate_sqlite.py): всегда True —
    режим один, database_url() сам падает на пустом DSN."""
    database_url()
    return True


# --------------------------------------------------------------------- pool

_pool: Any = None
_pool_lock = threading.Lock()


def _configure(conn: Any) -> None:
    """Per-connection setup: dict rows, pgvector, jsonb → str (как в SQLite)."""
    from psycopg.rows import dict_row

    conn.row_factory = dict_row
    try:
        from pgvector.psycopg import register_vector
        from pgvector.psycopg.vector import VectorBinaryLoader, VectorLoader
        from psycopg.types import TypeInfo

        register_vector(conn)

        # Лоадеры pgvector по умолчанию возвращают pgvector.Vector; всему
        # воркеру нужны np.ndarray — переопределяем в одной точке.
        class _NdarrayLoader(VectorLoader):
            def load(self, data: Any) -> Any:
                vec = super().load(data)
                return None if vec is None else vec.to_numpy()

        class _NdarrayBinaryLoader(VectorBinaryLoader):
            def load(self, data: Any) -> Any:
                vec = super().load(data)
                return None if vec is None else vec.to_numpy()

        info = TypeInfo.fetch(conn, "vector")
        conn.adapters.register_loader(info.oid, _NdarrayLoader)
        conn.adapters.register_loader(info.oid, _NdarrayBinaryLoader)
    except Exception:
        log.debug("pgvector не зарегистрирован (нет extension vector?)", exc_info=True)
    # jsonb наружу отдаём строкой: существующий код делает json.loads(...)
    # по payload_json/meta_json/track_ids_json и не должен знать про jsonb.
    from psycopg.types import json as _psycopg_json

    def _loads_str(data: Any) -> Any:
        return (
            data.decode() if isinstance(data, (bytes, bytearray, memoryview)) else data
        )

    _psycopg_json.set_json_loads(_loads_str, context=conn)


def get_pool() -> Any:
    global _pool
    if _pool is not None:
        return _pool
    with _pool_lock:
        if _pool is None:
            from psycopg import conninfo
            from psycopg_pool import ConnectionPool

            url = database_url()
            # Таймауты из §5.1 контракта; make_conninfo мержит их поверх DSN.
            ci = conninfo.make_conninfo(
                url,
                application_name="music-hive-worker",
                options="-c statement_timeout=10s -c idle_in_transaction_session_timeout=30s",
            )
            _pool = ConnectionPool(
                conninfo=ci,
                min_size=1,
                max_size=4,
                timeout=30,
                name="music-hive-worker",
                configure=_configure,
                open=True,
            )
    return _pool


def reset_pool() -> None:
    """Тесты переключают DATABASE_URL — пул надо пересоздавать."""
    global _pool
    with _pool_lock:
        if _pool is not None:
            _pool.close()
            _pool = None


# --------------------------------------------------------- SQL translation


def translate_sql(sql: str) -> tuple[str, bool, bool]:
    """sqlite-стиль → psycopg3. Возвращает (sql, or_ignore, is_insert)."""
    or_ignore = False
    m = _RE_IGNORE.match(sql)
    if m:
        # хвост \s+ уже съеден regex'ом — просто приклеиваем остаток
        sql = "INSERT INTO " + sql[m.end() :]
        or_ignore = True
    for old, new in _TABLE_RENAMES.items():
        sql = re.sub(rf"\b{old}\b", new, sql)
    sql = _RE_NAMED.sub(r"%(\1)s", sql)
    sql = sql.replace("?", "%s")
    is_insert = re.match(r"\s*INSERT\b", sql, re.IGNORECASE) is not None
    return sql, or_ignore, is_insert


# ------------------------------------------------------------------ adapter

# Таблицы с identity-колонкой id: только им адаптер добавляет RETURNING id
# (ради cur.lastrowid). Вставки в таблицы с составными/иными PK — как есть.
_IDENTITY_TABLES = frozenset(
    {
        "tracks",
        "genres",
        "jobs",
        "listening_history",
        "playlists",
        "discover_tips",
        "recommendation_impressions",
    }
)
_RE_INSERT_INTO = re.compile(
    r"^\s*INSERT\s+INTO\s+([A-Za-z_][A-Za-z0-9_]*)", re.IGNORECASE
)


class _Cursor:
    """Cursor-подобная обёртка над psycopg-курсором."""

    def __init__(self, cur: Any, lastrowid: int | None) -> None:
        self._cur = cur
        self.lastrowid = lastrowid

    def fetchone(self) -> Any:
        return self._cur.fetchone()

    def fetchall(self) -> list[Any]:
        return self._cur.fetchall()


class PgConn:
    """psycopg-соединение с легаси-совместимым execute() (`?`/`:name`).

    SELECT/UPDATE выполняются как есть; к INSERT в таблицы с identity-колонкой
    ``id`` без RETURNING добавляется ``RETURNING id`` ради ``cur.lastrowid``
    (все читатели lastrowid в проекте попадают в этот список). Вставки в
    таблицы с составными/иными PK (embedding_models, scan_state, …) остаются
    как есть.
    """

    def __init__(self, conn: Any) -> None:
        self._conn = conn

    def execute(self, sql: str, params: Any = ()) -> _Cursor:
        pg_sql, or_ignore, is_insert = translate_sql(sql)
        if or_ignore:
            pg_sql += " ON CONFLICT DO NOTHING"
        lastrowid: int | None = None
        wants_id = False
        if is_insert and "returning" not in pg_sql.lower():
            m = _RE_INSERT_INTO.match(pg_sql)
            table = m.group(1).lower() if m else ""
            wants_id = table in _IDENTITY_TABLES
            if wants_id:
                pg_sql += " RETURNING id"
        cur = self._conn.execute(pg_sql, params if params else None)
        if wants_id:
            row = cur.fetchone()
            if row is not None and row.get("id") is not None:
                lastrowid = int(row["id"])
        return _Cursor(cur, lastrowid)

    def commit(self) -> None:
        self._conn.commit()

    def rollback(self) -> None:
        self._conn.rollback()

    def close(self) -> None:
        self._conn.close()


@contextmanager
def connect_pg() -> Iterator[PgConn]:
    with get_pool().connection() as conn:
        # psycopg_pool сам делает commit/rollback при выходе из блока.
        yield PgConn(conn)


# ------------------------------------------------------- миграции / owner


class NotMigratedError(RuntimeError):
    pass


def check_migrated(attempts: int = 5, delay_sec: float = 2.0) -> None:
    """Fail-fast: схема применяется только goose (player). Воркер не делает DDL.

    Проверяет goose_db_version ≥ 1; даёт несколько попыток подключения
    (защита от старта раньше PG/player вне compose, §5.2).
    """
    import time

    import psycopg
    from psycopg import errors as pg_errors

    last_exc: Exception | None = None
    for i in range(attempts):
        try:
            with connect_pg() as conn:
                row = conn.execute(
                    "SELECT COALESCE(MAX(version_id), 0) AS v FROM goose_db_version"
                ).fetchone()
            version = int(row["v"]) if row else 0
            if version < 1:
                raise NotMigratedError(_MIGRATE_HINT)
            return
        except NotMigratedError:
            raise
        except pg_errors.UndefinedTable:
            # Подключение есть, схемы нет — ретраить бессмысленно.
            raise NotMigratedError(_MIGRATE_HINT) from None
        except psycopg.Error as exc:
            last_exc = exc
            if i + 1 < attempts:
                log.warning(
                    "PostgreSQL недоступен (%s), попытка %d/%d", exc, i + 1, attempts
                )
                time.sleep(delay_sec)
    raise NotMigratedError(f"PostgreSQL недоступен: {last_exc}. {_MIGRATE_HINT}")


_MIGRATE_HINT = (
    "Схема базы не инициализирована (goose_db_version отсутствует или < 1). "
    "Воркер не выполняет DDL — запусти `music-hive-player migrate` и повтори."
)

_owner_id: int | None = None
_owner_lock = threading.Lock()


def owner_id() -> int:
    """Владелец (users.id) — единый user_id для user-scoped записей воркера.

    F1: multi-user ещё нет (F2), поэтому все записи воркера относятся к
    владельцу id=1 (§4.2 «легаси-строки → владелец»). Если users пуста —
    создаём заблокированного владельца; реальный bootstrap пароля сделает
    F2 (/api/auth/setup) или импортёр F1.5, обновив эту строку.
    """
    global _owner_id
    if _owner_id is not None:
        return _owner_id
    with _owner_lock:
        if _owner_id is None:
            with connect_pg() as conn:
                conn.execute(
                    """
                    INSERT INTO users (username, display_name, password_argon2,
                                       is_admin, is_owner)
                    VALUES ('owner', 'owner', %s, TRUE, TRUE)
                    ON CONFLICT (username) DO NOTHING
                    """,
                    # Никому неизвестный пароль: вход невозможен, пока F2/F1.5
                    # не перезапишут argon2-хеш настоящим.
                    (f"$locked${secrets.token_hex(32)}",),
                )
                row = conn.execute(
                    "SELECT id FROM users WHERE username = 'owner'"
                ).fetchone()
            _owner_id = int(row["id"])
    return _owner_id


def resolve_user(username: str | None) -> int:
    """username → users.id (PG-режим); None/пустое → владелец (owner_id).

    F2.4: точка резолва ``--user`` CLI и user_id из payload job'ов.
    username — CITEXT, сравнение регистронезависимое. Неизвестное имя —
    KeyError (вызывающие CLI-команды показывают его пользователю).
    """
    if username is None or not username.strip():
        return owner_id()
    with connect_pg() as conn:
        row = conn.execute(
            "SELECT id FROM users WHERE username = %s", (username.strip(),)
        ).fetchone()
    if row is None:
        raise KeyError(f"пользователь не найден: {username.strip()!r}")
    return int(row["id"])


def reset_owner_cache() -> None:
    global _owner_id
    with _owner_lock:
        _owner_id = None
