#!/usr/bin/env bash
# CLAP embed on AMD ROCm (RX 9070 / gfx1201). Uses .venv-rocm (Python 3.12 + torch ROCm).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if [[ ! -x .venv-rocm/bin/music-hive ]]; then
  echo "Missing .venv-rocm — create with Python 3.12 + ROCm torch first." >&2
  exit 1
fi

export MUSIC_HIVE_ROOT="${MUSIC_HIVE_ROOT:-$ROOT}"
export MUSIC_HIVE_DB_PATH="${MUSIC_HIVE_DB_PATH:-$ROOT/data/db/music-hive.db}"
export LD_LIBRARY_PATH="${ROOT}/.rocm-extra/opt/rocm/lib:/opt/rocm/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"

exec .venv-rocm/bin/music-hive embed "$@"
