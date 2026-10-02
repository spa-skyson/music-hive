-- 00001_init.sql — эталонная схема music-hive v2 (фаза F1).
-- Источник: docs/architecture/schema-postgres.sql (КОНТРАКТ — правки только там).
--
-- АДАПТАЦИЯ под goose: контракт предлагал завернуть весь DDL в один блок
-- StatementBegin/End, но драйвер pgx (extended protocol) не выполняет
-- multi-statement Exec, поэтому каждый стейтмент завершается обычным ';'
-- и goose разбирает их по отдельности. Semicolons внутри тел стейтментов
-- (CHECK и пр.) отсутствуют, StatementBegin не требуется.
--
-- Таблицы emb_* и emb_*_groups СОЗНАТЕЛЬНО отсутствуют: их создаёт активация
-- модели эмбеддинга (job model_activate), HNSW требует фиксированной
-- размерности vector(N). См. docs/architecture/schema-postgres.sql.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS citext;

-- ============================================================ users / auth

CREATE TABLE users (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username        CITEXT NOT NULL UNIQUE,
    display_name    TEXT NOT NULL DEFAULT '',
    -- argon2id, формат PHC-string (golang.org/x/crypto/argon2, m=64MB t=3 p=1)
    password_argon2 TEXT NOT NULL,
    -- md5(пароль); неизбежный размен за Subsonic token+salt auth (см. §7.2 дизайна).
    -- Может быть затёрт пустой строкой ценой отключения token+salt.
    subsonic_md5    TEXT NOT NULL DEFAULT '',
    is_admin        BOOLEAN NOT NULL DEFAULT FALSE,
    is_owner        BOOLEAN NOT NULL DEFAULT FALSE,   -- первый пользователь; необорудуемый админ
    settings        JSONB NOT NULL DEFAULT '{}'::jsonb, -- UI-настройки per-user
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at   TIMESTAMPTZ
);

-- Серверные сессии: cookie music_hive_session = sessions.id (opaque UUID).
CREATE TABLE sessions (
    id           UUID PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ,
    user_agent   TEXT NOT NULL DEFAULT '',
    ip           INET
);
CREATE INDEX idx_sessions_user    ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

