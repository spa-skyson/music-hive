"""Allow `python -m music_hive …` (used by player worker autostart)."""

from music_hive.cli import app

if __name__ == "__main__":
    app()
