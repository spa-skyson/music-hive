"""Тесты model-server (#32): контракт HTTP на мок-энкодерах + ffmpeg-декод.

Без реальных весов и без torch/muq: тяжёлые импорты в encoders.py ленивые
(внутри методов), app.py инъекция моков через create_app(encoders=...).
ffmpeg-тесты генерируют sine-mp3 (как tests/test_segments.py проекта) и
пропускаются, если ffmpeg/ffprobe отсутствуют.
"""

from __future__ import annotations

import os
import shutil
import subprocess

import numpy as np
import pytest

os.environ.setdefault("REMOTE_TOKEN", "ci-token")  # до import app (fail-closed)

from app import Settings, create_app, settings_from_env
from encoders import (
    CLAP_MODEL_KEY,
    MUQ_MODEL_KEY,
    DecodeError,
    decode_audio,
    ffprobe_duration,
    plan_windows,
    temp_audio,
)

TOKEN = "ci-token"
AUTH = {"Authorization": f"Bearer {TOKEN}"}
HAS_FFMPEG = bool(shutil.which("ffmpeg") and shutil.which("ffprobe"))


class FakeEncoder:
    """Мок контракта энкодера: «декодирует» только байты с маркером FAKE."""

    def __init__(
        self, name: str, model_key: str, dim: int, loaded: bool = False
    ) -> None:
        self.name = name
        self.model_key_val = model_key
        self.dim = dim
        self._loaded = loaded

    def model_key(self) -> str:
        return self.model_key_val

    @property
    def loaded(self) -> bool:
        return self._loaded

    def embed_file(self, data: bytes) -> np.ndarray:
        if not data.startswith(b"FAKE"):
            raise DecodeError("ffmpeg не декодировал аудио: invalid data")
        # ненормализованный вектор → app обязан вернуть L2-единичный
        vec = np.zeros(self.dim, dtype=np.float32)
        vec[0], vec[1] = 3.0, 4.0  # норма 5 → после L2: [0.6, 0.8, 0...]
        return vec


def make_client(**enc_kwargs) -> object:
    from fastapi.testclient import TestClient

    encoders = {
        "clap": FakeEncoder("clap", CLAP_MODEL_KEY, 512, **enc_kwargs),
        "muq": FakeEncoder("muq", MUQ_MODEL_KEY, 512, **enc_kwargs),
    }
    settings = Settings(
        token=TOKEN,
        models=("clap", "muq"),
        warmup=False,
        max_body_bytes=1024 * 1024,
        decode_timeout=30.0,
    )
    app = create_app(encoders=encoders, settings=settings)
    return TestClient(app)


# -- /health ---------------------------------------------------------------


def test_health_no_auth():
    with make_client() as client:
        r = client.get("/health")
        assert r.status_code == 200
        body = r.json()
        assert body["ok"] is True
        assert body["models_loaded"] == 0


def test_health_counts_loaded():
    from fastapi.testclient import TestClient

    encoders = {
        "clap": FakeEncoder("clap", CLAP_MODEL_KEY, 512, loaded=True),
        "muq": FakeEncoder("muq", MUQ_MODEL_KEY, 512, loaded=True),
    }
    app = create_app(
        encoders=encoders,
        settings=Settings(TOKEN, ("clap", "muq"), False, 1024, 30.0),
    )
    with TestClient(app) as client:
        assert client.get("/health").json() == {"ok": True, "models_loaded": 2}


# -- auth ------------------------------------------------------------------


def test_models_requires_auth():
    with make_client() as client:
        assert client.get("/v1/models").status_code == 401
        assert (
            client.get(
                "/v1/models", headers={"Authorization": "Bearer wrong"}
            ).status_code
            == 401
        )
        assert client.get("/v1/models", headers=AUTH).status_code == 200


def test_embeddings_requires_auth():
    with make_client() as client:
        assert client.post("/v1/embeddings?model=clap").status_code == 401
        assert (
            client.post(
                "/v1/embeddings?model=clap",
                content=b"FAKE",
                headers={"Authorization": "Bearer nope"},
            ).status_code
            == 401
        )


def test_fail_closed_without_token():
    with pytest.raises(RuntimeError, match="REMOTE_TOKEN"):
        create_app(
            settings=Settings(
                token="",
                models=("clap",),
                warmup=False,
                max_body_bytes=1,
                decode_timeout=1.0,
            )
        )


def test_settings_from_env_defaults():
    st = settings_from_env({"REMOTE_TOKEN": "t"})
    assert st.models == ("clap", "muq")
    assert st.warmup is True
    assert st.max_body_bytes == 100 * 1024 * 1024
    st0 = settings_from_env(
        {
            "REMOTE_TOKEN": "t",
            "WARMUP": "0",
            "MODELS": "muq",
            "MAX_BODY_MB": "1",
            "DECODE_TIMEOUT": "5",
        }
    )
    assert st0.warmup is False
    assert st0.models == ("muq",)
    assert st0.max_body_bytes == 1024 * 1024
    assert st0.decode_timeout == 5.0


