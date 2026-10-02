"""Энкодеры model-server (#32): CLAP + MuQ-MuLan + ONNX (#33), ленивая загрузка.

Самодостаточный модуль (не импортирует src/music_hive — образ живёт своей
жизнью). Протокол энкодера:

    name          — slug из HTTP-контракта ('clap' | 'muq' | '<onnx-имя>')
    sample_rate   — частота декода ffmpeg (CLAP 48k, MuQ 24k; onnx — из конфига)
    dim           — размерность эмбеддинга (CLAP/MuQ: 512; onnx — из конфига)
    model_key()   — ключ, СОГЛАСОВАННЫЙ с воркером (#31) → emb_<slug>
    loaded        — модель в RAM (для /v1/models и /health)
    embed_file()  — raw-байты аудиофайла → агрегированный вектор

Окна: CLAP — 3×30с start/middle/end (та же стратегия clap_3x30_v1, что в
пайплайне music-hive: per-window L2 → mean); MuQ-MuLan сам режет длинный
wav на 10-секундные клипы и усредняет латенты (extract_audio_latents,
исходники muq) — подаём весь декодированный трек, fp32 (требование README
MuQ: fp32 против NaN). Финальная L2-нормализация — в app.py (одна точка,
согласовано с валидациями §6.1 воркера).

Декод — subprocess ffmpeg (есть в образе): mono float32 на нужном sr.
Не декодируется → DecodeError → HTTP 415.
"""

from __future__ import annotations

import json
import logging
import os
import shutil
import subprocess
import tempfile
import threading
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path

import numpy as np

logger = logging.getLogger(__name__)

# Ключи зафиксированы контрактом с воркером (#31) — не выводить из repo-имени.
CLAP_REPO = "laion/larger_clap_music_and_speech"
CLAP_MODEL_KEY = "clap:laion/larger_clap_music_and_speech"
MUQ_REPO = "OpenMuQ/MuQ-MuLan-large"
MUQ_MODEL_KEY = "muq:MuQ-MuLan-large"

# torch-устройство (#33, gpu-образ): cpu по умолчанию; на gpu-варианте образа
# ставьте MUSIC_HIVE_DEVICE=cuda (нужен NVIDIA Container Toolkit на хосте).
DEVICE = os.environ.get("MUSIC_HIVE_DEVICE", "cpu")

# ONNX-конфиги (#33): формат models.d/onnx/<name>.json воркера (#28,
# docs/embed-models.md). В образе — /models/onnx (точка монтирования),
# локально — env MUSIC_HIVE_ONNX_DIR.
DEFAULT_ONNX_DIR = "/models/onnx"
_ONNX_REQUIRED_KEYS = ("path", "sample_rate", "window_sec", "input_name", "output_name")

CLAP_SEGMENT_SEC = 30.0  # стратегия clap_3x30_v1 пайплайна music-hive
# dim_latent OpenMuQ/MuQ-MuLan-large = 512 (config.json модели; сверяется при загрузке)
MUQ_DIM = 512

_DECODE_TIMEOUT = float(os.environ.get("DECODE_TIMEOUT", "120"))

# ponytail: один глобальный лок на весь инференс (CPU, две модели в одном
# процессе) — torch-инференс не потокобезопасен и параллелить его на CPU
# бессмысленно; per-model локи если появится GPU и throughput станет узким местом.
INFER_LOCK = threading.Lock()


class DecodeError(ValueError):
    """Аудио не декодируется (ffmpeg/ffprobe) → HTTP 415."""


def _ffmpeg_bin() -> str:
    ffmpeg = shutil.which("ffmpeg")
    if not ffmpeg:
        raise DecodeError("ffmpeg не найден в образе/окружении")
    return ffmpeg


def _ffprobe_bin() -> str:
    ffprobe = shutil.which("ffprobe")
    if not ffprobe:
        raise DecodeError("ffprobe не найден в образе/окружении")
    return ffprobe


@contextmanager
def temp_audio(data: bytes) -> Iterator[Path]:
    """Байты тела запроса → временный файл (ffmpeg сниффит контейнер по
    содержимому; pipe не подходит для m4a с moov в конце)."""
    fd, name = tempfile.mkstemp(suffix=".audio", prefix="model-server-")
    os.close(fd)
    path = Path(name)
    try:
        path.write_bytes(data)
        yield path
    finally:
        path.unlink(missing_ok=True)


