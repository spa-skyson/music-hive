#!/usr/bin/env python3
"""Экспорт референсной CLAP-модели в ONNX (F4.2, #28).

`laion/larger_clap_music_and_speech` → models.d/onnx/clap-reference.onnx
+ clap-reference.json. Полная инструкция и чек-лист — docs/embed-models.md.

Запуск (нужны сеть до huggingface.co, ~2.5 ГБ под веса и extra onnx):

    uv run --extra onnx python scripts/export_clap_onnx.py [--repo ID] [--out DIR]

Почему torch.onnx.export, а не optimum-cli: у optimum нет ONNX-конфига для
архитектуры clap (см. optimum/exporters/onnx/model_configs.py), а экспорт
ClapModel.forward даёт текстовые входы/выходы без audio_projection. Здесь
экспортируется обёртка get_audio_features тем же движком (torch.onnx.export —
то, что optimum использует под капотом); вариант с optimum-cli — для инспекции
графа, описан в docs/embed-models.md.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]

REPO_ID = "laion/larger_clap_music_and_speech"
SR = 48_000
WINDOW_SEC = 30.0  # = сегменту пайплайна: экстрактор сам делает fusion/repeatpad
DIM = 512

# Конфиг генерируется 1-в-1 с закоммиченным models.d/onnx/clap-reference.json.
CONFIG = {
    "path": "clap-reference.onnx",
    "sample_rate": SR,
    "window_sec": WINDOW_SEC,
    "hop_sec": WINDOW_SEC,
    "input_name": "input_features",
    "output_name": "embedding",
    "input_kind": "log_mel",
    "feature_extractor": REPO_ID,
    "aggregate": "mean",
    "batch_size": 1,
    "dim": DIM,
}


class ClapAudioEmbed:
    """get_audio_features (transformers ≥5) как nn.Module для экспорта.

    Входы — выходы ClapFeatureExtractor: input_features [B, 4, 1001, 64]
    float32 и is_longer [B, 1] (bool → float32). Выход — L2-нормированный
    512-d вектор (F.normalize, как в get_audio_features).
    """

    def __init__(self, repo: str) -> None:
        from transformers import ClapModel

        clap = ClapModel.from_pretrained(repo).eval()
        self.audio_model = clap.audio_model
        self.audio_projection = clap.audio_projection

    def __call__(self, input_features, is_longer):
        import torch.nn.functional as F

        pooled = self.audio_model(
            input_features=input_features, is_longer=is_longer
        ).pooler_output
        return F.normalize(self.audio_projection(pooled), dim=-1)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", default=REPO_ID, help="HF repo модели")
    parser.add_argument(
        "--out", default=str(ROOT / "models.d" / "onnx"), help="каталог конфигов"
    )
    args = parser.parse_args()
    repo = args.repo

    import torch
    from onnxruntime import InferenceSession
    from transformers import ClapFeatureExtractor

    out_dir = Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)

    print(f"[*] загрузка {repo} (первый запуск скачает ~2.5 ГБ)…")
    fe = ClapFeatureExtractor.from_pretrained(repo)
    if int(fe.sampling_rate) != SR:
        raise SystemExit(f"FAIL: extractor на {fe.sampling_rate} Гц, ожидалось {SR}")
    embed = ClapAudioEmbed(repo)

    # 30-секундный сегмент → is_longer=True → путь fusion, как в прод-пайплайне
    rng = np.random.default_rng(0)
    dummy = rng.standard_normal(SR * int(WINDOW_SEC)).astype(np.float32)
    feats = fe([dummy], sampling_rate=SR, return_tensors="np")
    input_features = feats["input_features"].astype(np.float32)
    is_longer = feats["is_longer"].astype(np.float32)
    print(
        f"[*] вход экстрактора: input_features={input_features.shape} is_longer={is_longer.shape}"
    )

    onnx_path = out_dir / "clap-reference.onnx"
    with torch.no_grad():
        torch.onnx.export(
            embed,
            (torch.from_numpy(input_features), torch.from_numpy(is_longer)),
            str(onnx_path),
            input_names=["input_features", "is_longer"],
            output_names=["embedding"],
            dynamic_axes={
                "input_features": {0: "batch"},
                "is_longer": {0: "batch"},
                "embedding": {0: "batch"},
            },
            opset_version=17,
            do_constant_folding=True,
        )

    # --- проверка факта (§6.1: конфиг может врать) ---
    with torch.no_grad():
        ref = embed(
            torch.from_numpy(input_features), torch.from_numpy(is_longer)
        ).numpy()
    got = InferenceSession(str(onnx_path), providers=["CPUExecutionProvider"]).run(
        ["embedding"],
        {"input_features": input_features, "is_longer": is_longer},
    )[0]
    dim = int(got.shape[-1])
    cos = float(
        np.dot(ref.reshape(-1), got.reshape(-1))
        / (np.linalg.norm(ref) * np.linalg.norm(got))
    )
    print(f"[*] dim={dim} cosine(torch, onnx)={cos:.6f}")
    if dim != DIM:
        raise SystemExit(f"FAIL: dim {dim} != {DIM}")
    if cos < 0.999:
        raise SystemExit(f"FAIL: cosine {cos:.6f} < 0.999 — экспорт неточный")
    for vec in (ref, got):
        if not np.all(np.isfinite(vec)):
            raise SystemExit("FAIL: NaN/Inf в выходе")

    cfg_path = out_dir / "clap-reference.json"
    config = {**CONFIG, "feature_extractor": repo}
    cfg_path.write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")
    print(f"[+] OK: {onnx_path}\n[+] конфиг: {cfg_path}")
    print(
        "[i] переключение: MUSIC_HIVE_EMBEDDING_MODEL=onnx:clap-reference (docs/embed-models.md)"
    )


if __name__ == "__main__":
    main()
