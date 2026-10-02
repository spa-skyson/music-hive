"""F4.2 (#28): ONNX-бэкенд — конфиг-парсер, фабрика, toy-модель, полный PG-цикл.

Toy-модель собирается в фикстуре onnx.helper: waveform [B, 4000] →
chunk-means → [B, 8]. Реальный onnxruntime; PG-цикл — по паттерну
test_pg_model_switch (pg_env).

Запуск:

    uv run --extra onnx --extra dev pytest tests/test_embed_onnx.py -q

без extra onnx инференс-тесты пропускаются (конфиг/фабрика — работают).
"""

from __future__ import annotations

import json
import os
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

PG_URL = os.environ.get("MUSIC_HIVE_TEST_DATABASE_URL", "").strip()

try:  # extra onnx: onnx — сборка графов, onnxruntime — инференс
    import onnx
    import onnxruntime  # noqa: F401

    HAS_ONNX = True
except ImportError:
    HAS_ONNX = False

needs_onnx = pytest.mark.skipif(
    not HAS_ONNX, reason="не установлен extra onnx (uv run --extra onnx)"
)

TOY_SR = 8_000
TOY_WINDOW_SEC = 0.5
TOY_W = TOY_SR * TOY_WINDOW_SEC  # 4000 сэмплов
TOY_DIM = 8

BASE_CFG = {
    "path": "toy.onnx",
    "sample_rate": TOY_SR,
    "window_sec": TOY_WINDOW_SEC,
    "input_name": "audio",
    "output_name": "embedding",
}


# --------------------------------------------------------------- toy-графы


def _build_toy_waveform_onnx(path: Path) -> None:
    """audio [B, 4000] → Reshape [B, 8, 500] → ReduceMean по времени → [B, 8]."""
    from onnx import TensorProto, helper, numpy_helper

    audio = helper.make_tensor_value_info("audio", TensorProto.FLOAT, ["B", int(TOY_W)])
    emb = helper.make_tensor_value_info("embedding", TensorProto.FLOAT, ["B", TOY_DIM])
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


def _build_toy_logmel_onnx(path: Path) -> None:
    """input_features [B, 4, 1001, 64] (+is_longer [B,1], не используется) → [B, 8]."""
    from onnx import TensorProto, helper, numpy_helper

    feats = helper.make_tensor_value_info(
        "input_features", TensorProto.FLOAT, ["B", 4, 1001, 64]
    )
    is_longer = helper.make_tensor_value_info("is_longer", TensorProto.FLOAT, ["B", 1])
    emb = helper.make_tensor_value_info("embedding", TensorProto.FLOAT, ["B", TOY_DIM])
    shape = numpy_helper.from_array(
        np.asarray([0, TOY_DIM, -1], dtype=np.int64), "shape"
    )
    nodes = [
        helper.make_node(
            "ReduceMean", ["input_features"], ["frames"], axes=[1, 2], keepdims=0
        ),
        helper.make_node("Reshape", ["frames", "shape"], ["reshaped"]),
        helper.make_node(
            "ReduceMean", ["reshaped"], ["embedding"], axes=[2], keepdims=0
        ),
    ]
    graph = helper.make_graph(nodes, "toy_logmel", [feats, is_longer], [emb], [shape])
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
def models_dir(tmp_path: Path) -> Path:
    """Каталог конфигов через MUSIC_HIVE_ONNX_DIR + toy-модель waveform."""
    from music_hive.config import get_settings

    models = tmp_path / "models.d"
    models.mkdir()
    if HAS_ONNX:  # без extra onnx граф не собрать; тесты инференса всё равно skip
        _build_toy_waveform_onnx(models / "toy.onnx")
    _write_cfg(models)
    os.environ["MUSIC_HIVE_ONNX_DIR"] = str(models)
    get_settings.cache_clear()
    yield models
    os.environ.pop("MUSIC_HIVE_ONNX_DIR", None)
    get_settings.cache_clear()


# ------------------------------------------------------------ конфиг-парсер


@pytest.fixture
def clean_settings(monkeypatch: pytest.MonkeyPatch):
    """Чистый settings-кеш без MUSIC_HIVE_ONNX_DIR (env тест выставит сам)."""
    from music_hive.config import get_settings

    monkeypatch.delenv("MUSIC_HIVE_ONNX_DIR", raising=False)
    get_settings.cache_clear()
    yield
    get_settings.cache_clear()


