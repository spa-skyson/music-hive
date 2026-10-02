# Third-party software

This repository is a fork of the upstream [musik](https://github.com/torwin-job/musik)
project (MIT, © musik contributors). The origin and attribution of the fork
are also recorded in `LICENSE` and in the «Происхождение» section of `README.md`.

The project is distributed under GPL-3.0 (see `LICENSE`). Original code
inherited from the MIT-licensed musik fork is relicensed under GPL-3.0 with
attribution preserved. Dependencies remain under their respective licenses.

[Navidrome](https://github.com/navidrome/navidrome) — референс и источник
заимствованного кода для подсистемы Subsonic `/rest/*` (GPL-3.0,
© 2016—2026 Navidrome contributors). Заимствованные части (player/internal/
subsonic: конверт subsonic-response, коды ошибок, middleware POST-мержа и
аутентификации, маршрутизация путей с `.view`, DTO и формы ответов
browse-каталога и списков альбомов) остаются GPL-3.0; файлы
с заимствованиями несут уведомление в шапке.

> Памятка GPL-3.0: при распространении — исходники обязаны идти с продуктом.

Important runtime components include:

- PyTorch, Transformers, librosa and their Python dependencies;
- the CLAP model and model weights downloaded from their upstream provider;
- [MuQ / MuQ-MuLan](https://github.com/tencent-ailab/MuQ) (Tencent AI Lab):
  the `muq` package code is MIT, but the model weights
  (`OpenMuQ/MuQ-MuLan-large`) are released under **CC-BY-NC 4.0**
  (non-commercial) — the weights are used optionally in `model-server/`
  (task #32); see the license notice in `model-server/README.md` before
  exposing that service publicly or using it commercially;
- modernc SQLite and other Go modules listed in `player/go.sum`;
- Flutter and plugins listed in `mobile/flutter/pubspec.lock`;
- ffmpeg supplied by the host or container base distribution.

This repository does not intentionally vendor model weights, music, ffmpeg
binaries or package-manager caches. Before redistributing a container image or
mobile binary, review the exact licenses of the resolved dependency versions
and the selected model weights. An ffmpeg build may enable optional codecs
with additional licensing requirements.
