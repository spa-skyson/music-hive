"""ONNX-бэкенд энкодера (#28): модели описывают конфиги models.d/onnx/<name>.json.

Поток encode: сегменты → окна window_sec (шаг hop_sec; хвост короче окна
отбрасывается, сегмент короче окна → одно нулём-дополненное окно) →
батч-инференс onnxruntime (CPU, сессия ленивая и кешируется по пути, как
torch-модель в clap.py) → каждое окно L2 → mean → L2 (порядок как в clap.py,
референсный экспорт CLAP даёт совместимые векторы).

Сегменты обязаны приходить на sample_rate конфига (протокол §6.1);
ресемплинг до нужного sr делает load_segment_audio в pipeline — тот же
подход, что у CLAP, у сырых ndarray своей частоты нет.

input_kind: "waveform" — вход [B, W] float32; "log_mel" — мел-спектрограмму
считает ClapFeatureExtractor (transformers, уже в основных зависимостях;
репозиторий/каталог — ключ "feature_extractor" конфига), выходы экстрактора
подаются в одноимённые входы графа (input_features, is_longer).

Валидация §6.1 (NaN/норма) — в ValidatedEncoder фабрики, бэкенд сам себя
не валидирует.
"""

from __future__ import annotations

import json
import logging
import sys
from dataclasses import dataclass
from collections.abc import Iterator
from functools import lru_cache
from importlib.util import find_spec
from pathlib import Path
from typing import Any

import numpy as np

from music_hive.config import ROOT, get_settings

logger = logging.getLogger(__name__)

INPUT_KINDS = ("waveform", "log_mel")
_REQUIRED_KEYS = ("path", "sample_rate", "window_sec", "input_name", "output_name")
_KNOWN_KEYS = frozenset(
    _REQUIRED_KEYS
    + (
        "hop_sec",
        "input_kind",
        "aggregate",
        "batch_size",
        "dim",
        "feature_extractor",
    )
)


@dataclass(frozen=True)
class OnnxConfig:
    """Разобранный models.d/onnx/<name>.json (схема — docs/embed-models.md)."""

    name: str
    path: Path  # абсолютный путь к .onnx
    sample_rate: int
    window_sec: float
    input_name: str
    output_name: str
    hop_sec: float
    input_kind: str = "waveform"
    aggregate: str = "mean"
    batch_size: int = 1
    dim: int | None = None
    feature_extractor: str = ""  # input_kind="log_mel": HF repo id или каталог


def _candidate_dirs() -> list[Path]:
    """Каталоги поиска конфигов: env-override → корень репо → prefix wheel-инсталла."""
    override = get_settings().onnx_dir.strip()
    if override:
        return [Path(override)]
    return [
        ROOT / "models.d" / "onnx",
        Path(sys.prefix) / "models.d" / "onnx",
    ]


