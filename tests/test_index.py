from __future__ import annotations

import numpy as np

from music_hive.db import ensure_db, save_embedding, upsert_track
from music_hive.index.brute import load_index
from music_hive.index.clusters import _kmeans


def test_neighbors_self_excluded(pg_env, tmp_path):
    from music_hive.config import get_settings

    ensure_db()
    get_settings.cache_clear()

    # three nearly-orthogonal / similar unit vectors
    rng = np.random.default_rng(0)
    base = rng.normal(size=32).astype(np.float32)
    base /= np.linalg.norm(base)
    near = base + 0.05 * rng.normal(size=32).astype(np.float32)
    near /= np.linalg.norm(near)
    far = rng.normal(size=32).astype(np.float32)
    far /= np.linalg.norm(far)

    ids = []
    for i, (vec, md5) in enumerate([(base, "aaa"), (near, "bbb"), (far, "ccc")]):
        path = str(tmp_path / f"t{i}.wav")
        tid = upsert_track(
            {
                "path": path,
                "file_md5": md5,
                "file_mtime": 0.0,
                "file_size": 1,
                "title": f"T{i}",
                "artist": "A",
                "album": None,
                "year": None,
                "track_number": i,
                "duration": 1.0,
                "bitrate": 128,
                "sample_rate": 44100,
                "channels": 2,
                "fingerprint": None,
                "lufs": None,
                "artwork_path": None,
            }
        )
        save_embedding(tid, vec)
        ids.append(tid)

    idx = load_index()
    assert idx.size == 3
    nb = idx.neighbors(ids[0], k=2)
    assert nb[0].track_id == ids[1]
    assert nb[0].cosine > nb[1].cosine
    get_settings.cache_clear()


def test_kmeans_labels():
    rng = np.random.default_rng(1)
    a = rng.normal(size=(20, 8)).astype(np.float32)
    a /= np.linalg.norm(a, axis=1, keepdims=True)
    labels, centroids, inertia = _kmeans(a, k=3, seed=1)
    assert labels.shape == (20,)
    assert centroids.shape == (3, 8)
    assert 0.0 <= inertia <= 2.0
