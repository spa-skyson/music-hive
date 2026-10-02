"""F2.4 (GitLab #22): per-user taste profiles и персональные генераторы (PG).

Запуск — как у test_pg_worker.py (тот же паттерн):

    MUSIC_HIVE_TEST_DATABASE_URL=postgres://... uv run pytest tests/test_pg_users.py -q

Без переменной модуль пропускается. Фикстура pg_env — общая, из tests/conftest.py;
хелперы (_make_wav, _unit_vec) импортируются из test_pg_worker.
"""

from __future__ import annotations

import secrets
from datetime import UTC, datetime

import numpy as np
import pytest
from test_pg_worker import PG_URL, _make_wav, _unit_vec

pytestmark = pytest.mark.skipif(
    not PG_URL, reason="MUSIC_HIVE_TEST_DATABASE_URL не задан — PG-тесты пропущены"
)


def _create_user(username: str) -> int:
    """Второй пользователь (пароль заблокирован — вход невозможен, как у owner-bootstrap).

    Идемпотентен: фикстура pg_env не чистит users (F1-базлайн), username — CITEXT UNIQUE.
    """
    from music_hive.db.schema import connect

    with connect() as conn:
        conn.execute(
            """
            INSERT INTO users (username, display_name, password_argon2, is_admin)
            VALUES (%s, %s, %s, FALSE)
            ON CONFLICT (username) DO NOTHING
            """,
            (username, username, f"$locked${secrets.token_hex(32)}"),
        )
        row = conn.execute(
            "SELECT id FROM users WHERE username = %s", (username,)
        ).fetchone()
    assert row is not None
    return int(row["id"])


def _seed_catalog(tmp_path, n_left: int = 2, n_right: int = 2) -> list[int]:
    """n_left треков вокруг e1, n_right — вокруг e2 (ортогональные направления)."""
    from music_hive.db import ensure_db, save_embedding
    from music_hive.db.schema import connect
    from music_hive.scanner import scan_library

    lib = tmp_path / "lib"
    lib.mkdir(exist_ok=True)
    e1 = _unit_vec(8, seed=101)
    e2raw = _unit_vec(8, seed=102)
    e2 = e2raw - float(e1 @ e2raw) * e1  # ортогонализуем → контраст вкусов
    e2 /= np.linalg.norm(e2)

    freqs = [330.0, 335.0, 340.0, 345.0, 350.0, 355.0, 660.0, 665.0, 670.0, 675.0, 680.0, 685.0]
    for i in range(n_left + n_right):
        _make_wav(lib / f"t{i}.wav", freq=freqs[i % len(freqs)])
    ensure_db()
    result = scan_library(lib, extract_audio=False, workers=2)
    assert result.upserted == n_left + n_right

    with connect() as conn:
        ids = [
            int(r["id"])
            for r in conn.execute(
                "SELECT id FROM tracks ORDER BY id LIMIT %s",
                (n_left + n_right,),
            ).fetchall()
        ]
    for i, tid in enumerate(ids):
        base = e1 if i < n_left else e2
        jitter = 0.01 * _unit_vec(8, seed=200 + i)
        save_embedding(tid, base + jitter)
    return ids


