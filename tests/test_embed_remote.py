"""F31 (#31): remote HTTP-энкодер — контракт с model-сервером #32, encode_file-путь.

Мок-сервер (http.server в фикстуре, без сети наружу) отвечает по
зафиксированному контракту: GET /v1/models, POST /v1/embeddings?model=<name>
(тело — файл целиком). Полный цикл с БД — PG (pg_env), #11 F5.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace

import numpy as np
import pytest
import soundfile as sf

PG_URL = os.environ.get("MUSIC_HIVE_TEST_DATABASE_URL", "").strip()

SERVER_KEY = "test:mock-model"
VEC4 = [0.5, 0.5, 0.5, 0.5]  # L2=1 — проходит валидацию §6.1 без ренорма


def _make_wav(path: Path, seconds: float = 1.0, freq: float = 440.0) -> None:
    sr = 22_050
    t = np.linspace(0, seconds, int(sr * seconds), endpoint=False)
    y = (0.2 * np.sin(2 * np.pi * freq * t)).astype(np.float32)
    sf.write(path, y, sr)


# ------------------------------------------------------------ мок-сервер #32


class _Handler(BaseHTTPRequestHandler):
    """Ответы контракта #32; поведение крутится через server.state."""

    def log_message(self, *args: object) -> None:  # тише в выводе pytest
        pass

    @property
    def state(self) -> dict:
        return self.server.state  # type: ignore[attr-defined]

    def _json(self, obj: object, status: int = 200) -> None:
        body = json.dumps(obj).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _record(self, **entry: object) -> None:
        self.state["requests"].append(entry)

    def do_GET(self) -> None:
        st = self.state
        self._record(method="GET", path=self.path)
        if st.get("sleep"):
            time.sleep(st["sleep"])
        if self.path == "/v1/models":
            self._json({"models": st["models"]}, st.get("models_status", 200))
        else:
            self._json({"error": "not found"}, 404)

    def do_POST(self) -> None:
        st = self.state
        n = int(self.headers.get("Content-Length", 0))
        self._record(
            method="POST",
            path=self.path,
            auth=self.headers.get("Authorization"),
            content_type=self.headers.get("Content-Type"),
            body=self.rfile.read(n),
        )
        if st.get("sleep"):
            time.sleep(st["sleep"])
        if st.get("embed_status"):
            self._json(st.get("embed_error", {"error": "boom"}), st["embed_status"])
            return
        dim = st["dim"] if st.get("dim") is not None else len(st["vector"])
        self._json(
            {
                "model_key": st["model_key"],
                "dim": dim,
                "embedding": list(st["vector"]),
            }
        )


@pytest.fixture
def mock_server():
    """Живой локальный HTTP-мок #32: url + мутируемый state."""
    state: dict = {
        "models": [
            {"name": "clap", "model_key": SERVER_KEY, "dim": 4, "loaded": True}
        ],
        "model_key": SERVER_KEY,
        "vector": VEC4,
        "requests": [],
    }
    srv = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
    srv.state = state  # type: ignore[attr-defined]
    thread = threading.Thread(target=srv.serve_forever, daemon=True)
    thread.start()
    try:
        yield SimpleNamespace(
            url=f"http://127.0.0.1:{srv.server_address[1]}", state=state
        )
    finally:
        srv.shutdown()
        srv.server_close()


# ------------------------------------------------------- модель/ключ/кеш GET


def test_model_key_from_server_and_cached(mock_server):
    from music_hive.embed.remote import RemoteEncoder

    enc = RemoteEncoder(mock_server.url, "clap", "tok")
    assert enc.sample_rate == 0  # декод — на сервере, локальной частоты нет
    assert enc.model_key() == SERVER_KEY
    assert enc.model_key() == SERVER_KEY  # кеш: второй вызов без GET
    gets = [r for r in mock_server.state["requests"] if r["method"] == "GET"]
    assert len(gets) == 1
    assert gets[0]["path"] == "/v1/models"
    enc.warm_up()  # доступность: тот же кеш, новых запросов нет
    assert len([r for r in mock_server.state["requests"] if r["method"] == "GET"]) == 1


def test_model_key_missing_model_lists_available(mock_server):
    from music_hive.embed.remote import RemoteEncoder

    enc = RemoteEncoder(mock_server.url, "muq", "tok")
    with pytest.raises(RuntimeError, match=r"модели 'muq' нет.*clap"):
        enc.model_key()