def ffprobe_duration(path: Path) -> float:
    """Длительность аудио в секундах; ошибка → DecodeError."""
    try:
        out = subprocess.run(
            [
                _ffprobe_bin(),
                "-v",
                "error",
                "-show_entries",
                "format=duration",
                "-of",
                "default=noprint_wrappers=1:nokey=1",
                str(path),
            ],
            capture_output=True,
            timeout=30,
            check=True,
        ).stdout
        return float(out.decode().strip())
    except (
        subprocess.CalledProcessError,
        subprocess.TimeoutExpired,
        ValueError,
    ) as exc:
        raise DecodeError(f"ffprobe не смог прочитать аудио: {exc}") from exc


def decode_audio(
    path: Path,
    *,
    sample_rate: int,
    offset_sec: float = 0.0,
    duration_sec: float | None = None,
) -> np.ndarray:
    """Файл → mono float32 ndarray на sample_rate (окно [offset, offset+duration))."""
    cmd = [
        _ffmpeg_bin(),
        "-nostdin",
        "-v",
        "error",
        "-ss",
        f"{max(offset_sec, 0.0):.3f}",
    ]
    if duration_sec is not None:
        cmd += ["-t", f"{max(duration_sec, 0.05):.3f}"]
    cmd += [
        "-i",
        str(path),
        "-f",
        "f32le",
        "-acodec",
        "pcm_f32le",
        "-ac",
        "1",
        "-ar",
        str(int(sample_rate)),
        "pipe:1",
    ]
    try:
        proc = subprocess.run(
            cmd, capture_output=True, timeout=_DECODE_TIMEOUT, check=False
        )
    except subprocess.TimeoutExpired as exc:
        raise DecodeError(f"таймаут декода ffmpeg (> {_DECODE_TIMEOUT:.0f}с)") from exc
    if proc.returncode != 0:
        raise DecodeError(
            f"ffmpeg не декодировал аудио: {proc.stderr.decode(errors='replace')[-500:]}"
        )
    y = np.frombuffer(proc.stdout, dtype=np.float32)
    if y.size == 0:
        raise DecodeError("ffmpeg вернул пустой аудиопоток")
    return y.copy()


def plan_windows(
    duration_sec: float, segment_sec: float = CLAP_SEGMENT_SEC
) -> list[tuple[str, float, float]]:
    """Копия стратегии clap_3x30_v1 (segments.py music-hive): ≤ сегмента —
    весь файл одним окном; иначе start/middle/end с дедупликацией близких
    offset'ов. Возвращает (name, offset_sec, duration_sec)."""
    if duration_sec <= 0:
        return []
    if duration_sec <= segment_sec + 0.05:
        return [("full", 0.0, duration_sec)]
    candidates = [
        ("start", 0.0, segment_sec),
        ("middle", max(0.0, duration_sec / 2.0 - segment_sec / 2.0), segment_sec),
        ("end", max(0.0, duration_sec - segment_sec), segment_sec),
    ]
    unique: list[tuple[str, float, float]] = []
    for win in candidates:
        if any(abs(win[1] - u[1]) < 0.5 for u in unique):
            continue
        unique.append(win)
    return unique


def l2_normalize(vec: np.ndarray) -> np.ndarray:
    n = float(np.linalg.norm(vec))
    if n < 1e-12:
        return vec.astype(np.float32)
    return (vec / n).astype(np.float32)


class BaseEncoder:
    """Общий каркас: ленивая модель, модель грузится при первом embed/warm_up."""

    name: str = ""
    sample_rate: int = 0
    dim: int = 0

    def __init__(self) -> None:
        self._model: object | None = None

    def model_key(self) -> str:  # pragma: no cover - перекрывается
        raise NotImplementedError

    @property
    def loaded(self) -> bool:
        return self._model is not None

    def warm_up(self) -> None:
        """Прогрев = загрузка весов в RAM (тяжёлый инференс не нужен)."""
        self._ensure_model()

    def embed_file(self, data: bytes) -> np.ndarray:
        with temp_audio(data) as path:
            y = decode_audio(path, sample_rate=self.sample_rate)
        return self.embed_waveform(y)

    def embed_waveform(self, y: np.ndarray) -> np.ndarray:  # pragma: no cover
        raise NotImplementedError

    def _ensure_model(self) -> object:  # pragma: no cover
        raise NotImplementedError