def load_config(name: str) -> OnnxConfig:
    """Читает и валидирует конфиг модели; ошибкам — ValueError с подсказками."""
    if not name or "/" in name or "\\" in name or name.startswith("."):
        raise ValueError(f"некорректное имя ONNX-модели {name!r}")

    searched = [d / f"{name}.json" for d in _candidate_dirs()]
    cfg_path = next((p for p in searched if p.is_file()), None)
    if cfg_path is None:
        raise ValueError(
            f"конфиг ONNX-модели {name!r} не найден; искал: "
            + ", ".join(str(p) for p in searched)
            + " (положите <имя>.json в models.d/onnx или задайте MUSIC_HIVE_ONNX_DIR)"
        )
    try:
        raw: Any = json.loads(cfg_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise ValueError(f"битый JSON в {cfg_path}: {exc}") from exc
    if not isinstance(raw, dict):
        raise ValueError(
            f"{cfg_path}: ожидается JSON-объект, получено {type(raw).__name__}"
        )

    unknown = sorted(set(raw) - _KNOWN_KEYS)
    if unknown:
        raise ValueError(
            f"{cfg_path}: неизвестные ключи {unknown}; схема — docs/embed-models.md"
        )
    missing = [k for k in _REQUIRED_KEYS if k not in raw]
    if missing:
        raise ValueError(f"{cfg_path}: не хватает ключей {missing}")

    sr = raw["sample_rate"]
    if not isinstance(sr, int) or sr <= 0:
        raise ValueError(
            f"{cfg_path}: sample_rate должен быть int > 0, получено {sr!r}"
        )
    window = raw["window_sec"]
    if not isinstance(window, (int, float)) or window <= 0:
        raise ValueError(
            f"{cfg_path}: window_sec должен быть числом > 0, получено {window!r}"
        )
    hop = raw.get("hop_sec", window)
    if not isinstance(hop, (int, float)) or hop <= 0:
        raise ValueError(
            f"{cfg_path}: hop_sec должен быть числом > 0, получено {hop!r}"
        )
    input_kind = raw.get("input_kind", "waveform")
    if input_kind not in INPUT_KINDS:
        raise ValueError(f"{cfg_path}: input_kind {input_kind!r} не из {INPUT_KINDS}")
    aggregate = raw.get("aggregate", "mean")
    if aggregate != "mean":
        raise ValueError(
            f"{cfg_path}: aggregate {aggregate!r} не поддержан (только 'mean')"
        )
    batch = raw.get("batch_size", 1)
    if not isinstance(batch, int) or batch < 1:
        raise ValueError(
            f"{cfg_path}: batch_size должен быть int >= 1, получено {batch!r}"
        )
    dim = raw.get("dim")
    if dim is not None and (not isinstance(dim, int) or dim < 1):
        raise ValueError(
            f"{cfg_path}: dim должен быть int >= 1 или отсутствовать, получено {dim!r}"
        )
    feature_extractor = str(raw.get("feature_extractor", ""))
    if input_kind == "log_mel" and not feature_extractor:
        raise ValueError(
            f"{cfg_path}: input_kind='log_mel' требует 'feature_extractor' "
            "(HF repo id или каталог с preprocessor_config.json)"
        )

    path = Path(str(raw["path"]))
    if not path.is_absolute():
        path = cfg_path.parent / path
    return OnnxConfig(
        name=name,
        path=path,
        sample_rate=int(sr),
        window_sec=float(window),
        hop_sec=float(hop),
        input_name=str(raw["input_name"]),
        output_name=str(raw["output_name"]),
        input_kind=input_kind,
        aggregate=aggregate,
        batch_size=int(batch),
        dim=dim,
        feature_extractor=feature_extractor,
    )


@lru_cache(maxsize=None)
def _session(model_path: str):
    """Ленивая InferenceSession (CPU) — как _load_model в clap.py.

    ort.InferenceSession.run потокобезопасен, в отличие от torch-инференса
    CLAP — глобальный лок не нужен.
    """
    import onnxruntime as ort

    if not Path(model_path).is_file():
        raise ValueError(
            f"файл весов ONNX не найден: {model_path} "
            "(ключ 'path' конфига; референс — scripts/export_clap_onnx.py)"
        )
    return ort.InferenceSession(model_path, providers=["CPUExecutionProvider"])


def _l2_normalize(vec: np.ndarray) -> np.ndarray:
    n = float(np.linalg.norm(vec))
    if n < 1e-12:
        return vec.astype(np.float32)
    return (vec / n).astype(np.float32)


def _chunked(items: list[np.ndarray], size: int) -> Iterator[list[np.ndarray]]:
    for i in range(0, len(items), size):
        yield items[i : i + size]


class OnnxEncoder:
    """Encoder-протокол (embed/base.py) поверх onnxruntime.

    Конструктор только читает конфиг и проверяет наличие пакета — тяжёлое
    (сессия, экстрактор) грузится при первом encode/warm_up.
    """

    def __init__(self, name: str) -> None:
        self.cfg = load_config(name)
        if find_spec("onnxruntime") is None:
            raise ValueError(
                f"пакет onnxruntime не установлен (модель {name!r}) — "
                "установите music-hive[onnx]"
            )
        self.sample_rate = self.cfg.sample_rate
        self._extractor: Any = None  # log_mel: ClapFeatureExtractor, лениво

    def model_key(self) -> str:
        return f"onnx:{self.cfg.name}"

    def warm_up(self) -> None:
        """Прогон нулевого сегмента — грузит сессию до первого encode."""
        self.encode([np.zeros(self._window_samples, dtype=np.float32)])

    # -- окна -------------------------------------------------------------

    @property
    def _window_samples(self) -> int:
        return round(self.cfg.sample_rate * self.cfg.window_sec)

    def _windows(self, y: np.ndarray) -> list[np.ndarray]:
        """Нарезка на окна фиксированной длины W: полные окна шагом hop;
        сегмент короче W → одно окно с нулевым дополнением справа."""
        w = self._window_samples
        hop = round(self.cfg.sample_rate * self.cfg.hop_sec)
        if y.size <= w:
            padded = np.zeros(w, dtype=np.float32)
            padded[: y.size] = y
            return [padded]
        return [y[s : s + w] for s in range(0, y.size - w + 1, hop)]

    # -- инференс ----------------------------------------------------------

    def encode(self, segments: list[np.ndarray]) -> np.ndarray:
        if not segments:
            raise ValueError("пустой список сегментов")
        windows: list[np.ndarray] = []
        for seg in segments:
            y = np.asarray(seg, dtype=np.float32).reshape(-1)
            if y.size:
                windows.extend(self._windows(y))
        if not windows:
            raise ValueError("все сегменты пусты")

        outs = [
            self._run_batch(chunk) for chunk in _chunked(windows, self.cfg.batch_size)
        ]
        emb = np.concatenate(outs, axis=0)  # [n_windows, dim]
        if self.cfg.dim is not None and emb.shape[1] != self.cfg.dim:
            raise ValueError(
                f"{self.model_key()}: фактическая dim {emb.shape[1]} != dim конфига "
                f"{self.cfg.dim} (конфиг может врать — §6.1, проверка факта)"
            )
        emb = emb / np.maximum(np.linalg.norm(emb, axis=1, keepdims=True), 1e-12)
        return _l2_normalize(np.mean(emb, axis=0))

    def _run_batch(self, chunk: list[np.ndarray]) -> np.ndarray:
        """Батч окон → [len(chunk), dim]; feed по input_kind."""
        sess = _session(str(self.cfg.path))
        if self.cfg.input_kind == "log_mel":
            feed = self._mel_feed(sess, chunk)
        else:
            feed = {self.cfg.input_name: np.stack(chunk).astype(np.float32, copy=False)}
        out = sess.run([self.cfg.output_name], feed)[0]
        arr = np.asarray(out, dtype=np.float32)
        if arr.ndim == 1:  # модель без батч-оси (выход [dim])
            arr = arr.reshape(1, -1)
        return arr.reshape(len(chunk), -1)

    def _mel_feed(self, sess: Any, chunk: list[np.ndarray]) -> dict[str, np.ndarray]:
        """log_mel: ClapFeatureExtractor по окнам; выходы экстрактора с
        именами-входами графа идут в feed (input_features, is_longer)."""
        if self._extractor is None:
            from transformers import ClapFeatureExtractor

            fe = ClapFeatureExtractor.from_pretrained(self.cfg.feature_extractor)
            if int(fe.sampling_rate) != self.cfg.sample_rate:
                raise ValueError(
                    f"{self.model_key()}: feature_extractor на {fe.sampling_rate} Гц, "
                    f"конфиг — на {self.cfg.sample_rate}"
                )
            self._extractor = fe
        feats = self._extractor(
            chunk, sampling_rate=self.cfg.sample_rate, return_tensors="np"
        )
        input_names = {i.name for i in sess.get_inputs()}
        feed = {k: v for k, v in feats.items() if k in input_names}
        if self.cfg.input_name not in feed:
            raise ValueError(
                f"{self.model_key()}: граф не имеет входа {self.cfg.input_name!r} "
                f"(есть {sorted(input_names)})"
            )
        return {
            k: (v.astype(np.float32) if v.dtype == bool else v) for k, v in feed.items()
        }
