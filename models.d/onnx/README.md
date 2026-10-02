# models.d/onnx — конфиги ONNX-энкодеров

`<имя>.json` + веса `<имя>.onnx` рядом. Схема, env-переменные и инструкция —
[docs/embed-models.md](../../docs/embed-models.md). Веса `*.onnx` в git не
коммитятся (сотни МБ — гигабайты), экспортируйте локально:

    uv run --extra onnx python scripts/export_clap_onnx.py

`clap-reference.json` — референсный конфиг `laion/larger_clap_music_and_speech`
(генерируется скриптом, коммитится как пример схемы).
