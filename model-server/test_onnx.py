"""ONNX-энкодер model-server (#33): toy-модель + полный HTTP-цикл.

Toy-граф собирается onnx.helper по паттерну tests/test_embed_onnx.py проекта:
waveform [B, 4000] → chunk-means → [B, 8]. Реальный onnxruntime + ffmpeg:
TestClient POST mp3 → вектор dim конфига, L2=1; ошибки контракта.

Запуск (из корня репо):

    uv run --with fastapi --with httpx --with numpy --with onnx \
        --with onnxruntime --with pytest pytest model-server/ -q

без onnx/onnxruntime или ffmpeg инференс-тесты пропускаются.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
from pathlib import Path

import numpy as np
import pytest

os.environ.setdefault("REMOTE_TOKEN", "ci-token")  # до import app (fail-closed)

from app import Settings, create_app  # noqa: E402
from encoders import OnnxEncoder, build_encoders, load_onnx_config  # noqa: E402

TOKEN = "ci-token"
AUTH = {"Authorization": f"Bearer {TOKEN}"}
HAS_FFMPEG = bool(shutil.which("ffmpeg") and shutil.which("ffprobe"))

try:  # onnx — сборка графов, onnxruntime — инференс
    import onnx
    import onnxruntime  # noqa: F401

    HAS_ONNX = True
except ImportError:
    HAS_ONNX = False

needs_onnx = pytest.mark.skipif(
    not (HAS_ONNX and HAS_FFMPEG), reason="нужны onnx/onnxruntime и ffmpeg"
)

TOY_SR = 8_000
TOY_WINDOW_SEC = 0.5
TOY_W = int(TOY_SR * TOY_WINDOW_SEC)  # 4000 сэмплов
TOY_DIM = 8

BASE_CFG = {
    "path": "toy.onnx",
    "sample_rate": TOY_SR,
    "window_sec": TOY_WINDOW_SEC,
    "input_name": "audio",
    "output_name": "embedding",
}


# --------------------------------------------------------------- toy-граф


def _build_toy_onnx(path: Path) -> None:
    """audio [B, 4000] → Reshape [B, 8, 500] → ReduceMean по времени → [B, 8]."""
    from onnx import TensorProto, helper, numpy_helper

    audio = helper.make_tensor_value_info(
        "audio", TensorProto.FLOAT, ["B", TOY_W]
    )
    emb = helper.make_tensor_value_info(
        "embedding", TensorProto.FLOAT, ["B", TOY_DIM]
    )
    shape = numpy_helper.from_array(
        np.asarray([0, TOY_DIM, -1], dtype=np.int64), "shape"
    )
    nodes = [
        helper.make_node("Reshape", ["audio", "shape"], ["reshaped"]),
        helper.make_node(
            "ReduceMean", ["reshaped"], ["embedding"], axes=[2], keepdims=0
        ),
    ]
    graph = helper.make_graph(nodes, "toy", [audio], [emb], [shape])
    model = helper.make_model(graph, opset_imports=[helper.make_opsetid("", 13)])
    model.ir_version = 8
    onnx.checker.check_model(model)
    onnx.save(model, str(path))


def _write_cfg(models: Path, name: str = "toy", **overrides: object) -> Path:
    cfg = {**BASE_CFG, **overrides}
    path = models / f"{name}.json"
    path.write_text(json.dumps(cfg), encoding="utf-8")
    return path


@pytest.fixture
def toy_dir(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Каталог конфигов через MUSIC_HIVE_ONNX_DIR + toy-модель waveform."""
    models = tmp_path / "models.d"
    models.mkdir()
    if HAS_ONNX:  # без onnx граф не собрать; конфиг-тесты всё равно работают
        _build_toy_onnx(models / "toy.onnx")
    _write_cfg(models)
    monkeypatch.setenv("MUSIC_HIVE_ONNX_DIR", str(models))
    return models


