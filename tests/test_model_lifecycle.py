"""F4.3 (GitLab #29): жизненный цикл моделей — jobs model_activate/model_status,
пересчёт вкуса всех пользователей, CLI models.

PG-часть — по паттерну conftest.pg_env (MUSIC_HIVE_TEST_DATABASE_URL),
полная смена модели — на toy-ONNX из F4.2 (extra onnx). Без PG переменной
PG-тесты пропускаются.
"""

from __future__ import annotations

import os
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf
from test_embed_onnx import (
    TOY_DIM,
    TOY_SR,
    TOY_WINDOW_SEC,
    _build_toy_waveform_onnx,
    _write_cfg,
    needs_onnx,
)
from test_pg_users import _create_user
from test_pg_worker import _make_wav, _unit_vec

PG_URL = os.environ.get("MUSIC_HIVE_TEST_DATABASE_URL", "").strip()
pg_only = pytest.mark.skipif(
    not PG_URL, reason="MUSIC_HIVE_TEST_DATABASE_URL не задан — PG-тесты пропущены"
)

KEY_CLAP = "clap:laion/larger_clap_music_and_speech"


@pytest.fixture
def sqlite_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path):
    """Изолированные пути (без БД): для фабричных тестов конфига."""
    monkeypatch.delenv("MUSIC_HIVE_ONNX_DIR", raising=False)
    monkeypatch.setenv("MUSIC_HIVE_LIBRARY", str(tmp_path / "lib"))
    monkeypatch.setenv("MUSIC_HIVE_DATA_DIR", str(tmp_path / "data"))
    monkeypatch.setenv(
        "MUSIC_HIVE_ARTWORK_CACHE", str(tmp_path / "data" / "cache" / "artwork")
    )
    from music_hive.config import get_settings
    from music_hive.db import backend

    get_settings.cache_clear()
    backend.reset_pool()
    yield
    get_settings.cache_clear()
    backend.reset_pool()


# ------------------------------------------------------------- kinds/reload


def test_job_kinds_and_reload_kinds():
    """Новые kinds зарегистрированы; reload-инвалидация — после model_activate."""
    from music_hive.jobs.runner import _RELOAD_KINDS, JOB_KINDS

    assert {"model_activate", "model_status"} <= JOB_KINDS
    assert "model_activate" in _RELOAD_KINDS
    # model_status ничего не меняет — reload не нужен
    assert "model_status" not in _RELOAD_KINDS


def test_get_encoder_resolves_full_clap_key(sqlite_env, monkeypatch: pytest.MonkeyPatch):
    """Дефект F4.3: фабрика обязана собирать энкодер по ПОЛНОМУ ключу
    'clap:<repo>' (model_key() == ключ), иначе model_activate чужого repo
    не может закодировать векторы в таблицу этого ключа. 'clap' — как
    раньше, дефолтный repo воркера. Конструктор ленивый — без encode."""
    from music_hive.config import get_settings
    from music_hive.embed.base import get_encoder

    monkeypatch.delenv("MUSIC_HIVE_CLAP_MODEL", raising=False)
    get_settings.cache_clear()

    enc = get_encoder("clap:fake/repo")
    assert enc.model_key() == "clap:fake/repo"
    # имя фабрики — по-прежнему дефолтный repo из настроек (контракт F4.1)
    assert get_encoder("clap").model_key() == f"clap:{get_settings().clap_model}"


# ----------------------------------------------------------------- CLI (PG)


def test_pg_cli_models_lists_registry(pg_env, tmp_path: Path):
    """CLI `models` — таблица реестра с честными цифрами."""
    from typer.testing import CliRunner

    from music_hive.cli import app
    from music_hive.db import ensure_db, save_embedding, upsert_track
    from music_hive.db.store import model_key

    runner = CliRunner()
    ensure_db()
    tid = upsert_track(
        {
            "path": str(tmp_path / "a.wav"),
            "file_md5": "a" * 32,
            "file_mtime": 0.0,
            "file_size": 1,
            "title": "a",
            "artist": "A",
            "album": None,
            "year": None,
            "track_number": None,
            "duration": 1.0,
            "bitrate": None,
            "sample_rate": None,
            "channels": None,
            "fingerprint": None,
            "lufs": None,
            "artwork_path": None,
        }
    )
    # регистрируем активную модель ключом config-энкодера
    save_embedding(tid, _unit_vec(8, seed=0))
    assert model_key() == KEY_CLAP

    r = runner.invoke(app, ["models"])
    assert r.exit_code == 0, r.output
    assert KEY_CLAP in r.output
    assert "pending" in r.output