# -- /v1/models ------------------------------------------------------------


def test_models_contract_exact():
    with make_client() as client:
        r = client.get("/v1/models", headers=AUTH)
        assert r.status_code == 200
        assert r.json() == {
            "models": [
                {
                    "name": "clap",
                    "model_key": CLAP_MODEL_KEY,
                    "dim": 512,
                    "loaded": False,
                },
                {
                    "name": "muq",
                    "model_key": MUQ_MODEL_KEY,
                    "dim": 512,
                    "loaded": False,
                },
            ]
        }


# -- /v1/embeddings ---------------------------------------------------------


def test_embeddings_happy_path_and_l2():
    with make_client() as client:
        r = client.post(
            "/v1/embeddings?model=clap", content=b"FAKE-AUDIO", headers=AUTH
        )
        assert r.status_code == 200
        body = r.json()
        assert body["model_key"] == CLAP_MODEL_KEY
        assert body["dim"] == 512
        emb = np.asarray(body["embedding"], dtype=np.float32)
        assert emb.shape == (512,)
        assert abs(float(np.linalg.norm(emb)) - 1.0) < 1e-5  # L2 сервисом
        assert abs(emb[0] - 0.6) < 1e-6 and abs(emb[1] - 0.8) < 1e-6


def test_embeddings_muq_key():
    with make_client() as client:
        r = client.post("/v1/embeddings?model=muq", content=b"FAKE", headers=AUTH)
        assert r.status_code == 200
        assert r.json()["model_key"] == MUQ_MODEL_KEY


def test_embeddings_unknown_model_404():
    with make_client() as client:
        r = client.post("/v1/embeddings?model=whisper", content=b"FAKE", headers=AUTH)
        assert r.status_code == 404
        assert "whisper" in r.json()["detail"]


def test_embeddings_missing_model_param_404():
    with make_client() as client:
        assert (
            client.post("/v1/embeddings", content=b"FAKE", headers=AUTH).status_code
            == 404
        )


def test_embeddings_undecodable_415():
    with make_client() as client:
        r = client.post(
            "/v1/embeddings?model=clap", content=b"not audio at all", headers=AUTH
        )
        assert r.status_code == 415


def test_embeddings_empty_body_415():
    with make_client() as client:
        r = client.post("/v1/embeddings?model=clap", content=b"", headers=AUTH)
        assert r.status_code == 415


def test_embeddings_body_limit_413():
    from fastapi.testclient import TestClient

    settings = Settings(
        token=TOKEN,
        models=("clap",),
        warmup=False,
        max_body_bytes=8,
        decode_timeout=1.0,
    )
    app = create_app(
        encoders={"clap": FakeEncoder("clap", CLAP_MODEL_KEY, 512)},
        settings=settings,
    )
    with TestClient(app) as client:
        # Content-Length заранее больше лимита
        assert (
            client.post(
                "/v1/embeddings?model=clap", content=b"F" * 16, headers=AUTH
            ).status_code
            == 413
        )
        # тело без Content-Length сверх лимита — тот же 413
        req = client.build_request(
            "POST", "/v1/embeddings?model=clap", data=iter([b"F" * 16]), headers=AUTH
        )
        req.headers.pop("content-length", None)
        resp = client.send(req, stream=True)
        try:
            assert resp.status_code == 413
        finally:
            resp.close()


def test_embeddings_encoder_error_500():
    class Exploder(FakeEncoder):
        def embed_file(self, data: bytes) -> np.ndarray:
            raise RuntimeError("boom")

    from fastapi.testclient import TestClient

    app = create_app(
        encoders={"clap": Exploder("clap", CLAP_MODEL_KEY, 512)},
        settings=Settings(TOKEN, ("clap",), False, 1024, 30.0),
    )
    with TestClient(app) as client:
        r = client.post("/v1/embeddings?model=clap", content=b"FAKE", headers=AUTH)
        assert r.status_code == 500


def test_embeddings_nan_500():
    class Nanner(FakeEncoder):
        def embed_file(self, data: bytes) -> np.ndarray:
            vec = super().embed_file(data)
            vec[2] = np.nan
            return vec

    from fastapi.testclient import TestClient

    app = create_app(
        encoders={"clap": Nanner("clap", CLAP_MODEL_KEY, 512)},
        settings=Settings(TOKEN, ("clap",), False, 1024, 30.0),
    )
    with TestClient(app) as client:
        r = client.post("/v1/embeddings?model=clap", content=b"FAKE", headers=AUTH)
        assert r.status_code == 500
        assert "NaN" in r.json()["detail"]


# -- окна CLAP (копия clap_3x30_v1) -----------------------------------------


def test_plan_windows_short_is_full():
    assert plan_windows(20.0) == [("full", 0.0, 20.0)]