def _make_client(encoders: dict):
    from fastapi.testclient import TestClient

    settings = Settings(
        token=TOKEN,
        models=tuple(encoders),
        warmup=False,
        max_body_bytes=1024 * 1024,
        decode_timeout=30.0,
    )
    return TestClient(create_app(encoders=encoders, settings=settings))


def _sine_mp3(path: Path, duration: float = 1.0) -> bytes:
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
            "-f",
            "mp3",
            str(path),
        ],
        check=True,
        capture_output=True,
    )
    return path.read_bytes()


# ------------------------------------------------------------ конфиг-парсер


def test_config_load_defaults(toy_dir: Path):
    cfg = load_onnx_config("toy")
    assert cfg["sample_rate"] == TOY_SR
    assert "hop_sec" not in cfg  # опционален — энкодер берёт = window_sec


def test_config_not_found_hint(toy_dir: Path):
    with pytest.raises(ValueError, match=r"не найден.*MUSIC_HIVE_ONNX_DIR"):
        load_onnx_config("ghost")


def test_config_bad_name_traversal(toy_dir: Path):
    for bad in ("../evil", "a/b", "", "."):
        with pytest.raises(ValueError, match="некорректное имя"):
            load_onnx_config(bad)


def test_config_bad_json(toy_dir: Path):
    (toy_dir / "broken.json").write_text("{oops", encoding="utf-8")
    with pytest.raises(ValueError, match="битый JSON"):
        load_onnx_config("broken")


def test_config_rejects_log_mel(toy_dir: Path):
    _write_cfg(toy_dir, "logmel", input_kind="log_mel")
    with pytest.raises(ValueError, match="только waveform"):
        load_onnx_config("logmel")


def test_config_missing_keys(toy_dir: Path):
    bad = {k: v for k, v in BASE_CFG.items() if k != "input_name"}
    (toy_dir / "nofield.json").write_text(json.dumps(bad), encoding="utf-8")
    with pytest.raises(ValueError, match="не хватает ключей"):
        load_onnx_config("nofield")


# ------------------------------------------------------------------ фабрика


def test_build_encoders_onnx_prefix(toy_dir: Path):
    enc = build_encoders(("onnx:toy",))["onnx:toy"]
    assert isinstance(enc, OnnxEncoder)
    assert enc.model_key() == "onnx:toy"  # ключ реестра воркера emb_onnx_toy
    assert enc.sample_rate == TOY_SR
    assert enc.loaded is False  # сессия ленивая


def test_build_encoders_unknown_hint_mentions_onnx():
    with pytest.raises(ValueError, match="onnx:<имя конфига>"):
        build_encoders(("whisper",))


@needs_onnx
def test_missing_weights_clear_error(toy_dir: Path):
    _write_cfg(toy_dir, "noweights", path="absent.onnx")
    enc = OnnxEncoder("noweights")
    with pytest.raises(ValueError, match="файл весов ONNX не найден"):
        enc.warm_up()


# --------------------------------------------------- инференс + HTTP-цикл


def _expected_toy(windows: list[np.ndarray]) -> np.ndarray:
    """Зеркало математики энкодера: chunk-means → L2 окна → mean → L2."""
    outs = np.stack([w.reshape(TOY_DIM, -1).mean(axis=1) for w in windows])
    outs = outs / np.maximum(np.linalg.norm(outs, axis=1, keepdims=True), 1e-12)
    mean = outs.mean(axis=0)
    return (mean / np.linalg.norm(mean)).astype(np.float32)


@needs_onnx
def test_waveform_windowing_math(toy_dir: Path):
    """2 окна разного контента → per-window L2 → mean → L2 (1-в-1 с воркером)."""
    enc = OnnxEncoder("toy")
    rng = np.random.default_rng(7)
    y = rng.standard_normal(int(2 * TOY_W)).astype(np.float32)
    vec = enc.embed_waveform(y)
    assert vec.shape == (TOY_DIM,) and vec.dtype == np.float32
    assert np.allclose(vec, _expected_toy([y[:TOY_W], y[TOY_W:]]), atol=1e-6)


