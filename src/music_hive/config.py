from __future__ import annotations

import logging
import os
from collections.abc import Mapping
from functools import lru_cache
from pathlib import Path

from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict

ROOT = Path(__file__).resolve().parents[2]

logger = logging.getLogger(__name__)


def legacy_env_vars(environ: Mapping[str, str] | None = None) -> list[str]:
    """Имена переменных старого формата musik: префикс MUSIK_, но не MUSIC_HIVE_."""
    env = os.environ if environ is None else environ
    return sorted(
        name
        for name in env
        if name.startswith("MUSIK_") and not name.startswith("MUSIC_HIVE_")
    )


class Settings(BaseSettings):
    model_config = SettingsConfigDict(
        env_prefix="MUSIC_HIVE_", env_file=".env", extra="ignore"
    )

    # Единственный бэкенд — PostgreSQL (psycopg3 + пул, #11 F5).
    database_url: str = ""
    library: Path = Field(default_factory=lambda: ROOT / "data" / "music")
    data_dir: Path = Field(default_factory=lambda: ROOT / "data")
    artwork_cache: Path = Field(
        default_factory=lambda: ROOT / "data" / "cache" / "artwork"
    )

    extensions: tuple[str, ...] = (".mp3", ".flac", ".m4a", ".wav", ".ogg", ".opus")
    analysis_seconds: float = 60.0
    workers: int = 4
    embedding_model: str = "clap"
    clap_model: str = "laion/larger_clap_music_and_speech"
    # Каталог конфигов ONNX-энкодеров (MUSIC_HIVE_EMBEDDING_MODEL=onnx:<имя>):
    # пусто → <корень репо>/models.d/onnx (или <sys.prefix>/models.d/onnx из wheel).
    onnx_dir: str = ""
    # Remote-энкодер #31 (MUSIC_HIVE_EMBEDDING_MODEL=remote:<base_url>):
    # имя модели на model-сервере #32 и Bearer-токен; ключ реестра worker
    # берёт у сервера (GET /v1/models), не собирает локально.
    remote_model: str = "clap"
    remote_token: str = ""
    # Три окна: начало / середина / конец (сек каждое)
    embed_segment_sec: float = 30.0
    embed_workers: int = 1
    # Доля «дальних» треков в плейлистах (exploration / bandit)
    explore_ratio: float = 0.25
    worker_addr: str = "127.0.0.1:8790"
    # F1.4 §5.3: claim через FOR UPDATE SKIP LOCKED — задержка старта больше не
    # критична, дизайн-значение 2с.
    worker_poll_sec: float = 2.0
    # #40 lease: воркер продлевает claimed_at хартбитом (~lease/10, ≤30с);
    # job со status='running' и claimed_at старше lease_sec reaper возвращает
    # в pending, а при исчерпанных попытках — в failed («ядовитая» задача).
    lease_sec: float = 300.0
    job_max_attempts: int = Field(default=3, ge=1)
    # Rebuild all recommendation shelves once per night. "local" uses the
    # worker host/container timezone; an IANA name (e.g. Europe/Moscow) is safer
    # for containers whose system timezone is UTC.
    nightly_mixes_enabled: bool = True
    nightly_mixes_hour: int = Field(default=3, ge=0, le=23)
    nightly_mixes_timezone: str = "local"
    # Player notifies itself after embed/rescan (set by music-hive-player autostart)
    player_reload_url: str = "http://127.0.0.1:8787/api/reload"
    # Library folder watcher (music-hive watch)
    watch_debounce_sec: float = 45.0
    watch_clusters: bool = True
    watch_mixes: bool = True

    def ensure_dirs(self) -> None:
        self.artwork_cache.mkdir(parents=True, exist_ok=True)


@lru_cache
def get_settings() -> Settings:
    legacy = legacy_env_vars()
    if legacy:
        logger.warning(
            "обнаружены переменные старого формата musik; переименуйте "
            "(MUSIK_* → MUSIC_HIVE_*): %s",
            ", ".join(legacy),
        )
    s = Settings()
    s.ensure_dirs()
    return s