class ClapEncoder(BaseEncoder):
    """laion/larger_clap_music_and_speech (transformers ClapModel, MIT):
    48kHz, 512-d; per-window L2 → mean (финальный L2 — app.py)."""

    name = "clap"
    sample_rate = 48_000
    dim = 512

    def model_key(self) -> str:
        return CLAP_MODEL_KEY

    def embed_file(self, data: bytes) -> np.ndarray:
        vectors: list[np.ndarray] = []
        with temp_audio(data) as path:
            duration = ffprobe_duration(path)
            windows = plan_windows(duration)
            for _name, offset, dur in windows:
                y = decode_audio(
                    path,
                    sample_rate=self.sample_rate,
                    offset_sec=offset,
                    duration_sec=dur,
                )
                vectors.append(self.embed_waveform(y))
        return np.mean(np.stack(vectors, axis=0), axis=0)

    def _ensure_model(self) -> object:
        if self._model is not None:
            return self._model
        from transformers import ClapModel, ClapProcessor

        logger.info(
            "Loading CLAP %s (weights cache: HF_HOME=%s)",
            CLAP_REPO,
            os.environ.get("HF_HOME", "~/.cache/huggingface"),
        )
        try:  # сначала офлайн (кеш уже скачан) — как embed/clap.py воркера
            processor = ClapProcessor.from_pretrained(CLAP_REPO, local_files_only=True)
            model = ClapModel.from_pretrained(CLAP_REPO, local_files_only=True)
        except (OSError, ValueError):
            processor = ClapProcessor.from_pretrained(CLAP_REPO)
            model = ClapModel.from_pretrained(CLAP_REPO)
        model.eval().to(DEVICE)  # MUSIC_HIVE_DEVICE=cuda на gpu-образе (#33)
        self._model = (processor, model)
        return self._model

    def embed_waveform(self, y: np.ndarray) -> np.ndarray:
        import torch

        processor, model = self._ensure_model()
        with INFER_LOCK:
            inputs = processor(
                audio=[y],  # transformers>=5: kw `audio` (мн. `audios` падает)
                sampling_rate=self.sample_rate,
                return_tensors="pt",
                padding=True,
            )
            inputs = {
                k: v.to(DEVICE)
                for k, v in inputs.items()
                if torch.is_tensor(v)
            }
            with torch.no_grad():
                out = model.get_audio_features(**inputs)
                feats = getattr(out, "pooler_output", None)
                if feats is None:
                    feats = out
                vec = feats[0].detach().cpu().float().numpy().reshape(-1)
        if vec.size < 32:
            raise RuntimeError(f"CLAP вернул неожиданную размерность {vec.size}")
        return l2_normalize(vec)  # per-window L2, как в пайплайне music-hive


def _patch_easydict_attn(model: object) -> int:
    """Проставляет `_attn_implementation = "eager"` всем EasyDict-конфигам
    модулей модели; возвращает число пропатченных.

    muq 0.1.0 (Dec 2024) передаёт свои EasyDict-конфиги в transformers-класс
    Wav2Vec2ConformerSelfAttention; transformers 5.x читают
    `config._attn_implementation` прямо в forward (masking_utils) →
    AttributeError на EasyDict без этого атрибута. EasyDict позволяет
    установить атрибут, поэтому патч после from_pretrained достаточен.

    CLAP не затронут: там нативный PretrainedConfig (атрибут есть всегда).
    """
    try:
        from easydict import EasyDict
    except ImportError:  # нет easydict → нет и этой проблемы
        return 0
    patched = 0
    for _name, module in model.named_modules():  # type: ignore[attr-defined]
        cfg = getattr(module, "config", None)
        # EasyDict хранит атрибуты как ключи dict — `in` покрывает оба варианта
        if isinstance(cfg, EasyDict) and "_attn_implementation" not in cfg:
            cfg._attn_implementation = "eager"
            patched += 1
    logger.info(
        "%s: _attn_implementation=eager проставлен %d EasyDict-конфигам",
        MUQ_MODEL_KEY,
        patched,
    )
    return patched


