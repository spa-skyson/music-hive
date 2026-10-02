"""model-server (#32): HTTP-сервис эмбеддингов CLAP + MuQ-MuLan + ONNX (#33).

Фиксированный контракт (стучится воркер #31, энкодер remote:<url>):

    GET  /v1/models                      → {"models": [{name, model_key, dim, loaded}]}
    POST /v1/embeddings?model=<name>     → 200 {"model_key", "dim", "embedding"}
                                           тело: raw-байты аудио (octet-stream)
                                           401 нет/неверный Bearer, 404 нет модели,
                                           415 не декодируется, 413 > MAX_BODY_MB,
                                           500 прочее
    GET  /health                         → {"ok": true, "models_loaded": n} — без auth

Auth: Authorization: Bearer $REMOTE_TOKEN на /v1/* (health — открыт).
REMOTE_TOKEN обязателен: пустой → сервис не стартует (fail-closed, как
принято в music-hive). Финальная L2-нормализация вектора — здесь, одна
точка (согласовано с валидациями §6.1 воркера).

Запуск: uvicorn app:app --host 0.0.0.0 --port 8100 (образ — Dockerfile).
Тесты: create_app(encoders=..., settings=...) — инъекция мок-энкодеров.
"""

from __future__ import annotations

import hmac
import logging
import os
import threading
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass

import numpy as np
from encoders import BaseEncoder, DecodeError, build_encoders, l2_normalize
from fastapi import FastAPI, HTTPException, Query, Request
from fastapi.concurrency import run_in_threadpool

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class Settings:
    token: str
    models: tuple[str, ...]
    warmup: bool
    max_body_bytes: int
    decode_timeout: float


def settings_from_env(env: dict[str, str] | None = None) -> Settings:
    env = dict(os.environ if env is None else env)
    return Settings(
        token=env.get("REMOTE_TOKEN", "").strip(),
        models=tuple(
            m.strip()
            for m in env.get("MODELS", "clap,muq").split(",")
            if m.strip()
        ),  # значения: clap | muq | onnx:<имя конфига> (#33)
        warmup=env.get("WARMUP", "1").strip().lower() not in ("0", "false", "no"),
        max_body_bytes=int(float(env.get("MAX_BODY_MB", "100")) * 1024 * 1024),
        decode_timeout=float(env.get("DECODE_TIMEOUT", "120")),
    )


def _warmup(encoders: dict[str, BaseEncoder]) -> None:
    """Прогрев в фоне: /health отвечает сразу, models_loaded растёт по мере
    загрузки весов (700M параметров с диска — не секунды)."""
    for enc in encoders.values():
        try:
            enc.warm_up()
            logger.info("warmup done: %s", enc.model_key())
        except Exception:
            logger.exception(
                "warmup failed: %s — загрузится при первом запросе", enc.name
            )


def create_app(
    encoders: dict[str, BaseEncoder] | None = None,
    settings: Settings | None = None,
) -> FastAPI:
    st = settings or settings_from_env()
    if not st.token:  # fail-closed: без токена сервис не поднимается
        raise RuntimeError(
            "REMOTE_TOKEN не задан/пуст — отказываюсь стартовать (fail-closed). "
            "Передайте токен через env REMOTE_TOKEN."
        )
    enc = encoders if encoders is not None else build_encoders(st.models)

    @asynccontextmanager
    async def lifespan(_app: FastAPI) -> AsyncIterator[None]:
        if st.warmup:
            threading.Thread(
                target=_warmup, args=(enc,), daemon=True, name="model-warmup"
            ).start()
        yield

    app = FastAPI(
        title="music-hive model-server",
        version="1.0.0",
        lifespan=lifespan,
        docs_url=None,
        redoc_url=None,
        openapi_url=None,
    )

    def _auth(request: Request) -> None:
        scheme, _, cred = request.headers.get("authorization", "").partition(" ")
        if (
            scheme.lower() != "bearer"
            or not cred
            or not hmac.compare_digest(cred.encode(), st.token.encode())
        ):
            raise HTTPException(
                status_code=401,
                detail="invalid or missing bearer token",
                headers={"WWW-Authenticate": "Bearer"},
            )

    @app.get("/health")
    def health() -> dict[str, object]:
        return {"ok": True, "models_loaded": sum(1 for e in enc.values() if e.loaded)}

    @app.get("/v1/models")
    def models(request: Request) -> dict[str, object]:
        _auth(request)
        return {
            "models": [
                {
                    "name": e.name,
                    "model_key": e.model_key(),
                    "dim": e.dim,
                    "loaded": e.loaded,
                }
                for e in enc.values()
            ]
        }

    @app.post("/v1/embeddings")
    async def embeddings(
        request: Request, model: str = Query(default="")
    ) -> dict[str, object]:
        _auth(request)
        enc_obj = enc.get(model)
        if enc_obj is None:
            raise HTTPException(
                status_code=404,
                detail=f"model {model!r} not found; available: {sorted(enc)}",
            )
        content_length = request.headers.get("content-length")
        if (
            content_length
            and content_length.isdigit()
            and int(content_length) > st.max_body_bytes
        ):
            raise HTTPException(
                status_code=413,
                detail=f"request body {int(content_length)} bytes exceeds "
                f"MAX_BODY_MB ({st.max_body_bytes} bytes)",
            )
        data = await request.body()
        if len(data) > st.max_body_bytes:
            raise HTTPException(
                status_code=413, detail="request body exceeds MAX_BODY_MB"
            )
        if not data:
            raise HTTPException(status_code=415, detail="empty request body")
        try:
            # блокирующий декод+инференс — в threadpool, event loop жив (/health)
            vec = await run_in_threadpool(enc_obj.embed_file, data)
        except DecodeError as exc:
            raise HTTPException(status_code=415, detail=str(exc)) from exc
        except Exception as exc:
            logger.exception("embedding failed: model=%s", model)
            raise HTTPException(
                status_code=500, detail=f"embedding failed: {exc}"
            ) from exc
        vec = np.asarray(vec, dtype=np.float32).reshape(-1)
        if not np.all(np.isfinite(vec)):
            raise HTTPException(status_code=500, detail="embedding contains NaN/Inf")
        vec = l2_normalize(vec)
        return {
            "model_key": enc_obj.model_key(),
            "dim": int(vec.size),
            "embedding": vec.tolist(),
        }

    return app


app = create_app()
