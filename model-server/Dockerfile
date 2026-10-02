# model-server (#32): inference-сервис эмбеддингов для отдельной VM.
# Матрица образов (#33) — один Dockerfile, три таргета (docker build --target):
#
#   cpu  (default)  torch-cpu: CLAP+MuQ, ~2.5 ГБ; основной вариант
#   onnx            onnxruntime без torch/transformers, ~400 МБ; требует
#                   примонтированные .onnx + конфиги в /models/onnx
#                   (экспорт весов — scripts/export_clap_onnx.py в корне репо)
#   gpu             torch из cu12x-индекса, HEAVY (~6 ГБ): нужен NVIDIA
#                   Container Toolkit; устройство — MUSIC_HIVE_DEVICE=cuda
#
# `docker build model-server/` без --target собирает ПОСЛЕДНЮЮ стадию,
# поэтому cpu — последняя. Общие слои (ffmpeg, user, healthcheck) — в base.
# Зеркало Harbor: база по умолчанию тянет Docker Hub; для закрытого контура
# замените на cr.home.fwz.ru/dockerhub/python:3.12-slim (зеркало dockerhub).
FROM python:3.12-slim AS base

# ffmpeg — декод аудио (subprocess из encoders.py), ffprobe — длительность.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg \
  && rm -rf /var/lib/apt/lists/*

# CVE-фикс упаковочного инструментария (аудит 2026-09-26), всё предустановлено
# в python:3.12-slim и не запинено в requirements.txt:
#   - jaraco.context CVE-2026-23949 (path traversal), fix >=6.1.0
#   - setuptools  CVE-2025-47273 (path traversal в PackageIndex), fix >=78.1.1
#   - wheel       CVE-2026-24049 (privesc через malicious wheel), fix >=0.46.2
# Ставится под root в system site-packages ДО useradd/USER modelsrv, поэтому
# non-root пользователя не затрагивает; base-стадия общая — onnx/gpu/cpu
# наследуют фикс автоматически.
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install --upgrade "jaraco.context>=6.1.0" "setuptools>=78.1.1" "wheel>=0.46.2"

WORKDIR /srv

COPY requirements.txt ./

# non-root + каталоги: /cache/hf — кеш весов HF (cpu/gpu), /models/onnx —
# точка монтирования .onnx и конфигов (onnx; монтировать :ro — см. README).
RUN useradd --create-home --uid 1000 modelsrv \
  && mkdir -p /cache/hf /models/onnx \
  && chown -R modelsrv:modelsrv /srv /cache /models

EXPOSE 8100

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8100/health', timeout=3)"]

# WORKERS>1 = отдельный процесс на воркер = копия весов в RAM каждой копии
# (обе модели ≈ 6-8 ГБ fp32) — оставляйте 1, пока RAM VM не позволяет больше.
CMD ["sh", "-c", "exec uvicorn app:app --host 0.0.0.0 --port 8100 --workers ${WORKERS:-1}"]

# --- onnx: лёгкий инференс без torch (~400 МБ) ------------------------------
# Общие зависимости дублируют requirements.txt (там — torch/muq/transformers,
# которые сюда сознательно НЕ входят: образу нужен только onnxruntime).
FROM base AS onnx
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install "fastapi>=0.115" "uvicorn[standard]>=0.30" "numpy>=2.0" "onnxruntime>=1.18"
COPY app.py encoders.py ./
ENV PYTHONUNBUFFERED=1 \
    MUSIC_HIVE_ONNX_DIR=/models/onnx
USER modelsrv

# --- gpu: HEAVY (~6 ГБ), CUDA-сборка torch ----------------------------------
# torch из cu12x-индекса отдельно и ДО requirements.txt (иначе muq утащит
# CPU-сборку); CUDA-рантайм идёт внутри torch-вилок. На хосте —
# NVIDIA Container Toolkit; в коде устройство выбирает MUSIC_HIVE_DEVICE.
FROM base AS gpu
ARG TORCH_INDEX_URL=https://download.pytorch.org/whl/cu124
# torchvision из того же индекса — иначе muq тянет PyPI-сборку, не
# совпадающую с установленным torch; явный пин единообразен с cpu.
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install --index-url "${TORCH_INDEX_URL}" "torch>=2.2,<3" "torchaudio>=2.2,<3" "torchvision>=0.20,<0.30"
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install -r requirements.txt
COPY app.py encoders.py ./
ENV PYTHONUNBUFFERED=1 \
    HF_HOME=/cache/hf \
    NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,utility
USER modelsrv

# --- cpu (default): torch из CPU-индекса -------------------------------------
FROM base AS cpu
# torch из CPU-индекса отдельно и ДО requirements.txt (иначе muq утащит
# CUDA-сборку ~2.5 ГБ); тот же приём, что в Dockerfile.worker корня.
ARG TORCH_INDEX_URL=https://download.pytorch.org/whl/cpu
# torchvision из того же индекса — muq тянет PyPI/CUDA-сборку, несовместимую
# с torch+cpu (RuntimeError: operator torchvision::nms does not exist).
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install --index-url "${TORCH_INDEX_URL}" "torch>=2.2,<3" "torchaudio>=2.2,<3" "torchvision>=0.20,<0.30"
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install -r requirements.txt
COPY app.py encoders.py ./
ENV PYTHONUNBUFFERED=1 \
    HF_HOME=/cache/hf
USER modelsrv
