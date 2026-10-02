from __future__ import annotations

from pathlib import Path

import numpy as np
import soundfile as sf

from music_hive.scanner.hashing import file_md5
from music_hive.scanner.tags import read_tags


def _make_wav(path: Path, seconds: float = 1.5, freq: float = 440.0) -> None:
    sr = 22050
    t = np.linspace(0, seconds, int(sr * seconds), endpoint=False)
    y = (0.2 * np.sin(2 * np.pi * freq * t)).astype(np.float32)
    sf.write(path, y, sr)


def test_md5_and_tags(tmp_path: Path):
    wav = tmp_path / "01 - Test Song.wav"
    _make_wav(wav)
    assert len(file_md5(wav)) == 32
    tags = read_tags(wav)
    assert tags.title == "Test Song"