# --------------------------------------------------------------------- PG


@pg_only
def test_pg_model_status_honest_numbers(pg_env, tmp_path: Path):
    """model_status: active/dim/vectors/pending — ровно состояние базы."""
    from music_hive.db import ensure_db, save_embedding
    from music_hive.db.schema import connect
    from music_hive.db.store import active_model
    from music_hive.jobs.queue import enqueue_job
    from music_hive.jobs.runner import run_one
    from music_hive.scanner import scan_library

    lib = tmp_path / "lib"
    lib.mkdir()
    for i in range(3):
        _make_wav(lib / f"t{i}.wav", freq=440 + 100 * i)
    ensure_db()
    scan_library(lib, extract_audio=False, workers=1)
    with connect() as conn:
        ids = [
            int(r["id"])
            for r in conn.execute("SELECT id FROM tracks ORDER BY id").fetchall()
        ]
    for i, tid in enumerate(ids[:2]):  # векторы — только у двух из трёх
        save_embedding(tid, _unit_vec(8, seed=i))

    enqueue_job("model_status", {})
    summary = run_one()
    assert summary is not None and summary["status"] == "done", summary
    assert summary["result"] == {
        "active": KEY_CLAP,
        "models": [{"key": KEY_CLAP, "dim": 8, "active": True, "vectors": 2}],
        "pending": 1,  # третий трек без вектора активной модели
    }
    # status — срез, ничего не переключает
    assert active_model()["model_key"] == KEY_CLAP


@pg_only
def test_pg_model_activate_unknown_model_fails_and_keeps_active(pg_env, tmp_path: Path):
    """Активация незарегистрированной/несуществующей модели — job failed,
    прежняя активная остаётся (fail до флипа реестра)."""
    from music_hive.db import ensure_db, save_embedding
    from music_hive.db.schema import connect
    from music_hive.db.store import active_model
    from music_hive.jobs.queue import enqueue_job
    from music_hive.jobs.runner import run_one
    from music_hive.scanner import scan_library

    lib = tmp_path / "lib"
    lib.mkdir()
    _make_wav(lib / "x.wav", freq=440)
    ensure_db()
    scan_library(lib, extract_audio=False, workers=1)
    with connect() as conn:
        tid = int(
            conn.execute("SELECT id FROM tracks ORDER BY id LIMIT 1").fetchone()["id"]
        )
    save_embedding(tid, _unit_vec(8, seed=1))
    assert active_model()["model_key"] == KEY_CLAP

    enqueue_job("model_activate", {"model_key": "onnx:ghost"})
    summary = run_one()
    assert summary is not None and summary["status"] == "failed"
    assert active_model()["model_key"] == KEY_CLAP  # флипа не было


