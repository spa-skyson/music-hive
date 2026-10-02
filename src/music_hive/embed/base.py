"""Протокол энкодера (§6.1 контракта): бэкенды (clap, onnx — #28, remote — #31) за одним интерфейсом.

Фабрика `get_encoder` оживляет MUSIC_HIVE_EMBEDDING_MODEL; валидация §6.1
живёт в обёртке протокола, бэкенды сами себя не валидируют.
"""

from __future__ import annotations

import logging
from pathlib import Path
from typing import Protocol, runtime_checkable

import numpy as np

logger = logging.getLogger(__name__)

# Бэкенды фабрики: 'clap', 'onnx:<имя>' (конфиг models.d/onnx/<имя>.json, #28)
# и 'remote:<base_url>' (HTTP model-сервер #32, Bearer-токен).
AVAILABLE_BACKENDS = ("clap", "onnx", "remote")


@runtime_checkable
class Encoder(Protocol):
    """Аудио-энкодер: список моно-сегментов трека → один вектор.

    Сегменты: mono float32 ``np.ndarray`` на ``sample_rate`` энкодера
    (CLAP: 48 000 Гц), длина окна задаётся настройкой embed_segment_sec
    (стратегия start/middle/end описана в segments.SEGMENT_STRATEGY).
    Возврат: L2-нормированный float32 1-D фиксированной размерности
    (CLAP: 512). Агрегация окон (mean + нормировка) — ответственность
    энкодера, валидация результата — обёртки ValidatedEncoder.
    """

    sample_rate: int

    def model_key(self) -> str:
        """Ключ реестра embedding_models, напр. 'clap:laion/larger_clap_music_and_speech'."""
        ...

    def encode(self, segments: list[np.ndarray]) -> np.ndarray:
        """Список моно-сегментов → один агрегированный вектор трека."""
        ...

    # Опциональный файловый путь (#31, duck-typing как warm_up): бэкенд
    # сам декодирует аудио (remote — на сервере), pipeline тогда не режет
    # сегменты локально. В сам Protocol его не включаем: runtime_checkable
    # isinstance проверяет наличие методов — сегментные бэкенды (clap,
    # onnx) его не объявляют и остаются валидными Encoder.


def validate_vector(vec: np.ndarray, *, key: str = "") -> np.ndarray:
    """§6.1: NaN/Inf и нулевая норма → ValueError; L2 ∉ [0.98, 1.02] → ренормализация + warning."""
    vec = np.asarray(vec, dtype=np.float32).reshape(-1)
    if not np.all(np.isfinite(vec)):
        raise ValueError(f"embedding содержит NaN/Inf ({key})")
    n = float(np.linalg.norm(vec))
    if n < 1e-12:
        raise ValueError(f"нулевая норма эмбеддинга ({key})")
    if not 0.98 <= n <= 1.02:
        logger.warning(
            "embedding L2-норма %.4f вне [0.98, 1.02] (%s) — ренормализуем", n, key
        )
        vec = vec / n
    return vec.astype(np.float32)


class ValidatedEncoder:
    """Обёртка протокола: единая точка валидации §6.1 поверх любого бэкенда.

    Опциональные ``warm_up()`` и ``encode_file(path)`` бэкенда (remote #31:
    файл кодируется целиком на сервере) прокидываются, если бэкенд их
    предоставляет.
    """

    def __init__(self, inner: Encoder) -> None:
        self.inner = inner
        # encode_file объявлен только у файловых бэкендов (remote #31) —
        # иначе pipeline duck-typing'ом уведёт clap/onnx по файловому пути.
        if callable(getattr(inner, "encode_file", None)):
            self.encode_file = self._encode_file_validated

    def _encode_file_validated(self, path: Path) -> np.ndarray:
        return validate_vector(self.inner.encode_file(path), key=self.model_key())

    @property
    def sample_rate(self) -> int:
        return self.inner.sample_rate

    def model_key(self) -> str:
        return self.inner.model_key()

    def encode(self, segments: list[np.ndarray]) -> np.ndarray:
        return validate_vector(self.inner.encode(segments), key=self.model_key())

    def warm_up(self) -> None:
        warm = getattr(self.inner, "warm_up", None)
        if callable(warm):
            warm()


def get_encoder(name: str | None = None) -> Encoder:
    """Фабрика энкодеров. None → settings.embedding_model (MUSIC_HIVE_EMBEDDING_MODEL).

    'clap' → CLAP-бэкенд (repo модели — MUSIC_HIVE_CLAP_MODEL; transformers
    грузится лениво при первом encode); 'clap:<repo>' — тот же бэкенд с
    явным repo (полный ключ реестра, F4.3 model_activate); 'onnx:<имя>' →
    ONNX-бэкенд (конфиг models.d/onnx/<имя>.json, onnxruntime — extra
    music-hive[onnx]); 'remote:<base_url>' → HTTP-энкодер #31: декод и
    окна на model-сервере #32 (модель — MUSIC_HIVE_REMOTE_MODEL, токен —
    MUSIC_HIVE_REMOTE_TOKEN; без токена — ValueError). Неизвестное имя →
    ValueError.
    """
    from music_hive.config import get_settings

    if name is None:
        name = get_settings().embedding_model
    if name == "clap" or name.startswith("clap:"):
        from music_hive.embed.clap import ClapEncoder

        repo = name.removeprefix("clap:") if name.startswith("clap:") else ""
        return ValidatedEncoder(ClapEncoder(repo or get_settings().clap_model))
    if name.startswith("onnx:"):
        from music_hive.embed.onnx import OnnxEncoder

        return ValidatedEncoder(OnnxEncoder(name.removeprefix("onnx:")))
    if name.startswith("remote:"):
        from music_hive.embed.remote import RemoteEncoder

        settings = get_settings()
        if not settings.remote_token:
            raise ValueError(
                "remote-энкодер требует MUSIC_HIVE_REMOTE_TOKEN "
                "(Bearer-токен model-сервера)"
            )
        return ValidatedEncoder(
            RemoteEncoder(
                name.removeprefix("remote:"),
                settings.remote_model,
                settings.remote_token,
            )
        )
    raise ValueError(
        f"неизвестный энкодер {name!r}; доступные: clap | onnx:<имя> "
        "(конфиг models.d/onnx/<имя>.json) | remote:<base_url> (model-сервер #32)"
    )