class MuqEncoder(BaseEncoder):
    """OpenMuQ/MuQ-MuLan-large (пакет muq): 24kHz, fp32, dim_latent=512.
    Веса — CC-BY-NC 4.0 (см. README и THIRD_PARTY.md). Модель сама режет
    длинный wav на 10-с клипы и усредняет латенты (extract_audio_latents),
    поэтому подаём весь трек целиком."""

    name = "muq"
    sample_rate = 24_000
    dim = MUQ_DIM

    def model_key(self) -> str:
        return MUQ_MODEL_KEY

    def _ensure_model(self) -> object:
        if self._model is not None:
            return self._model
        from muq import MuQMuLan

        logger.info("Loading MuQ-MuLan %s (fp32 %s)", MUQ_REPO, DEVICE)
        model = MuQMuLan.from_pretrained(MUQ_REPO)
        _patch_easydict_attn(model)  # transformers 5.x vs muq EasyDict (API drift)
        model.eval().to(DEVICE)  # CPU fp32: NaN-риски на половинной точности
        # config.mulan — MuLanConfig (dataclass) при local-инстанцировании или
        # EasyDict (dict) при from_pretrained; ждём оба варианта.
        mulan_cfg = getattr(model.config, "mulan", None)
        dim_latent = getattr(mulan_cfg, "dim_latent", None)
        if dim_latent is None and hasattr(mulan_cfg, "get"):
            dim_latent = mulan_cfg.get("dim_latent")
        actual_dim = int(dim_latent or MUQ_DIM)
        if actual_dim != self.dim:
            logger.warning(
                "%s: фактическая dim_latent %d != ожидаемой %d — обновляю",
                MUQ_MODEL_KEY,
                actual_dim,
                self.dim,
            )
            self.dim = actual_dim
        self._model = model
        return self._model

    def embed_waveform(self, y: np.ndarray) -> np.ndarray:
        import torch

        model = self._ensure_model()
        wavs = torch.from_numpy(
            np.ascontiguousarray(y, dtype=np.float32)
        ).to(DEVICE).unsqueeze(0)  # [1, T] fp32 @ 24kHz
        with INFER_LOCK, torch.no_grad():
            latents = model(wavs=wavs)
        return latents[0].detach().cpu().float().numpy().reshape(-1)


def _onnx_dir() -> Path:
    """Каталог ONNX-конфигов: env-override → /models/onnx (точка монтирования)."""
    return Path(os.environ.get("MUSIC_HIVE_ONNX_DIR", DEFAULT_ONNX_DIR))


def load_onnx_config(name: str) -> dict:
    """Читает models.d/onnx/<name>.json (формат воркера #28) и валидирует
    минимум, нужный энкодеру. Ошибки — ValueError (fail-closed на старте)."""
    if not name or "/" in name or "\\" in name or name.startswith("."):
        raise ValueError(f"некорректное имя ONNX-модели {name!r}")
    path = _onnx_dir() / f"{name}.json"
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise ValueError(
            f"конфиг ONNX-модели {name!r} не найден: {path} "
            "(положите <имя>.json + <имя>.onnx в каталог монтирования или "
            "задайте MUSIC_HIVE_ONNX_DIR; экспорт — scripts/export_clap_onnx.py)"
        ) from exc
    except json.JSONDecodeError as exc:
        raise ValueError(f"битый JSON в {path}: {exc}") from exc
    if not isinstance(raw, dict):
        raise ValueError(f"{path}: ожидается JSON-объект")
    missing = [k for k in _ONNX_REQUIRED_KEYS if k not in raw]
    if missing:
        raise ValueError(f"{path}: не хватает ключей {missing}")
    if raw.get("input_kind", "waveform") != "waveform":
        # log_mel требует ClapFeatureExtractor (transformers) — его в
        # onnx-образе сознательно нет; такие модели остаются на воркере.
        raise ValueError(
            f"{path}: input_kind={raw['input_kind']!r} не поддержан "
            "(model-server: только waveform)"
        )
    if raw.get("aggregate", "mean") != "mean":
        raise ValueError(
            f"{path}: aggregate={raw['aggregate']!r} не поддержан (только 'mean')"
        )
    return raw