@pg_only
@needs_onnx
def test_pg_model_activate_switches_model_and_recomputes_taste(
    pg_env, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    """DoD F4.3: model_activate переключает активную на onnx:toy, добирает
    векторы реальным toy-энкодером (конфиг воркера остаётся clap), обе
    emb_* изолированы, вкус пересчитан у обоих пользователей по-разному."""
    from music_hive.config import get_settings
    from music_hive.db import backend, ensure_db, pending_count, save_embedding
    from music_hive.db.schema import connect
    from music_hive.db.store import (
        active_model,
        emb_table_name,
        get_embedding,
        model_key,
    )
    from music_hive.index.brute import load_index
    from music_hive.jobs.queue import enqueue_job
    from music_hive.jobs.runner import run_one
    from music_hive.listen.history import record_listen
    from music_hive.listen.profile import (
        CONTEXT_OFFLINE,
        build_profile,
        resolve_taste,
    )
    from music_hive.scanner import scan_library

    # toy-энкодер доступен фабрике, но конфиг воркера — по-прежнему clap:
    # job обязан кодировать моделью из payload, а не из env воркера
    models = tmp_path / "models.d"
    models.mkdir()
    _build_toy_waveform_onnx(models / "toy.onnx")
    _write_cfg(models, dim=TOY_DIM)
    monkeypatch.setenv("MUSIC_HIVE_ONNX_DIR", str(models))
    monkeypatch.setenv("MUSIC_HIVE_EMBED_SEGMENT_SEC", str(TOY_WINDOW_SEC))
    get_settings.cache_clear()
    assert model_key() == KEY_CLAP

    lib = tmp_path / "lib"
    lib.mkdir()
    for name, freq in (("a.wav", 440.0), ("b.wav", 880.0)):
        t = np.linspace(0, TOY_WINDOW_SEC, int(TOY_SR * TOY_WINDOW_SEC), endpoint=False)
        sf.write(
            lib / name, (0.2 * np.sin(2 * np.pi * freq * t)).astype(np.float32), TOY_SR
        )
    ensure_db()
    scan_library(lib, extract_audio=False, workers=1)
    with connect() as conn:
        ids = [
            int(r["id"])
            for r in conn.execute("SELECT id FROM tracks ORDER BY id").fetchall()
        ]

    # модель A (clap) активна, оба трека «готовы» фейковыми векторами
    for i, tid in enumerate(ids):
        save_embedding(tid, _unit_vec(8, seed=100 + i))
    assert active_model()["model_key"] == KEY_CLAP

    # два пользователя с противоположным прослушиванием; профили под clap
    alice = _create_user("alice")
    owner = backend.owner_id()
    assert alice != owner
    record_listen(ids[0], "like", user_id=alice)
    record_listen(ids[1], "like")  # default → владелец
    build_profile(persist=True, user_id=alice)
    build_profile(persist=True)
    with connect() as conn:
        before = {
            int(r["user_id"])
            for r in conn.execute(
                "SELECT user_id FROM user_taste_profiles WHERE model_key = %s",
                (KEY_CLAP,),
            ).fetchall()
        }
    assert before == {alice, owner}

    # --- job model_activate: onnx:toy
    enqueue_job("model_activate", {"model_key": "onnx:toy"})
    summary = run_one()
    assert summary is not None and summary["status"] == "done", summary
    res = summary["result"]
    assert res["model_key"] == "onnx:toy"
    assert res["embed"]["computed"] == 2 and res["embed"]["failed"] == 0
    with connect() as conn:
        n_users = int(
            conn.execute("SELECT COUNT(*) AS n FROM users").fetchone()["n"]
        )
    assert res["taste"]["users"] == n_users  # все пользователи, не только владелец

    # активная — toy; векторы toy реальные (toy-граф), emb_clap нетронута
    assert active_model()["model_key"] == "onnx:toy"
    table_toy = emb_table_name("onnx:toy")
    table_clap = emb_table_name(KEY_CLAP)
    with connect() as conn:
        n_toy = int(
            conn.execute(
                f"SELECT COUNT(*) AS n FROM {table_toy} WHERE status = 'ready'"
            ).fetchone()["n"]
        )
        n_clap = int(
            conn.execute(
                f"SELECT COUNT(*) AS n FROM {table_clap} WHERE status = 'ready'"
            ).fetchone()["n"]
        )
    assert (n_toy, n_clap) == (2, 2)  # изолированные таблицы
    vec_a = get_embedding(ids[0])  # читает из таблицы активной модели
    assert vec_a is not None and vec_a.shape == (TOY_DIM,)
    assert abs(float(np.linalg.norm(vec_a)) - 1.0) < 1e-5
    assert not np.allclose(vec_a, _unit_vec(8, seed=100), atol=1e-3)  # не фейк clap
    assert not np.allclose(vec_a, get_embedding(ids[1]))  # контент-зависимые

    # вкус пересчитан у обоих под ключ активной модели — и по-разному:
    # alice лайкала трек a, владелец — трек b (по одному лайку каждый)
    with connect() as conn:
        taste = {
            int(r["user_id"]): r["vec"]
            for r in conn.execute(
                """
                SELECT user_id, vec FROM user_taste_profiles
                WHERE model_key = %s AND context = %s
                """,
                ("onnx:toy", CONTEXT_OFFLINE),
            ).fetchall()
        }
    assert set(taste) == {alice, owner}
    index = load_index()
    assert index.size == 2 and index.dim == TOY_DIM
    row_a = index.row_of(ids[0])
    row_b = index.row_of(ids[1])
    assert float(taste[alice] @ index.matrix[row_a]) > 0.99
    assert float(taste[owner] @ index.matrix[row_b]) > 0.99
    assert not np.allclose(taste[alice], taste[owner])  # «по-разному»

    # после переключения resolve_taste берёт свежий offline_report активной модели
    v, src = resolve_taste(user_id=alice)
    assert src == "offline_report"
    assert np.allclose(v, taste[alice], atol=1e-6)

    # недостающее закрыто: pending по активной модели — ноль
    assert pending_count("onnx:toy") == 0


@pg_only
def test_pg_model_activate_clap_key_writes_under_full_key(
    pg_env, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    """Дефект приёмки F4.3: model_activate по clap-ключу писал векторы в
    фантомную emb_clap (имя фабрики) вместо таблицы ПОЛНОГО ключа → вечный
    pending. Здесь: активен дефолтный clap → активация 'clap:fake/repo' —
    векторы обязаны лечь в emb_clap_fake_repo, фантомной emb_clap нет,
    pending=0. Энкодер — стаб с честным contractом фабрики (полный ключ →
    его же model_key), реальная ClapEncoder не грузится."""
    from music_hive.db import ensure_db, pending_count, save_embedding
    from music_hive.db.schema import connect
    from music_hive.db.store import active_model, emb_table_name
    from music_hive.jobs.queue import enqueue_job
    from music_hive.jobs.runner import run_one
    from music_hive.scanner import scan_library

    requested: list[str | None] = []

    class _StubEncoder:
        sample_rate = 22050

        def __init__(self, key: str) -> None:
            self._key = key

        def model_key(self) -> str:
            return self._key

        def encode(self, segments: list[np.ndarray]) -> np.ndarray:
            return _unit_vec(8, seed=len(segments))

    def _fake_get_encoder(name: str | None = None) -> _StubEncoder:
        from music_hive.config import get_settings

        requested.append(name)
        if name is None or name == "clap":
            return _StubEncoder(f"clap:{get_settings().clap_model}")
        return _StubEncoder(str(name))

    monkeypatch.setattr("music_hive.embed.pipeline.get_encoder", _fake_get_encoder)

    lib = tmp_path / "lib"
    lib.mkdir()
    _make_wav(lib / "a.wav", freq=440)
    _make_wav(lib / "b.wav", freq=880)
    ensure_db()
    scan_library(lib, extract_audio=False, workers=1)
    with connect() as conn:
        ids = [
            int(r["id"])
            for r in conn.execute("SELECT id FROM tracks ORDER BY id").fetchall()
        ]

    # отправная точка: активен дефолтный clap, оба трека готовы фейками
    for i, tid in enumerate(ids):
        save_embedding(tid, _unit_vec(8, seed=i))
    assert active_model()["model_key"] == KEY_CLAP

    key_foreign = "clap:fake/repo"
    enqueue_job("model_activate", {"model_key": key_foreign})
    summary = run_one()
    assert summary is not None and summary["status"] == "done", summary
    res = summary["result"]
    assert res["embed"]["computed"] == 2 and res["embed"]["failed"] == 0
    # фабрика получила ПОЛНЫЙ ключ payload'а, а не имя фабрики 'clap'
    assert requested == [key_foreign]

    # векторы — в таблице ПОЛНОГО ключа; фантомной emb_clap (и строки 'clap'
    # в реестре) не появилось; активация закрыла pending
    assert active_model()["model_key"] == key_foreign
    table = emb_table_name(key_foreign)
    assert table == "emb_clap_fake_repo"
    with connect() as conn:
        n_vec = int(
            conn.execute(
                f"SELECT COUNT(*) AS n FROM {table} WHERE status = 'ready'"
            ).fetchone()["n"]
        )
        phantom_table = conn.execute(
            "SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = 'emb_clap'"
        ).fetchone()
        phantom_row = conn.execute(
            "SELECT 1 FROM embedding_models WHERE model_key = 'clap'"
        ).fetchone()
    assert n_vec == 2
    assert phantom_table is None
    assert phantom_row is None
    assert pending_count(key_foreign) == 0


@pg_only
def test_pg_list_models_registry_row_without_table(pg_env):
    """Харденинг F4.3: строка реестра без физической emb_<slug> (таблицу
    создаёт первая запись векторов) не должна ронять list_models в
    UndefinedTable — /api/admin/models и CLI models обязаны отвечать."""
    from music_hive.db import ensure_db, list_models
    from music_hive.db.schema import connect

    ensure_db()
    with connect() as conn:
        conn.execute(
            """
            INSERT INTO embedding_models (model_key, backend, repo, dim)
            VALUES (%s, 'clap', %s, 512)
            """,
            ("clap:ghost/repo", "ghost/repo"),
        )

    models = list_models()  # раньше: psycopg UndefinedTable
    entry = next(m for m in models if m["key"] == "clap:ghost/repo")
    assert entry == {"key": "clap:ghost/repo", "dim": 512, "active": False, "vectors": 0}