@needs_onnx
def test_waveform_short_track_padded(toy_dir: Path):
    enc = OnnxEncoder("toy")
    y = np.linspace(1, 2, 100, dtype=np.float32)
    padded = np.zeros(int(TOY_W), dtype=np.float32)
    padded[:100] = y
    assert np.allclose(enc.embed_waveform(y), _expected_toy([padded]), atol=1e-6)


@needs_onnx
def test_dim_fixed_by_fact_when_absent(toy_dir: Path):
    enc = OnnxEncoder("toy")
    assert enc.dim == 0  # dim опционален
    enc.embed_waveform(np.ones(int(TOY_W), dtype=np.float32))
    assert enc.dim == TOY_DIM  # зафиксирована фактом инференса


@needs_onnx
def test_dim_config_mismatch_raises(toy_dir: Path):
    _write_cfg(toy_dir, "badim", dim=9)
    enc = OnnxEncoder("badim")
    with pytest.raises(ValueError, match="фактическая dim 8 != dim конфига 9"):
        enc.embed_waveform(np.ones(int(TOY_W), dtype=np.float32))


@needs_onnx
def test_warm_up_loads_session(toy_dir: Path):
    enc = OnnxEncoder("toy")
    enc.warm_up()
    assert enc.loaded is True


@needs_onnx
def test_http_full_cycle_mp3(toy_dir: Path):
    """POST mp3-файла → 200, model_key=onnx:toy, dim конфига, L2=1; /v1/models."""
    data = _sine_mp3(toy_dir / "sine.mp3")
    with _make_client({"onnx:toy": OnnxEncoder("toy")}) as client:
        models = client.get("/v1/models", headers=AUTH).json()["models"]
        assert [m["name"] for m in models] == ["toy"]
        assert models[0]["model_key"] == "onnx:toy"
        assert models[0]["dim"] == 0  # dim не задан в конфиге — до инференса 0

        r = client.post(
            "/v1/embeddings?model=onnx:toy", content=data, headers=AUTH
        )
        assert r.status_code == 200
        body = r.json()
        assert body["model_key"] == "onnx:toy"
        assert body["dim"] == TOY_DIM
        emb = np.asarray(body["embedding"], dtype=np.float32)
        assert emb.shape == (TOY_DIM,)
        assert abs(float(np.linalg.norm(emb)) - 1.0) < 1e-5  # L2 сервисом

        # после инференса dim известна фактом
        models = client.get("/v1/models", headers=AUTH).json()["models"]
        assert models[0]["dim"] == TOY_DIM and models[0]["loaded"] is True


@needs_onnx
def test_http_dim_from_config(toy_dir: Path):
    _write_cfg(toy_dir, "toy", dim=TOY_DIM)
    data = _sine_mp3(toy_dir / "sine.mp3")
    with _make_client({"onnx:toy": OnnxEncoder("toy")}) as client:
        models = client.get("/v1/models", headers=AUTH).json()["models"]
        assert models[0]["dim"] == TOY_DIM  # из конфига, до инференса
        r = client.post("/v1/embeddings?model=onnx:toy", content=data, headers=AUTH)
        assert r.status_code == 200 and r.json()["dim"] == TOY_DIM


@needs_onnx
def test_http_contract_errors(toy_dir: Path):
    with _make_client({"onnx:toy": OnnxEncoder("toy")}) as client:
        # 401 без токена
        assert (
            client.post(
                "/v1/embeddings?model=onnx:toy", content=b"FAKE"
            ).status_code
            == 401
        )
        # 404 неизвестная модель
        r = client.post(
            "/v1/embeddings?model=onnx:ghost", content=b"FAKE", headers=AUTH
        )
        assert r.status_code == 404
        # 415 не декодируется ffmpeg
        r = client.post(
            "/v1/embeddings?model=onnx:toy", content=b"not audio", headers=AUTH
        )
        assert r.status_code == 415


def test_settings_models_env_with_onnx():
    from app import settings_from_env

    st = settings_from_env({"REMOTE_TOKEN": "t", "MODELS": "onnx:toy, clap"})
    assert st.models == ("onnx:toy", "clap")