class OnnxEncoder(BaseEncoder):
    """ONNX-энкодер (#33): конфиг <onnx-dir>/<name>.json (формат воркера #28),
    ленивая onnxruntime-сессия (CPU providers).

    model_key `onnx:<name>` совпадает с реестром воркера (таблица
    emb_onnx_<name>), математика — 1-в-1 с embed/onnx.py воркера: трек →
    окна window_sec (шаг hop_sec, короткий трек дополняется нулями) →
    батч-инференс → per-window L2 → mean → L2. ort session.run
    потокобезопасен — INFER_LOCK не нужен (как у воркера).
    """

    def __init__(self, name: str) -> None:
        super().__init__()
        raw = load_onnx_config(name)
        self.name = name
        self.sample_rate = int(raw["sample_rate"])
        self.window_sec = float(raw["window_sec"])
        self.hop_sec = float(raw.get("hop_sec", self.window_sec))
        self.input_name = str(raw["input_name"])
        self.output_name = str(raw["output_name"])
        self.batch_size = int(raw.get("batch_size", 1))
        # dim опционален в конфиге: до первого инференса неизвестна (0),
        # фиксируется фактом в embed_waveform (как у MuqEncoder).
        self.dim = int(raw["dim"]) if raw.get("dim") else 0
        weights = Path(str(raw["path"]))  # относительный — от каталога конфигов
        self._model_path = weights if weights.is_absolute() else _onnx_dir() / weights

    def model_key(self) -> str:
        return f"onnx:{self.name}"

    def _ensure_model(self) -> object:
        if self._model is None:
            import onnxruntime as ort

            if not self._model_path.is_file():
                raise ValueError(
                    f"файл весов ONNX не найден: {self._model_path} "
                    "(ключ 'path' конфига; экспорт — scripts/export_clap_onnx.py)"
                )
            logger.info("Loading ONNX session %s (CPU providers)", self._model_path)
            self._model = ort.InferenceSession(
                str(self._model_path), providers=["CPUExecutionProvider"]
            )
        return self._model

    def embed_waveform(self, y: np.ndarray) -> np.ndarray:
        sess = self._ensure_model()
        w = round(self.sample_rate * self.window_sec)
        hop = round(self.sample_rate * self.hop_sec)
        if y.size <= w:  # короткий трек → одно нулём-дополненное окно
            padded = np.zeros(w, dtype=np.float32)
            padded[: y.size] = y
            windows = [padded]
        else:
            windows = [y[s : s + w] for s in range(0, y.size - w + 1, hop)]
        outs = []
        for i in range(0, len(windows), self.batch_size):
            chunk = np.stack(windows[i : i + self.batch_size]).astype(
                np.float32, copy=False
            )
            out = sess.run([self.output_name], {self.input_name: chunk})[0]
            outs.append(
                np.asarray(out, dtype=np.float32).reshape(len(chunk), -1)
            )
        emb = np.concatenate(outs, axis=0)  # [n_windows, dim]
        if self.dim and emb.shape[1] != self.dim:
            raise ValueError(
                f"{self.model_key()}: фактическая dim {emb.shape[1]} != dim "
                f"конфига {self.dim}"
            )
        if not self.dim:
            self.dim = int(emb.shape[1])
        emb = emb / np.maximum(
            np.linalg.norm(emb, axis=1, keepdims=True), 1e-12
        )  # per-window L2 — как у воркера
        return l2_normalize(np.mean(emb, axis=0))


# Фабрика по slug'ам контракта; неизвестный slug → ValueError (startup fail).
FACTORIES: dict[str, type[BaseEncoder]] = {"clap": ClapEncoder, "muq": MuqEncoder}


def build_encoders(names: tuple[str, ...] | list[str]) -> dict[str, BaseEncoder]:
    encoders: dict[str, BaseEncoder] = {}
    for name in names:
        if name.startswith("onnx:"):  # onnx:<имя конфига> (#33)
            enc: BaseEncoder = OnnxEncoder(name.partition(":")[2])
        else:
            factory = FACTORIES.get(name)
            if factory is None:
                raise ValueError(
                    f"неизвестная модель {name!r}; доступные: "
                    f"{sorted(FACTORIES)} или onnx:<имя конфига>"
                )
            enc = factory()
        # ключ = значение MODELS / параметр ?model= ('onnx:toy' целиком)
        encoders[name] = enc
    if not encoders:
        raise ValueError(
            "MODELS пуст — укажите хотя бы одну модель (clap, muq, onnx:<имя>)"
        )
    return encoders