def test_config_defaults_and_path_resolution(
    clean_settings, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    from music_hive.embed.onnx import load_config

    models = tmp_path / "m"
    models.mkdir()
    _write_cfg(models)
    monkeypatch.setenv("MUSIC_HIVE_ONNX_DIR", str(models))
    cfg = load_config("toy")
    assert cfg.name == "toy"
    assert cfg.path == models / "toy.onnx"  # относительный путь — от конфига
    assert cfg.hop_sec == cfg.window_sec  # hop по умолчанию = window
    assert cfg.input_kind == "waveform"
    assert cfg.aggregate == "mean"
    assert cfg.batch_size == 1
    assert cfg.dim is None


def test_config_not_found_hints_path(
    clean_settings, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    from music_hive.embed.onnx import load_config

    monkeypatch.setenv("MUSIC_HIVE_ONNX_DIR", str(tmp_path / "empty"))
    with pytest.raises(ValueError, match=r"не найден.*MUSIC_HIVE_ONNX_DIR"):
        load_config("nope")


@pytest.mark.parametrize(
    ("overrides", "fragment"),
    [
        ({"sample_rate": 0}, "sample_rate"),
        ({"window_sec": "30"}, "window_sec"),
        ({"hop_sec": -1}, "hop_sec"),
        ({"input_kind": "spectro"}, "input_kind"),
        ({"aggregate": "max"}, "aggregate"),
        ({"batch_size": 0}, "batch_size"),
        ({"dim": 0}, "dim"),
        ({"input_kind": "log_mel"}, "feature_extractor"),  # log_mel без экстрактора
        ({"unknown_key": 1}, "неизвестные ключи"),
        ({}, "не хватает ключей"),  # затираем required ниже
    ],
)
def test_config_rejects_bad_values(
    clean_settings,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    overrides: dict,
    fragment: str,
):
    from music_hive.embed.onnx import _REQUIRED_KEYS, load_config

    models = tmp_path / "m"
    models.mkdir()
    cfg = {**BASE_CFG, **overrides}
    if fragment == "не хватает ключей":
        cfg.pop("input_name")
    (models / "toy.json").write_text(json.dumps(cfg), encoding="utf-8")
    monkeypatch.setenv("MUSIC_HIVE_ONNX_DIR", str(models))
    with pytest.raises(ValueError, match=fragment):
        load_config("toy")
    assert "input_name" in _REQUIRED_KEYS


def test_config_rejects_bad_model_name(
    clean_settings, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    from music_hive.embed.onnx import load_config

    monkeypatch.setenv("MUSIC_HIVE_ONNX_DIR", str(tmp_path))
    for bad in ("../evil", "a/b", "", "."):
        with pytest.raises(ValueError, match="некорректное имя"):
            load_config(bad)


def test_config_bad_json(
    clean_settings, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    from music_hive.embed.onnx import load_config

    models = tmp_path / "m"
    models.mkdir()
    (models / "toy.json").write_text("{oops", encoding="utf-8")
    monkeypatch.setenv("MUSIC_HIVE_ONNX_DIR", str(models))
    with pytest.raises(ValueError, match="битый JSON"):
        load_config("toy")


# ----------------------------------------------------------------- фабрика


def test_factory_missing_config_value_error(
    clean_settings, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    """Нет конфига → ValueError с подсказкой пути (не ImportError/KeyError)."""
    monkeypatch.setenv("MUSIC_HIVE_ONNX_DIR", str(tmp_path / "empty"))
    with pytest.raises(ValueError, match=r"не найден.*models.d"):
        from music_hive.embed.base import get_encoder

        get_encoder("onnx:ghost")


def test_factory_missing_package_hint(
    models_dir: Path, monkeypatch: pytest.MonkeyPatch
):
    """Нет onnxruntime → внятная «установите music-hive[onnx]», не ImportError."""
    import music_hive.embed.onnx as onnx_mod
    from music_hive.embed.base import get_encoder

    monkeypatch.setattr(onnx_mod, "find_spec", lambda name: None)
    with pytest.raises(ValueError, match=r"music-hive\[onnx\]"):
        get_encoder("onnx:toy")


def test_factory_default_is_clap_still(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    """Фича не сломала дефолт: без env — clap-бэкенд."""
    from music_hive.config import get_settings
    from music_hive.embed.base import get_encoder

    monkeypatch.delenv("MUSIC_HIVE_EMBEDDING_MODEL", raising=False)
    monkeypatch.delenv("MUSIC_HIVE_ONNX_DIR", raising=False)
    get_settings.cache_clear()
    try:
        assert get_encoder().model_key() == "clap:laion/larger_clap_music_and_speech"
    finally:
        get_settings.cache_clear()


@needs_onnx
def test_factory_onnx_key_and_protocol(models_dir: Path):
    from music_hive.embed.base import Encoder, ValidatedEncoder, get_encoder

    enc = get_encoder("onnx:toy")
    assert isinstance(enc, Encoder)
    assert isinstance(enc, ValidatedEncoder)
    assert enc.model_key() == "onnx:toy"
    assert enc.sample_rate == TOY_SR


# ------------------------------------------------------------- энкодер (toy)


def _expected_toy(windows: list[np.ndarray]) -> np.ndarray:
    """Зеркало математики энкодера для toy-графа: chunk-means → L2 окна → mean → L2."""
    outs = np.stack([w.reshape(TOY_DIM, -1).mean(axis=1) for w in windows])
    outs = outs / np.maximum(np.linalg.norm(outs, axis=1, keepdims=True), 1e-12)
    mean = outs.mean(axis=0)
    return (mean / np.linalg.norm(mean)).astype(np.float32)


@needs_onnx
def test_encode_unit_vector_and_dtype(models_dir: Path):
    from music_hive.embed.onnx import OnnxEncoder

    enc = OnnxEncoder("toy")
    vec = enc.encode([np.ones(int(TOY_W), dtype=np.float32)])
    assert vec.shape == (TOY_DIM,) and vec.dtype == np.float32
    assert np.allclose(
        vec, np.full(TOY_DIM, TOY_DIM**-0.5, dtype=np.float32), atol=1e-6
    )
    assert abs(float(np.linalg.norm(vec)) - 1.0) < 1e-6


@needs_onnx
def test_encode_windowing_and_aggregation(models_dir: Path):
    """Сегмент 2×W с разным контентом окон → mean нормированных окон (зеркало математики)."""
    from music_hive.embed.onnx import OnnxEncoder

    enc = OnnxEncoder("toy")
    rng = np.random.default_rng(7)
    seg = rng.standard_normal(int(2 * TOY_W)).astype(np.float32)
    vec = enc.encode([seg])
    w1, w2 = seg[: int(TOY_W)], seg[int(TOY_W) :]
    assert np.allclose(vec, _expected_toy([w1, w2]), atol=1e-6)


@needs_onnx
def test_encode_hop_windows(models_dir: Path):
    """hop_sec < window_sec → перекрывающиеся окна с шагом hop."""
    from music_hive.embed.onnx import OnnxEncoder, load_config

    assert load_config("toy").hop_sec == TOY_WINDOW_SEC  # база — без перекрытия
    cfg_path = models_dir / "toy.json"
    cfg = json.loads(cfg_path.read_text(encoding="utf-8"))
    cfg["hop_sec"] = TOY_WINDOW_SEC / 2
    cfg_path.write_text(json.dumps(cfg), encoding="utf-8")

    enc = OnnxEncoder("toy")
    rng = np.random.default_rng(11)
    seg = rng.standard_normal(int(2 * TOY_W)).astype(np.float32)
    vec = enc.encode([seg])
    w = int(TOY_W)
    hop = w // 2
    windows = [seg[s : s + w] for s in range(0, seg.size - w + 1, hop)]
    assert len(windows) == 3
    assert np.allclose(vec, _expected_toy(windows), atol=1e-6)


@needs_onnx
def test_encode_short_segment_padded(models_dir: Path):
    """Сегмент короче окна → одно нулём-дополненное окно, не пустота."""
    from music_hive.embed.onnx import OnnxEncoder

    enc = OnnxEncoder("toy")
    seg = np.linspace(1, 2, 100, dtype=np.float32)
    vec = enc.encode([seg])
    padded = np.zeros(int(TOY_W), dtype=np.float32)
    padded[:100] = seg
    assert np.allclose(vec, _expected_toy([padded]), atol=1e-6)


@needs_onnx
def test_encode_batches_match_single(models_dir: Path):
    from music_hive.embed.onnx import OnnxEncoder

    _write_cfg(models_dir, "toy_b4", batch_size=4)
    rng = np.random.default_rng(3)
    segs = [
        rng.standard_normal(int(k * TOY_W)).astype(np.float32) for k in (1.0, 2.0, 1.5)
    ]
    single = OnnxEncoder("toy").encode(segs)
    batched = OnnxEncoder("toy_b4").encode(segs)
    assert np.allclose(single, batched, atol=1e-6)


@needs_onnx
def test_encode_dim_fact_check(models_dir: Path):
    from music_hive.embed.onnx import OnnxEncoder

    _write_cfg(models_dir, "toy_bad_dim", dim=9)
    enc = OnnxEncoder("toy_bad_dim")
    with pytest.raises(ValueError, match=r"фактическая dim 8 != dim конфига 9"):
        enc.encode([np.ones(int(TOY_W), dtype=np.float32)])


@needs_onnx
def test_encode_rejects_empty(models_dir: Path):
    from music_hive.embed.onnx import OnnxEncoder

    enc = OnnxEncoder("toy")
    with pytest.raises(ValueError, match="пустой список сегментов"):
        enc.encode([])
    with pytest.raises(ValueError, match="все сегменты пусты"):
        enc.encode([np.zeros(0, dtype=np.float32)])


@needs_onnx
def test_warm_up_and_lazy_session(models_dir: Path):
    """warm_up — прогон нулевого сегмента; сессия создаётся при нём же."""
    from music_hive.embed.onnx import OnnxEncoder

    enc = OnnxEncoder("toy")
    enc.warm_up()
    vec = enc.encode([np.ones(int(TOY_W), dtype=np.float32)])
    assert abs(float(np.linalg.norm(vec)) - 1.0) < 1e-6


@needs_onnx
def test_missing_weights_clear_error(models_dir: Path):
    from music_hive.embed.onnx import OnnxEncoder

    _write_cfg(models_dir, "toy_noweights", path="absent.onnx")
    enc = OnnxEncoder("toy_noweights")
    with pytest.raises(ValueError, match="файл весов ONNX не найден.*absent.onnx"):
        enc.encode([np.ones(int(TOY_W), dtype=np.float32)])


@needs_onnx
def test_log_mel_via_feature_extractor(models_dir: Path):
    """input_kind=log_mel: мел считает ClapFeatureExtractor (offline-каталог),
    выходы экстрактора уходят в одноимённые входы графа (input_features, is_longer)."""
    from transformers import ClapFeatureExtractor

    fe_dir = models_dir / "fe"
    ClapFeatureExtractor().save_pretrained(fe_dir)  # дефолты: 48k, repeatpad
    _build_toy_logmel_onnx(models_dir / "logmel.onnx")
    _write_cfg(
        models_dir,
        "logmel",
        path="logmel.onnx",
        sample_rate=48_000,
        window_sec=9.0,
        input_name="input_features",
        input_kind="log_mel",
        feature_extractor=str(fe_dir),
    )
    from music_hive.embed.onnx import OnnxEncoder

    enc = OnnxEncoder("logmel")
    assert enc.model_key() == "onnx:logmel"
    vec = enc.encode([np.full(48_000 * 9, 0.1, dtype=np.float32)])
    assert vec.shape == (TOY_DIM,) and vec.dtype == np.float32
    assert abs(float(np.linalg.norm(vec)) - 1.0) < 1e-6


# ------------------------------------------------------------- полный цикл PG


@pytest.mark.skipif(
    not PG_URL, reason="MUSIC_HIVE_TEST_DATABASE_URL не задан — PG-тесты пропущены"
)
@needs_onnx
def test_pg_full_cycle_onnx_toy(
    pg_env, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    """scan → embed (MUSIC_HIVE_EMBEDDING_MODEL=onnx:toy) → emb_onnx_toy с L2=1 векторами."""
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

    models = tmp_path / "models.d"
    models.mkdir()
    _build_toy_waveform_onnx(models / "toy.onnx")
    _write_cfg(models, batch_size=4, dim=TOY_DIM)
    monkeypatch.setenv("MUSIC_HIVE_ONNX_DIR", str(models))
    monkeypatch.setenv("MUSIC_HIVE_EMBEDDING_MODEL", "onnx:toy")
    monkeypatch.setenv("MUSIC_HIVE_EMBED_SEGMENT_SEC", str(TOY_WINDOW_SEC))
    get_settings.cache_clear()

    lib = tmp_path / "lib"
    lib.mkdir()
    for name, freq in (("a.wav", 440.0), ("b.wav", 880.0)):
        t = np.linspace(0, TOY_WINDOW_SEC, int(TOY_SR * TOY_WINDOW_SEC), endpoint=False)
        sf.write(
            lib / name, (0.2 * np.sin(2 * np.pi * freq * t)).astype(np.float32), TOY_SR
        )

    ensure_db()
    scan_library(lib, extract_audio=False, workers=1)
    res = embed_library()
    assert res.computed == 2 and res.failed == 0 and res.total == 2

    # реестр: модель зарегистрирована и активна (первая — активируется сама)
    am = active_model()
    assert (
        am is not None and am["model_key"] == "onnx:toy" and int(am["dim"]) == TOY_DIM
    )
    assert emb_table_name("onnx:toy") == "emb_onnx_toy"

    with connect() as conn:
        n_ready = int(
            conn.execute(
                "SELECT COUNT(*) AS n FROM emb_onnx_toy WHERE status = 'ready'"
            ).fetchone()["n"]
        )
        ids = [
            int(r["id"])
            for r in conn.execute("SELECT id FROM tracks ORDER BY id").fetchall()
        ]
    assert n_ready == 2

    for tid in ids:
        vec = get_embedding(tid)  # читает из таблицы активной модели
        assert vec is not None and vec.shape == (TOY_DIM,)
        assert vec.dtype == np.float32
        assert abs(float(np.linalg.norm(vec)) - 1.0) < 1e-5

    # файлового кеша нет (F5); готовность — по рабочей модели
    assert list_tracks_needing_embedding() == []
    # векторы детерминированы контентом (разная частота → разные векторы)
    assert not np.allclose(get_embedding(ids[0]), get_embedding(ids[1]))