def test_pg_two_users_two_taste_profiles(pg_env, tmp_path):
    """DoD F2.4: два пользователя → две изолированные истории → два разных профиля."""
    from music_hive.db import backend
    from music_hive.db.schema import connect
    from music_hive.index.brute import load_index
    from music_hive.listen.history import history_counts, recent_history, record_listen
    from music_hive.listen.profile import build_profile, resolve_taste

    ids = _seed_catalog(tmp_path, n_left=2, n_right=2)
    e1 = _unit_vec(8, seed=101)
    e2raw = _unit_vec(8, seed=102)
    e2 = e2raw - float(e1 @ e2raw) * e1
    e2 /= np.linalg.norm(e2)

    alice = _create_user("alice")
    owner = backend.owner_id()
    assert alice != owner

    # alice слушает «левые» треки, владелец — «правые»
    record_listen(ids[0], "finish", user_id=alice)
    record_listen(ids[1], "finish", user_id=alice)
    record_listen(ids[2], "like")  # default → владелец
    record_listen(ids[3], "like")

    # истории изолированы
    assert history_counts(user_id=alice) == {"finish": 2, "total": 2}
    assert history_counts() == {"like": 2, "total": 2}  # default — владелец
    alice_tracks = {r["track_id"] for r in recent_history(10, user_id=alice)}
    assert alice_tracks == {ids[0], ids[1]}
    with connect() as conn:
        per_user = {
            int(r["user_id"]): int(r["n"])
            for r in conn.execute(
                "SELECT user_id, COUNT(*) AS n FROM listening_history GROUP BY user_id"
            ).fetchall()
        }
    assert per_user == {alice: 2, owner: 2}

    # ключевой DoD: resolve_taste даёт разные профили, каждый в свою сторону
    index = load_index()
    assert index.size == 4
    v_alice, src_alice = resolve_taste(index, user_id=alice)
    v_owner, src_owner = resolve_taste(index)
    assert src_alice in {"history_rebuild", "offline_report"}
    assert src_owner in {"history_rebuild", "offline_report"}
    assert float(v_alice @ e1) > 0.99 and float(v_alice @ e2) < 0.1
    assert float(v_owner @ e2) > 0.99 and float(v_owner @ e1) < 0.1
    assert not np.allclose(v_alice, v_owner)

    # персист per-user: две живые строки offline_report с разными векторами
    p_alice = build_profile(persist=True, user_id=alice)
    p_owner = build_profile(persist=True)
    assert p_alice.n_positive == 2 and p_owner.n_positive == 2
    with connect() as conn:
        rows = conn.execute(
            """
            SELECT user_id, n_positive FROM user_taste_profiles
            WHERE context = 'offline_report'
            """
        ).fetchall()
    assert {(int(r["user_id"]), int(r["n_positive"])) for r in rows} == {
        (alice, 2),
        (owner, 2),
    }
    # после персиста resolve берёт offline_report, вектор тот же
    v2_alice, src2 = resolve_taste(index, user_id=alice)
    assert src2 == "offline_report"
    assert np.allclose(v2_alice, v_alice, atol=1e-6)


def test_pg_shelves_are_instance_wide(pg_env, tmp_path):
    """Полки каталога (mix_pack/for_you/…) — инстанс-wide: от владельца,
    не зависят от слушания других пользователей и не дублируются по ним."""
    from music_hive.brain import mixes
    from music_hive.brain.generators import generate_daily
    from music_hive.brain.mixes import (
        _for_you_forbidden_rows,
        generate_mix_pack,
        mix_catalog,
    )
    from music_hive.brain.store import latest_playlist
    from music_hive.db import backend
    from music_hive.db.schema import connect
    from music_hive.index.brute import load_index
    from music_hive.listen.history import record_listen

    ids = _seed_catalog(tmp_path, n_left=6, n_right=6)
    owner = backend.owner_id()
    alice = _create_user("alice")

    generate_mix_pack(
        daily_size=2,
        for_you_size=3,
        weekday_size=2,
        weekly_size=3,
        new_size=2,
    )

    # ровно один набор полков, все — у владельца
    kinds = ["for_you", "daily", "new_releases", "weekly", *[k for k, _t, _w in mixes.WEEKDAY_KEYS]]
    with connect() as conn:
        shelves = {
            (r["kind"], int(r["owner_user_id"]))
            for r in conn.execute(
                "SELECT kind, owner_user_id FROM playlists"
            ).fetchall()
        }
    for kind in kinds:
        assert (kind, owner) in shelves
    assert all(owner_id == owner for _kind, owner_id in shelves)

    # слушание alice не меняет exclusion-набор полки for_you
    index = load_index()
    before = _for_you_forbidden_rows(index, recent_days=7)
    wd_today = datetime.now(UTC).weekday()
    wd_vec_before, wd_src_before, wd_n_before = mixes._weekday_taste(index, wd_today)
    record_listen(ids[0], "finish", user_id=alice)
    record_listen(ids[1], "like", user_id=alice)
    after = _for_you_forbidden_rows(index, recent_days=7)
    assert before == after
    # и не меняет weekday-вкус полок (сегодняшний день — самый чувствительный)
    wd_vec_after, wd_src_after, wd_n_after = mixes._weekday_taste(index, wd_today)
    assert np.allclose(wd_vec_before, wd_vec_after, atol=1e-6)
    assert (wd_src_before, wd_n_before) == (wd_src_after, wd_n_after)

    # личный daily у alice не подменяет полку владельца
    owner_daily_id = latest_playlist("daily")["id"]
    personal = generate_daily(size=2, user_id=alice)
    personal.persist()
    with connect() as conn:
        daily_owners = {
            int(r["owner_user_id"])
            for r in conn.execute(
                "SELECT owner_user_id FROM playlists WHERE kind = 'daily'"
            ).fetchall()
        }
    assert daily_owners == {owner, alice}
    assert latest_playlist("daily")["id"] == owner_daily_id
    daily_card = next(c for c in mix_catalog() if c["kind"] == "daily")
    assert daily_card["playlist_id"] == owner_daily_id