def test_encode_segments_not_supported(mock_server):
    """Протокол encode(segments) для remote неприменим — внятный отказ."""
    from music_hive.embed.remote import RemoteEncoder

    enc = RemoteEncoder(mock_server.url, "clap", "tok")
    enc.model_key()
    with pytest.raises(RuntimeError, match="encode_file"):
        enc.encode([np.zeros(4, dtype=np.float32)])


# ------------------------------------------------------------ encode_file


def test_encode_file_posts_whole_file_bytes(mock_server, tmp_path: Path):
    """(б) Сервер получает ФАЙЛ целиком (raw bytes), не сегменты."""
    from music_hive.embed.remote import RemoteEncoder

    wav = tmp_path / "x.wav"
    _make_wav(wav, freq=440)
    raw = wav.read_bytes()

    enc = RemoteEncoder(mock_server.url, "clap", "tok")
    vec = enc.encode_file(wav)

    posts = [r for r in mock_server.state["requests"] if r["method"] == "POST"]
    assert len(posts) == 1
    assert posts[0]["path"] == "/v1/embeddings?model=clap"
    assert posts[0]["auth"] == "Bearer tok"
    assert posts[0]["content_type"] == "application/octet-stream"
    assert posts[0]["body"] == raw  # байты файла 1-в-1, без локального декода

    assert vec.shape == (4,) and vec.dtype == np.float32
    assert abs(float(np.linalg.norm(vec)) - 1.0) < 1e-6
    assert np.allclose(vec, VEC4, atol=1e-6)