-- Per-user API-токены: Bearer. Секрет показывается один раз; в БД sha256-hex.
CREATE TABLE api_tokens (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL DEFAULT '',
    token_hash   TEXT NOT NULL UNIQUE,               -- sha256(token) hex
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX idx_api_tokens_user ON api_tokens(user_id);

-- ============================================================ каталог

-- Артисты и альбомы становятся сущностями: стабильные id нужны Subsonic
-- (artistID/albumID) и центроидам похожести. Создаются/обновляются сканером.
CREATE TABLE artists (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT NOT NULL,
    name_norm  TEXT NOT NULL UNIQUE,                 -- lower(trim(collapse ws))
    sort_name  TEXT NOT NULL DEFAULT '',
    mbid       UUID,                                 -- musicbrainz id из тегов, если есть
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE albums (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    artist_id      BIGINT NOT NULL REFERENCES artists(id),  -- album artist
    title          TEXT NOT NULL,
    title_norm     TEXT NOT NULL,
    year           INT,
    genre          TEXT NOT NULL DEFAULT '',         -- мажоритарный жанр треков
    cover_track_id BIGINT,                           -- FK добавлен после tracks
    mbid           UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (artist_id, title_norm)
);
CREATE INDEX idx_albums_artist ON albums(artist_id);

CREATE TABLE tracks (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    path            TEXT NOT NULL UNIQUE,
    file_md5        TEXT NOT NULL DEFAULT '',
    file_mtime      DOUBLE PRECISION,
    file_size       BIGINT,
    title           TEXT NOT NULL DEFAULT '',
    -- denorm-строки для быстрых листингов (источник истины — *_id):
    artist          TEXT NOT NULL DEFAULT '',
    album           TEXT NOT NULL DEFAULT '',
    artist_id       BIGINT REFERENCES artists(id),
    album_artist_id BIGINT REFERENCES artists(id),
    album_id        BIGINT,                          -- FK добавлен после albums
    track_number    INT,
    disc_number     INT,                             -- Subsonic discNumber (новое)
    year            INT,
    duration        DOUBLE PRECISION,
    bitrate         INT,
    sample_rate     INT,
    channels        INT,
    fingerprint     TEXT,
    lufs            DOUBLE PRECISION,
    is_duplicate_of BIGINT REFERENCES tracks(id),
    is_active       BOOLEAN NOT NULL DEFAULT TRUE,
    artwork_path    TEXT,
    mbid            UUID,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_tracks_md5      ON tracks(file_md5) WHERE file_md5 <> '';
CREATE INDEX idx_tracks_active   ON tracks(is_active) WHERE is_active;
CREATE INDEX idx_tracks_artist   ON tracks(artist_id);
CREATE INDEX idx_tracks_album    ON tracks(album_id);
CREATE INDEX idx_tracks_artist_album ON tracks(artist_id, album_id);

ALTER TABLE tracks  ADD CONSTRAINT fk_tracks_album  FOREIGN KEY (album_id) REFERENCES albums(id) ON DELETE SET NULL;
ALTER TABLE albums  ADD CONSTRAINT fk_albums_cover  FOREIGN KEY (cover_track_id) REFERENCES tracks(id) ON DELETE SET NULL;

CREATE TABLE genres (
    id   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);
CREATE TABLE track_genres (
    track_id BIGINT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    genre_id BIGINT NOT NULL REFERENCES genres(id) ON DELETE CASCADE,
    PRIMARY KEY (track_id, genre_id)
);
CREATE INDEX idx_track_genres_genre ON track_genres(genre_id);

-- Аудио-фичи, не зависящие от модели эмбеддинга (бывшая часть features).
CREATE TABLE track_audio_features (
    track_id    BIGINT PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    bpm         REAL,
    key_name    TEXT,
    mode        TEXT,
    lufs        DOUBLE PRECISION,
    cluster_id  INT,
    status      TEXT NOT NULL DEFAULT 'pending',     -- pending|ready|failed|retry
    error       TEXT,
    computed_at TIMESTAMPTZ
);
CREATE INDEX idx_taf_status ON track_audio_features(status) WHERE status <> 'ready';

-- ============================================================ эмбеддинги

-- Реестр моделей. Ровно одна активная на инстанс (каталог общий — модель
-- на инстанс, не на пользователя; см. D10).
CREATE TABLE embedding_models (
    model_key    TEXT PRIMARY KEY,                   -- 'clap:laion/larger_clap_music_and_speech' | 'onnx:<name>' | 'custom:<...>'
    backend      TEXT NOT NULL,                      -- clap | onnx | custom
    repo         TEXT NOT NULL DEFAULT '',           -- HF repo / путь к .onnx
    dim          INT NOT NULL,
    params       JSONB NOT NULL DEFAULT '{}'::jsonb, -- segment strategy, sr, окна
    is_active    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX idx_embedding_models_one_active
    ON embedding_models((is_active)) WHERE is_active;

-- Физические таблицы векторов emb_* / emb_*_groups создаются активацией
-- модели (job model_activate), НЕ миграцией: HNSW-индекс требует
-- фиксированной размерности vector(N). Шаблон — в schema-postgres.sql.

-- Профиль вкуса per-user: строк мало (users × contexts), индекс не нужен,
-- поэтому vector без typmod; размерность валидируется приложением.
-- Используется только как параметр запроса к ANN-индексу активной модели.
CREATE TABLE user_taste_profiles (
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    context    TEXT NOT NULL DEFAULT 'global',       -- global|morning|evening|weekday|weekend
    model_key  TEXT NOT NULL REFERENCES embedding_models(model_key) ON DELETE CASCADE,
    vec        vector,
    n_positive INT NOT NULL DEFAULT 0,
    n_negative INT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, context, model_key)
);

-- Недельные веса дрейфа вкуса (бывшая feature_weights), per-user per-model.
CREATE TABLE user_feature_weights (
    user_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    model_key TEXT NOT NULL REFERENCES embedding_models(model_key) ON DELETE CASCADE,
    week_key  TEXT NOT NULL,
    dim       INT NOT NULL,
    weight    REAL NOT NULL,
    PRIMARY KEY (user_id, model_key, week_key, dim)
);

-- ============================================================ прослушивание

CREATE TABLE listening_history (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    track_id     BIGINT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    ts           TIMESTAMPTZ NOT NULL,
    source       TEXT,
    action       TEXT NOT NULL,                      -- start|finish|skip|like|dislike|progress|track_end
    daypart      TEXT,
    weekday      INT,
    position_sec REAL,
    duration_sec REAL,
    listened_sec REAL,
    session_id   TEXT,
    reason       TEXT                                -- completed|skipped|next|NULL
);
CREATE INDEX idx_history_user_ts        ON listening_history(user_id, ts DESC);
CREATE INDEX idx_history_track          ON listening_history(track_id);
CREATE INDEX idx_history_weekday_action ON listening_history(weekday, action);

-- Бывшая rec_stats: per-user counters для скоринга радио.
CREATE TABLE user_track_stats (
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    track_id     BIGINT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    shown        INT NOT NULL DEFAULT 0,
    skipped_early INT NOT NULL DEFAULT 0,
    completed    INT NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, track_id)
);

CREATE TABLE recommendation_impressions (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id     TEXT NOT NULL,
    track_id       BIGINT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    position       INT NOT NULL,
    score          REAL NOT NULL DEFAULT 0,
    cosine_taste   REAL NOT NULL DEFAULT 0,
    cosine_current REAL NOT NULL DEFAULT 0,
    explore        INT NOT NULL DEFAULT 0,
    new_boost      INT NOT NULL DEFAULT 0,
    maturity       TEXT NOT NULL DEFAULT '',
    mode           TEXT NOT NULL DEFAULT '',
    shown_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_impr_shown_at ON recommendation_impressions(shown_at);
CREATE INDEX idx_impr_user_session ON recommendation_impressions(user_id, session_id, track_id, shown_at);

-- ОСОЗНАННО глобально (потолок: смешивает пользователей; см. §15 дизайна).
CREATE TABLE transitions (
    from_id    BIGINT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    to_id      BIGINT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    weight     REAL NOT NULL DEFAULT 1.0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (from_id, to_id)
);

-- ============================================================ плейлисты / избранное

CREATE TABLE playlists (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL DEFAULT 'user',      -- user|mix_daily|mix_* (системные от worker)
    name          TEXT NOT NULL,
    visibility    TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','public')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    meta_json     JSONB
);
CREATE INDEX idx_playlists_owner ON playlists(owner_user_id, id DESC);
CREATE INDEX idx_playlists_public ON playlists(id DESC) WHERE visibility = 'public';

CREATE TABLE playlist_tracks (
    playlist_id BIGINT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    position    INT NOT NULL,
    track_id    BIGINT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    explanation TEXT,
    PRIMARY KEY (playlist_id, position)
);

-- Слияние favorites / favorite_artists / favorite_albums (3 SQLite-таблицы → 1).
-- Subsonic star ↔ эта таблица; rating 1–5 — отдельная user_ratings.
CREATE TABLE user_favorites (
    user_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind      TEXT NOT NULL CHECK (kind IN ('track','artist','album')),
    track_id  BIGINT REFERENCES tracks(id) ON DELETE CASCADE,
    artist_id BIGINT REFERENCES artists(id) ON DELETE CASCADE,
    album_id  BIGINT REFERENCES albums(id) ON DELETE CASCADE,
    position  INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ( -- ровно одна цель заполнена и она соответствует kind
        (CASE WHEN track_id  IS NOT NULL THEN 1 ELSE 0 END
      + CASE WHEN artist_id IS NOT NULL THEN 1 ELSE 0 END
      + CASE WHEN album_id  IS NOT NULL THEN 1 ELSE 0 END) = 1
      AND (kind <> 'track'  OR track_id  IS NOT NULL)
      AND (kind <> 'artist' OR artist_id IS NOT NULL)
      AND (kind <> 'album'  OR album_id  IS NOT NULL)
    )
);
CREATE UNIQUE INDEX idx_user_favorites_uq ON user_favorites(
    user_id, kind,
    COALESCE(track_id, 0), COALESCE(artist_id, 0), COALESCE(album_id, 0)
);
CREATE INDEX idx_user_favorites_track ON user_favorites(track_id) WHERE kind = 'track';

CREATE TABLE user_ratings (
    user_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind      TEXT NOT NULL CHECK (kind IN ('track','artist','album')),
    track_id  BIGINT REFERENCES tracks(id) ON DELETE CASCADE,
    artist_id BIGINT REFERENCES artists(id) ON DELETE CASCADE,
    album_id  BIGINT REFERENCES albums(id) ON DELETE CASCADE,
    value     SMALLINT NOT NULL CHECK (value BETWEEN 0 AND 5),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (CASE WHEN track_id  IS NOT NULL THEN 1 ELSE 0 END
      + CASE WHEN artist_id IS NOT NULL THEN 1 ELSE 0 END
      + CASE WHEN album_id  IS NOT NULL THEN 1 ELSE 0 END) = 1
    )
);
CREATE UNIQUE INDEX idx_user_ratings_uq ON user_ratings(
    user_id, kind,
    COALESCE(track_id, 0), COALESCE(artist_id, 0), COALESCE(album_id, 0)
);

CREATE TABLE user_listen_later (
    user_id  BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    track_id BIGINT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    position INT NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, track_id)
);

-- ============================================================ playback / share

-- Состояние плеера per-user (id клиентский, как сейчас).
CREATE TABLE play_sessions (
    id             TEXT PRIMARY KEY,
    user_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    mode           TEXT NOT NULL DEFAULT '',
    current_id     BIGINT NOT NULL DEFAULT 0,
    queue_json     JSONB,
    exclude_json   JSONB,
    rated_json     JSONB,
    daily_ids_json JSONB,
    daily_pos      INT NOT NULL DEFAULT 0,
    playlist_name  TEXT NOT NULL DEFAULT '',
    playlist_kind  TEXT NOT NULL DEFAULT '',
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_play_sessions_user ON play_sessions(user_id, updated_at DESC);

CREATE TABLE radio_shares (
    token         TEXT PRIMARY KEY,                  -- 128 бит hex
    user_id       BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at    TIMESTAMPTZ,
    last_listen_at TIMESTAMPTZ,
    listen_count  INT NOT NULL DEFAULT 0
);

-- ============================================================ lyrics / discover / служебное

-- Глобальный кеш текстов (не изолируется).
CREATE TABLE lyrics (
    track_id      BIGINT PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    plain_lyrics  TEXT NOT NULL DEFAULT '',
    synced_lyrics TEXT NOT NULL DEFAULT '',           -- LRC
    source        TEXT NOT NULL DEFAULT '',
    source_id     TEXT NOT NULL DEFAULT '',
    instrumental  BOOLEAN NOT NULL DEFAULT FALSE,
    status        TEXT NOT NULL DEFAULT 'pending',    -- pending|ready|missing|failed
    error         TEXT,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE discover_tips (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL,
    artist         TEXT,
    album          TEXT,
    score          REAL NOT NULL DEFAULT 0,
    track_ids_json JSONB NOT NULL,
    explanation    TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_discover_user_kind ON discover_tips(user_id, kind, created_at DESC);

CREATE TABLE scan_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Очередь заданий: таблица + polling (claim через FOR UPDATE SKIP LOCKED, §5.3).
CREATE TABLE jobs (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending',     -- pending|running|done|failed
    payload_json JSONB,
    result_json  JSONB,
    error       TEXT,
    attempts    INT NOT NULL DEFAULT 0,
    claimed_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_jobs_pending  ON jobs(id) WHERE status = 'pending';
CREATE INDEX idx_jobs_status   ON jobs(status, updated_at DESC);

-- +goose Down
-- Циклические FK tracks<->albums рвём явно до DROP (мульти-таблица DROP
-- сам разрешает остальные зависимости, но констрейнты чище убрать первыми).
ALTER TABLE tracks DROP CONSTRAINT IF EXISTS fk_tracks_album;
ALTER TABLE albums DROP CONSTRAINT IF EXISTS fk_albums_cover;
DROP TABLE IF EXISTS jobs, scan_state, discover_tips, lyrics, radio_shares,
    play_sessions, user_listen_later, user_ratings, user_favorites,
    playlist_tracks, playlists, transitions, recommendation_impressions,
    user_track_stats, listening_history, user_feature_weights,
    user_taste_profiles, embedding_models, track_audio_features,
    track_genres, genres, tracks, albums, artists, api_tokens, sessions, users;
DROP EXTENSION IF EXISTS citext;
DROP EXTENSION IF EXISTS vector;