def test_pg_cli_user_option(pg_env, tmp_path):
    """CLI --user: запись от имени пользователя; неизвестный — ошибка; default — владелец."""
    from typer.testing import CliRunner

    from music_hive.cli import app
    from music_hive.db import backend, ensure_db
    from music_hive.db.schema import connect
    from music_hive.scanner import scan_library

    lib = tmp_path / "lib"
    lib.mkdir(exist_ok=True)
    _make_wav(lib / "c.wav", freq=500)
    ensure_db()
    scan_library(lib, extract_audio=False, workers=1)
    with connect() as conn:
        tid = int(
            conn.execute("SELECT id FROM tracks ORDER BY id LIMIT 1").fetchone()["id"]
        )

    alice = _create_user("alice")
    owner = backend.owner_id()

    runner = CliRunner()
    r1 = runner.invoke(app, ["listen", "like", str(tid), "--user", "alice"])
    assert r1.exit_code == 0, r1.output
    r2 = runner.invoke(app, ["listen", "finish", str(tid)])  # default — владелец
    assert r2.exit_code == 0, r2.output
    r3 = runner.invoke(app, ["listen", "like", str(tid), "--user", "ghost"])
    assert r3.exit_code == 1
    assert "не найден" in r3.output

    with connect() as conn:
        per_user = {
            int(r["user_id"]): (r["action"],)
            for r in conn.execute(
                "SELECT user_id, action FROM listening_history ORDER BY id"
            ).fetchall()
        }
    assert per_user == {alice: ("like",), owner: ("finish",)}


def test_pg_daily_job_payload_user(pg_env, tmp_path):
    """job daily с payload.user_id → персональный плейлист этого пользователя."""
    from music_hive.db import backend
    from music_hive.db.schema import connect
    from music_hive.jobs.queue import enqueue_job
    from music_hive.jobs.runner import run_one

    _seed_catalog(tmp_path, n_left=2, n_right=1)
    alice = _create_user("alice")
    owner = backend.owner_id()

    enqueue_job("daily", {"size": 2, "user_id": alice})
    summary = run_one()
    assert summary is not None and summary["status"] == "done", summary

    with connect() as conn:
        rows = conn.execute(
            "SELECT owner_user_id FROM playlists WHERE kind = 'daily'"
        ).fetchall()
    assert [int(r["owner_user_id"]) for r in rows] == [alice]
    assert alice != owner