def test_encode_file_validated_and_guards(mock_server, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    """Фабричная обёртка валидирует §6.1; серверные подвохи — RuntimeError."""
    from music_hive.config import get_settings
    from music_hive.embed.base import get_encoder

    monkeypatch.setenv("MUSIC_HIVE_REMOTE_TOKEN", "tok")
    get_settings.cache_clear()
    wav = tmp_path / "x.wav"
    _make_wav(wav)
    try:
        enc = get_encoder(f"remote:{mock_server.url}")
        enc.model_key()

        # NaN от сервера → ValueError (валидация в ValidatedEncoder, не мусор в БД)
        mock_server.state["vector"] = [float("nan")] * 4
        with pytest.raises(ValueError, match="NaN/Inf"):
            enc.encode_file(wav)

        # сервер сменил model_key под нами → чужой вектор не принимаем
        mock_server.state["vector"] = VEC4
        mock_server.state["model_key"] = "other:model"
        with pytest.raises(RuntimeError, match="other:model"):
            enc.encode_file(wav)

        # dim в ответе противоречит длине embedding (сервер врёт)
        mock_server.state["model_key"] = SERVER_KEY
        mock_server.state["dim"] = 5
        mock_server.state["vector"] = VEC4
        with pytest.raises(RuntimeError, match="dim"):
            enc.encode_file(wav)
    finally:
        get_settings.cache_clear()


def test_encode_file_empty_embedding(mock_server, tmp_path: Path):
    from music_hive.embed.remote import RemoteEncoder

    wav = tmp_path / "x.wav"
    _make_wav(wav)
    mock_server.state["vector"] = []
    enc = RemoteEncoder(mock_server.url, "clap", "tok")
    with pytest.raises(RuntimeError, match="пустой embedding"):
        enc.encode_file(wav)


# ------------------------------------------------------------------ ошибки HTTP


@pytest.mark.parametrize("status", [401, 404, 415, 500])
def test_http_errors_are_clear(mock_server, tmp_path: Path, status: int):
    from music_hive.embed.remote import RemoteEncoder

    wav = tmp_path / "x.wav"
    _make_wav(wav)
    mock_server.state["embed_status"] = status
    enc = RemoteEncoder(mock_server.url, "clap", "tok")
    with pytest.raises(RuntimeError, match=str(status)):
        enc.encode_file(wav)


def test_http_401_mentions_token(mock_server, tmp_path: Path):
    from music_hive.embed.remote import RemoteEncoder

    wav = tmp_path / "x.wav"
    _make_wav(wav)
    mock_server.state["embed_status"] = 401
    enc = RemoteEncoder(mock_server.url, "clap", "tok")
    with pytest.raises(RuntimeError, match="MUSIC_HIVE_REMOTE_TOKEN"):
        enc.encode_file(wav)


# --------------------------------------------------- (в) недоступен/таймаут


def test_unreachable_server_clear_error():
    from music_hive.embed.remote import RemoteEncoder

    # порт 9 (discard) закрыт — connection refused
    enc = RemoteEncoder("http://127.0.0.1:9", "clap", "tok")
    with pytest.raises(RuntimeError, match=r"недоступен.*127\.0\.0\.1:9"):
        enc.model_key()
    with pytest.raises(RuntimeError, match="недоступен"):
        enc.encode_file(Path("/etc/hostname"))


def test_timeout_clear_error(mock_server, monkeypatch: pytest.MonkeyPatch):
    import music_hive.embed.remote as remote_mod
    from music_hive.embed.remote import RemoteEncoder

    mock_server.state["sleep"] = 5.0
    monkeypatch.setattr(remote_mod, "LIST_TIMEOUT", 0.3)
    enc = RemoteEncoder(mock_server.url, "clap", "tok")
    with pytest.raises(RuntimeError, match="недоступен"):
        enc.model_key()


# ------------------------------------------------------- (в) ошибки сервера


def test_pipeline_marks_failed_on_server_error(
    pg_env, mock_server, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    """(в) Сервер падает на embeddings → track_audio_features=failed с внятной ошибкой."""
    from music_hive.config import get_settings
    from music_hive.db import ensure_db
    from music_hive.db.schema import connect
    from music_hive.embed.pipeline import embed_library
    from music_hive.scanner import scan_library

    monkeypatch.setenv("MUSIC_HIVE_EMBEDDING_MODEL", f"remote:{mock_server.url}")
    monkeypatch.setenv("MUSIC_HIVE_REMOTE_TOKEN", "tok")
    get_settings.cache_clear()
    mock_server.state["embed_status"] = 500
    lib = tmp_path / "lib"
    lib.mkdir()
    _make_wav(lib / "a.wav")
    ensure_db()
    scan_library(lib, extract_audio=False, workers=1)

    res = embed_library()
    assert res.failed == 1 and res.computed == 0
    with connect() as conn:
        row = conn.execute(
            "SELECT status, error FROM track_audio_features LIMIT 1"
        ).fetchone()
    assert row["status"] == "failed"
    assert "500" in row["error"]


# ------------------------------------------------------------------- (г) фабрика


def test_factory_no_token_value_error(monkeypatch: pytest.MonkeyPatch):
    from music_hive.config import get_settings
    from music_hive.embed.base import get_encoder

    monkeypatch.delenv("MUSIC_HIVE_REMOTE_TOKEN", raising=False)
    get_settings.cache_clear()
    try:
        with pytest.raises(ValueError, match="MUSIC_HIVE_REMOTE_TOKEN"):
            get_encoder("remote:http://127.0.0.1:9")
    finally:
        get_settings.cache_clear()


def test_factory_reads_remote_model_env(mock_server, monkeypatch: pytest.MonkeyPatch):
    from music_hive.config import get_settings
    from music_hive.embed.base import get_encoder
    from music_hive.embed.remote import RemoteEncoder

    monkeypatch.setenv("MUSIC_HIVE_REMOTE_TOKEN", "tok")
    monkeypatch.setenv("MUSIC_HIVE_REMOTE_MODEL", "muq")
    get_settings.cache_clear()
    try:
        enc = get_encoder(f"remote:{mock_server.url}")
        assert isinstance(enc.inner, RemoteEncoder)  # type: ignore[attr-defined]
        assert enc.inner.model_name == "muq"  # type: ignore[attr-defined]
        with pytest.raises(RuntimeError, match="muq"):
            enc.model_key()
    finally:
        get_settings.cache_clear()


def test_factory_wraps_remote_in_validated_encoder(mock_server, monkeypatch: pytest.MonkeyPatch):
    """Фабрика возвращает обёртку с encode_file (валидация §6.1 на месте)."""
    from music_hive.config import get_settings
    from music_hive.embed.base import ValidatedEncoder, get_encoder
    from music_hive.embed.remote import RemoteEncoder

    monkeypatch.setenv("MUSIC_HIVE_REMOTE_TOKEN", "tok")
    get_settings.cache_clear()
    try:
        enc = get_encoder(f"remote:{mock_server.url}")
        assert isinstance(enc, ValidatedEncoder)
        assert isinstance(enc.inner, RemoteEncoder)
        assert callable(getattr(enc, "encode_file", None))
    finally:
        get_settings.cache_clear()


def test_segment_backends_do_not_expose_encode_file():
    from music_hive.embed.base import ValidatedEncoder

    class SegOnly:
        sample_rate = 48_000

        def model_key(self) -> str:
            return "fake:v1"

        def encode(self, segments: list[np.ndarray]) -> np.ndarray:
            return np.ones(4, dtype=np.float32)

    wrapper = ValidatedEncoder(SegOnly())
    assert not hasattr(wrapper, "encode_file"), (
        "pipeline duck-typing уведёт сегментные бэкенды по файловому пути"
    )


# ---------------------------------------------------- (д) без тяжёлых импортов


def test_remote_factory_is_lazy_no_heavy_imports(monkeypatch: pytest.MonkeyPatch):
    """Remote-режим не импортирует torch/transformers (паттерн F4.1)."""
    monkeypatch.setenv("MUSIC_HIVE_REMOTE_TOKEN", "tok")
    code = (
        "import sys\n"
        "from music_hive.embed import get_encoder\n"
        "from music_hive.embed import pipeline  # device_info из clap\n"
        "enc = get_encoder('remote:http://127.0.0.1:9/')\n"
        "assert 'transformers' not in sys.modules, 'transformers импортирован фабрикой'\n"
        "assert 'torch' not in sys.modules, 'torch импортирован фабрикой'\n"
        "print(type(enc.inner).__name__)\n"
    )
    proc = subprocess.run(
        [sys.executable, "-c", code],
        capture_output=True,
        text=True,
        check=True,
        cwd=Path(__file__).resolve().parents[1],
    )
    assert "RemoteEncoder" in proc.stdout


# ------------------------------------------------------- (е) полный цикл PG


@pytest.mark.skipif(
    not PG_URL, reason="MUSIC_HIVE_TEST_DATABASE_URL не задан — PG-тесты пропущены"
)
def test_pg_full_cycle_remote(pg_env, mock_server, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    """scan → embed (remote) → emb_test_mock_model с векторами, реестр обновлён."""
    from music_hive.config import get_settings
    from music_hive.db import ensure_db
    from music_hive.db.schema import connect
    from music_hive.db.store import (
        active_model,
        emb_table_name,
        get_embedding,
        list_tracks_needing_embedding,
    )
    from music_hive.embed.pipeline import embed_library
    from music_hive.scanner import scan_library

    monkeypatch.setenv("MUSIC_HIVE_EMBEDDING_MODEL", f"remote:{mock_server.url}")
    monkeypatch.setenv("MUSIC_HIVE_REMOTE_TOKEN", "tok")
    get_settings.cache_clear()

    lib = tmp_path / "lib"
    lib.mkdir()
    _make_wav(lib / "a.wav", freq=440)
    _make_wav(lib / "b.wav", freq=880)
    ensure_db()
    scan_library(lib, extract_audio=False, workers=1)

    res = embed_library()
    assert res.computed == 2 and res.failed == 0 and res.total == 2

    # сервер получил ровно 2 файла целиком (encode_file-путь, не сегменты)
    posts = [r for r in mock_server.state["requests"] if r["method"] == "POST"]
    assert len(posts) == 2
    assert {p["body"] for p in posts} == {
        (lib / "a.wav").read_bytes(),
        (lib / "b.wav").read_bytes(),
    }

    # реестр: серверный ключ зарегистрирован и активен (первая запись)
    am = active_model()
    assert am is not None
    assert am["model_key"] == SERVER_KEY and int(am["dim"]) == 4
    assert emb_table_name(SERVER_KEY) == "emb_test_mock_model"

    with connect() as conn:
        n_ready = int(
            conn.execute(
                "SELECT COUNT(*) AS n FROM emb_test_mock_model WHERE status = 'ready'"
            ).fetchone()["n"]
        )
        ids = [
            int(r["id"])
            for r in conn.execute("SELECT id FROM tracks ORDER BY id").fetchall()
        ]
    assert n_ready == 2
    for tid in ids:
        vec = get_embedding(tid)  # читает из таблицы активной модели
        assert vec is not None and vec.shape == (4,) and vec.dtype == np.float32
        assert abs(float(np.linalg.norm(vec)) - 1.0) < 1e-5

    # всё готово → повторный прогон без force не находит работы
    assert list_tracks_needing_embedding() == []
