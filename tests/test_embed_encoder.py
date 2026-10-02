"""F4.1: протокол энкодера — фабрика, валидации §6.1, CLAP-агрегация и ленивость."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

import numpy as np
import pytest

from music_hive.embed.base import (
    AVAILABLE_BACKENDS,
    Encoder,
    ValidatedEncoder,
    get_encoder,
    validate_vector,
)


def _unit(dim: int, seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    v = rng.standard_normal(dim).astype(np.float32)
    return v / np.linalg.norm(v)


class FakeInner:
    """Минимальный бэкенд протокола без тяжёлых зависимостей."""

    sample_rate = 48_000

    def __init__(self, vec: np.ndarray) -> None:
        self._vec = vec
        self.encode_calls = 0

    def model_key(self) -> str:
        return "fake:v1"

    def encode(self, segments: list[np.ndarray]) -> np.ndarray:
        assert segments, "бэкенд получил пустые сегменты"
        self.encode_calls += 1
        return self._vec


# ------------------------------------------------------------- фабрика


def test_factory_default_clap_key_unchanged(monkeypatch: pytest.MonkeyPatch):
    """Default ('clap') → ключ реестра 1-в-1 с прежним: clap: + clap_model из конфига."""
    from music_hive.config import get_settings
    from music_hive.db.store import model_key

    monkeypatch.delenv("MUSIC_HIVE_EMBEDDING_MODEL", raising=False)
    monkeypatch.delenv("MUSIC_HIVE_CLAP_MODEL", raising=False)
    get_settings.cache_clear()
    try:
        enc = get_encoder()
        assert enc.model_key() == "clap:laion/larger_clap_music_and_speech"
        assert enc.model_key() == model_key()
        assert enc.sample_rate == 48_000
        assert isinstance(enc, Encoder)  # структурно удовлетворяет протоколу
    finally:
        get_settings.cache_clear()


def test_factory_reads_embedding_model_env(monkeypatch: pytest.MonkeyPatch):
    """MUSIC_HIVE_EMBEDDING_MODEL реально читается: неизвестное значение → ValueError."""
    from music_hive.config import get_settings

    monkeypatch.setenv("MUSIC_HIVE_EMBEDDING_MODEL", "bogus")
    get_settings.cache_clear()
    try:
        with pytest.raises(ValueError, match="bogus"):
            get_encoder()
        with pytest.raises(ValueError, match="bogus"):
            from music_hive.db.store import model_key

            model_key()
    finally:
        get_settings.cache_clear()


def test_factory_reads_clap_model_env(monkeypatch: pytest.MonkeyPatch):
    """MUSIC_HIVE_CLAP_MODEL попадает в ключ энкодера (смена модели без правки кода)."""
    from music_hive.config import get_settings
    from music_hive.db.store import model_key

    monkeypatch.delenv("MUSIC_HIVE_EMBEDDING_MODEL", raising=False)
    monkeypatch.setenv("MUSIC_HIVE_CLAP_MODEL", "custom/clap_b")
    get_settings.cache_clear()
    try:
        assert get_encoder().model_key() == "clap:custom/clap_b" == model_key()
        assert get_encoder("clap").model_key() == "clap:custom/clap_b"
    finally:
        get_settings.cache_clear()


def test_factory_unknown_and_reserved_names():
    """Неизвестное имя → ValueError со списком доступных; 'onnx:<имя>' — ветка #28
    (свой конфиг models.d/onnx; поведение — в test_embed_onnx.py)."""
    for name in ("nope", "onnx", "custom", "remote"):
        with pytest.raises(ValueError, match="доступные"):
            get_encoder(name)
    assert AVAILABLE_BACKENDS == ("clap", "onnx", "remote")


# ---------------------------------------------------------- валидации §6.1


def test_validate_rejects_nan_and_inf():
    v = _unit(8, seed=1)
    for bad in (np.nan, np.inf, -np.inf):
        v[0] = bad
        with pytest.raises(ValueError, match="NaN/Inf"):
            validate_vector(v)


def test_validate_rejects_zero_norm():
    with pytest.raises(ValueError, match="нулевая норма"):
        validate_vector(np.zeros(8, dtype=np.float32))


def test_validate_renormalizes_out_of_tolerance(caplog: pytest.LogCaptureFixture):
    v = _unit(8, seed=2) * 3.0
    with caplog.at_level("WARNING"):
        out = validate_vector(v, key="fake:v1")
    assert abs(float(np.linalg.norm(out)) - 1.0) < 1e-6
    assert any("ренормализуем" in r.message for r in caplog.records)


def test_validate_passes_unit_vector_through():
    v = _unit(8, seed=3)
    out = validate_vector(v)
    assert out.dtype == np.float32 and out.shape == (8,)
    assert np.allclose(out, v, atol=1e-6)


def test_validated_encoder_wraps_backend(caplog: pytest.LogCaptureFixture):
    """Валидация — в обёртке, не в бэкенде: бэкенд вернул мусорную норму → ренорм."""
    inner = FakeInner(_unit(8, seed=5) * 2.0)
    enc = ValidatedEncoder(inner)
    with caplog.at_level("WARNING"):
        out = enc.encode([np.zeros(48_000, dtype=np.float32)])
    assert inner.encode_calls == 1
    assert abs(float(np.linalg.norm(out)) - 1.0) < 1e-6
    assert enc.model_key() == "fake:v1"
    assert enc.sample_rate == 48_000

    # NaN из бэкенда → ValueError, а не запись мусора
    enc_nan = ValidatedEncoder(FakeInner(np.array([np.nan] * 8, dtype=np.float32)))
    with pytest.raises(ValueError, match="NaN/Inf"):
        enc_nan.encode([np.ones(8, dtype=np.float32)])


# --------------------------------------------------------------- CLAP


def test_clap_encoder_aggregates_segments(monkeypatch: pytest.MonkeyPatch):
    """ClapEncoder: mean по окнам + L2; model_key = clap:<repo>; пустые сегменты — отказ."""
    import music_hive.embed.clap as clap

    seen: list[str] = []

    def fake_embed_waveform(y: np.ndarray, *, model_id: str) -> np.ndarray:
        seen.append(model_id)
        v = np.zeros(4, dtype=np.float32)
        v[: min(4, y.size)] = y[:4]
        return v / np.linalg.norm(v)

    monkeypatch.setattr(clap, "embed_waveform", fake_embed_waveform)
    enc = get_encoder("clap")
    seg1 = np.array([1.0, 0.0, 0.0, 0.0], dtype=np.float32)
    seg2 = np.array([0.0, 1.0, 0.0, 0.0], dtype=np.float32)
    vec = enc.encode([seg1, seg2])
    assert vec.shape == (4,) and vec.dtype == np.float32
    assert abs(float(np.linalg.norm(vec)) - 1.0) < 1e-5
    # mean двух орто-единичных → диагональ 1/√2, потом нормировка
    assert np.allclose(vec[:2], [2**-0.5, 2**-0.5], atol=1e-6)
    assert seen == ["laion/larger_clap_music_and_speech"] * 2

    with pytest.raises(ValueError, match="пустой список сегментов"):
        enc.encode([])


def test_clap_factory_is_lazy_no_heavy_imports():
    """Фабрика 'clap' не тянет torch/transformers — модель грузится при первом encode."""
    code = (
        "import sys\n"
        "from music_hive.embed import get_encoder\n"
        "enc = get_encoder('clap')\n"
        "assert 'transformers' not in sys.modules, 'transformers импортирован фабрикой'\n"
        "assert 'torch' not in sys.modules, 'torch импортирован фабрикой'\n"
        "print(enc.model_key())\n"
    )
    proc = subprocess.run(
        [sys.executable, "-c", code],
        capture_output=True,
        text=True,
        check=True,
        cwd=Path(__file__).resolve().parents[1],
    )
    assert "clap:laion/larger_clap_music_and_speech" in proc.stdout
