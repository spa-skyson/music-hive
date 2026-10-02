"""Точка доступа воркера к БД: только PostgreSQL (#11, F5).

DDL выполняет исключительно goose (`music-hive-player migrate`); воркер
делает fail-fast проверку версии (backend.check_migrated). SQLite-режим
и его DDL сняты; чтение легаси-SQLite осталось только в инструменте
переезда db/migrate_sqlite.py (читает старый файл напрямую).
"""

from __future__ import annotations

from collections.abc import Iterator
from contextlib import contextmanager
from datetime import UTC, datetime
from typing import Any

from music_hive.db import backend


def utcnow() -> str:
    return datetime.now(UTC).isoformat()


@contextmanager
def connect() -> Iterator[Any]:
    """Соединение с PG из пула воркера (адаптер PgConn с `?`-плейсхолдерами)."""
    with backend.connect_pg() as conn:
        yield conn


def init_db() -> None:
    """Fail-fast: goose-версия схемы + owner_id для user-scoped записей (§4.2)."""
    backend.check_migrated()
    backend.owner_id()


def row_to_dict(row: Any | None) -> dict[str, Any] | None:
    if row is None:
        return None
    return dict(row)
