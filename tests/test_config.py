from __future__ import annotations

import os

from music_hive.config import legacy_env_vars


def test_legacy_env_vars(monkeypatch):
    monkeypatch.setenv("MUSIK_PASSWORD", "x")
    monkeypatch.setenv("MUSIC_HIVE_PASSWORD", "y")
    monkeypatch.setenv("MUSIK_SESSION_SECRET", "z")
    assert legacy_env_vars() == ["MUSIK_PASSWORD", "MUSIK_SESSION_SECRET"]


def test_no_legacy_env_vars(monkeypatch):
    for name in list(os.environ):
        if name.startswith("MUSIK_"):
            monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("MUSIC_HIVE_API_TOKEN", "y")
    assert legacy_env_vars() == []


def test_legacy_env_vars_explicit_mapping():
    env = {"MUSIK_PASSWORD": "x", "MUSIC_HIVE_API_TOKEN": "y", "HOME": "/root"}
    assert legacy_env_vars(env) == ["MUSIK_PASSWORD"]
