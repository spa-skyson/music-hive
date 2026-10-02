"""Remote HTTP-энкодер (#31): декод и нарезка окон — на model-сервере (#32).

Контракт с model-сервером (фиксирован, #32):

- GET  {base}/v1/models → {"models": [{"name", "model_key", "dim", "loaded"}]}
  — отсюда берётся СЕРВЕРНЫЙ model_key выбранной модели (истина для
  реестра embedding_models / emb_<slug>, а не локальная сборка ключа);
- POST {base}/v1/embeddings?model=<name> — тело = файл аудио целиком
  (raw bytes), заголовки Authorization: Bearer <token>,
  Content-Type: application/octet-stream; ответ 200:
  {"model_key", "dim", "embedding": [floats]} — сервер сам декодирует
  ffmpeg'ом, режет окна и агрегирует.

HTTP — stdlib urllib (как lyrics/lrclib.py и jobs/runner.py), новых
зависимостей нет. Валидация §6.1 (NaN/норма) — в ValidatedEncoder
фабрики, бэкенд сам себя не валидирует.
"""

from __future__ import annotations

import json
import logging
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

import numpy as np

logger = logging.getLogger(__name__)

# GET /v1/models — лёгкий запрос; POST файла — декод+инференс на сервере,
# могут быть долгими. Константы (не конфиг): подбираются под сервер #32.
LIST_TIMEOUT = 10.0
EMBED_TIMEOUT = 300.0


def _map_http_error(exc: urllib.error.HTTPError, url: str) -> RuntimeError:
    if exc.code == 401:
        return RuntimeError(
            f"{url}: 401 Unauthorized — проверьте MUSIC_HIVE_REMOTE_TOKEN"
        )
    if exc.code == 404:
        return RuntimeError(
            f"{url}: 404 Not Found — модель не найдена на сервере "
            "(MUSIC_HIVE_REMOTE_MODEL)"
        )
    if exc.code == 415:
        return RuntimeError(f"{url}: 415 Unsupported Media Type — сервер не принял формат аудио")
    return RuntimeError(f"{url}: HTTP {exc.code} от model-сервера")


class RemoteEncoder:
    """Encoder-протокол (embed/base.py) поверх HTTP model-сервера.

    sample_rate = 0: декод/ресемплинг/окна — забота сервера, локальных
    сегментов нет (load_segment_audio в pipeline не вызывается). Протокол
    требует encode(segments) — для remote он неприменим, поднимает
    RuntimeError: настоящий путь — encode_file(path) (duck-typing со
    стороны pipeline, как warm_up в F4.1).
    """

    sample_rate = 0

    def __init__(self, base_url: str, model_name: str, token: str) -> None:
        self.base_url = base_url.rstrip("/")
        self.model_name = model_name
        self.token = token
        self._model_key: str | None = None  # кеш GET /v1/models

    def model_key(self) -> str:
        """Ключ реестра — СЕРВЕРНЫЙ model_key выбранной модели (кеш)."""
        if self._model_key is None:
            self._model_key = self._fetch_model_key()
        return self._model_key

    def warm_up(self) -> None:
        """Доступность сервера: GET /v1/models (он же кеш model_key)."""
        self.model_key()

    def encode(self, segments: list[np.ndarray]) -> np.ndarray:
        raise RuntimeError(
            f"{self.model_key() if self._model_key else self.model_name}: remote-энкодер "
            "кодирует файлы целиком на сервере — используйте encode_file(path)"
        )

    def encode_file(self, path: Path) -> np.ndarray:
        """Файл аудио целиком → вектор (декод и окна — на сервере)."""
        url = "{}{}?{}".format(
            self.base_url,
            "/v1/embeddings",
            urllib.parse.urlencode({"model": self.model_name}),
        )
        payload = self._request_json(url, data=Path(path).read_bytes())
        key = str(payload.get("model_key", ""))
        if self._model_key is not None and key != self._model_key:
            # сервер сменил модель под нами → вектор чужой, в emb_<slug>
            # его писать нельзя (доверенная граница, §6.1)
            raise RuntimeError(
                f"{url}: сервер ответил model_key {key!r}, ожидался {self._model_key!r}"
            )
        vec = np.asarray(payload.get("embedding", []), dtype=np.float32).reshape(-1)
        dim = payload.get("dim")
        if vec.size == 0:
            raise RuntimeError(f"{url}: пустой embedding в ответе сервера")
        if isinstance(dim, int) and dim != vec.size:
            raise RuntimeError(
                f"{url}: dim ответа {vec.size} != заявленный {dim} (сервер противоречит себе)"
            )
        return vec

    # -- HTTP -------------------------------------------------------------

    def _fetch_model_key(self) -> str:
        url = f"{self.base_url}/v1/models"
        payload = self._request_json(url)
        models = payload.get("models") if isinstance(payload, dict) else None
        if not isinstance(models, list):
            raise RuntimeError(f"{url}: в ответе нет списка 'models': {payload!r}")
        for m in models:
            if isinstance(m, dict) and m.get("name") == self.model_name:
                key = str(m.get("model_key", ""))
                if not key:
                    raise RuntimeError(
                        f"{url}: модель {self.model_name!r} без model_key"
                    )
                return key
        names = [str(m.get("name")) for m in models if isinstance(m, dict)]
        raise RuntimeError(
            f"модели {self.model_name!r} нет на model-сервере ({url}); "
            f"доступны: {names or 'ничего'}"
        )

    def _request_json(self, url: str, *, data: bytes | None = None) -> dict:
        headers = {"Authorization": f"Bearer {self.token}"}
        method = "GET"
        if data is not None:
            headers["Content-Type"] = "application/octet-stream"
            method = "POST"
        req = urllib.request.Request(url, data=data, method=method, headers=headers)
        timeout = EMBED_TIMEOUT if data is not None else LIST_TIMEOUT
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                body = resp.read()
        except urllib.error.HTTPError as exc:
            raise _map_http_error(exc, url) from exc
        # URLError — подкатегория OSError; таймаут чтения приходит как
        # TimeoutError — тоже OSError. HTTPError ловим раньше него.
        except OSError as exc:
            raise RuntimeError(f"model-сервер недоступен ({url}): {exc}") from exc
        try:
            payload: dict = json.loads(body.decode("utf-8"))
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            raise RuntimeError(f"{url}: некорректный JSON от model-сервера: {exc}") from exc
        if not isinstance(payload, dict):
            raise RuntimeError(f"{url}: ожидается JSON-объект, получено {type(payload).__name__}")
        return payload