def test_plan_windows_long_three_windows():
    wins = plan_windows(240.0)
    assert [w[0] for w in wins] == ["start", "middle", "end"]
    assert wins[0][1] == 0.0
    assert abs(wins[1][1] - (120.0 - 15.0)) < 1e-6
    assert abs(wins[2][1] - 210.0) < 1e-6


def test_plan_windows_dedupe_medium():
    assert 2 <= len(plan_windows(31.0)) <= 3


# -- ffmpeg-декод (sine-mp3, как tests/test_segments.py) ---------------------


def _sine_mp3(path, duration: float = 2.0) -> None:
    subprocess.run(
        [
            shutil.which("ffmpeg"),
            "-nostdin",
            "-v",
            "error",
            "-f",
            "lavfi",
            "-i",
            f"sine=frequency=440:duration={duration}",
            "-q:a",
            "4",
            str(path),
        ],
        check=True,
        capture_output=True,
    )


@pytest.mark.skipif(not HAS_FFMPEG, reason="ffmpeg/ffprobe недоступны")
def test_decode_audio_sine_mp3_24k_48k(tmp_path):
    mp3 = tmp_path / "sine.mp3"
    _sine_mp3(mp3, duration=2.0)
    data = mp3.read_bytes()

    for sr, expected in ((24_000, 48_000), (48_000, 96_000)):
        with temp_audio(data) as path:
            y = decode_audio(path, sample_rate=sr)
        assert y.dtype == np.float32
        assert abs(y.size - expected) < 0.2 * sr  # ±0.2с на mp3-паддинг
        assert np.all(np.isfinite(y))
        assert float(np.max(np.abs(y))) > 0.1  # не тишина

    with temp_audio(data) as path:
        assert abs(ffprobe_duration(path) - 2.0) < 0.2


@pytest.mark.skipif(not HAS_FFMPEG, reason="ffmpeg/ffprobe недоступны")
def test_decode_audio_window(tmp_path):
    mp3 = tmp_path / "sine.mp3"
    _sine_mp3(mp3, duration=5.0)
    with temp_audio(mp3.read_bytes()) as path:
        y = decode_audio(path, sample_rate=48_000, offset_sec=1.0, duration_sec=2.0)
    assert y.dtype == np.float32
    assert abs(y.size - 96_000) < 0.2 * 48_000


@pytest.mark.skipif(not HAS_FFMPEG, reason="ffmpeg/ffprobe недоступны")
def test_decode_audio_garbage_raises_decode_error():
    with (
        temp_audio(b"definitely not audio \x00\x01\x02") as path,
        pytest.raises(DecodeError),
    ):
        decode_audio(path, sample_rate=24_000)


@pytest.mark.skipif(not HAS_FFMPEG, reason="ffmpeg/ffprobe недоступны")
def test_decode_audio_empty_raises_decode_error(tmp_path):
    empty = tmp_path / "empty.mp3"
    empty.write_bytes(b"")
    with temp_audio(b"") as path, pytest.raises(DecodeError):
        decode_audio(path, sample_rate=24_000)


# -- фабрика ----------------------------------------------------------------


def test_build_encoders_unknown_name():
    from encoders import build_encoders

    with pytest.raises(ValueError, match="неизвестная модель"):
        build_encoders(("clap", "whisper"))
    with pytest.raises(ValueError, match="MODELS пуст"):
        build_encoders(())


def test_build_encoders_instances():
    from encoders import MuqEncoder, build_encoders

    enc = build_encoders(("muq",))
    assert isinstance(enc["muq"], MuqEncoder)
    assert enc["muq"].model_key() == MUQ_MODEL_KEY
    assert enc["muq"].sample_rate == 24_000
    assert enc["muq"].dim == 512
    assert enc["muq"].loaded is False


# -- muq EasyDict-патч (transformers 5.x drift) -----------------------------


class _FakeModule:
    def __init__(self, config: object | None = None) -> None:
        if config is not None:
            self.config = config  # noqa: B010


class _FakeModel:
    """Дерево модулей без torch: named_modules() как у nn.Module."""

    def __init__(self, modules: list[_FakeModule]) -> None:
        self.modules_ = modules

    def named_modules(self):
        return iter(("", m) for m in self.modules_)


def test_patch_easydict_attn():
    """EasyDict-конфиг без _attn_implementation → патчится на "eager";
    прочие конфиги/модули не трогаются. Реальные модели не грузятся."""
    from easydict import EasyDict

    from encoders import _patch_easydict_attn

    target = EasyDict(hidden_size=1024)  # без _attn_implementation
    class_cfg = type("Cfg", (), {"_attn_implementation": "sdpa"})()  # не EasyDict
    done = EasyDict(_attn_implementation="sdpa")  # уже проставлен
    model = _FakeModel([_FakeModule(target), _FakeModule(class_cfg), _FakeModule(done), _FakeModule()])

    assert _patch_easydict_attn(model) == 1
    assert target._attn_implementation == "eager"
    assert class_cfg._attn_implementation == "sdpa"  # не перезаписан
    assert done._attn_implementation == "sdpa"  # не перезаписан
