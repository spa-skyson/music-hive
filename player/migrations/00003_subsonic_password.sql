-- 00003_subsonic_password.sql — обратимое хранение subsonic-пароля (F3.1,
-- GitLab #23): AES-GCM-шифрованный clear-пароль для token+salt
-- (t = md5hex(clear+salt) считает клиент; md5(subsonic_md5)+salt ≠ протокол).
--
-- users.subsonic_md5 (00001) остаётся для сравнения p= без расшифровки;
-- subsonic_password_enc = base64(nonce||AES-256-GCM(clear)), ключ —
-- SHA-256(MUSIC_HIVE_SUBSONIC_SECRET) либо HKDF от
-- MUSIC_HIVE_SESSION_SECRET с сепаратором "subsonic-enc". Пустая строка —
-- subsonic-пароль не задан (все /rest/*-аутентификации этого пользователя
-- завершаются ошибкой 40). Ротация секрета инвалидирует блобы (осознанно).

-- +goose Up
ALTER TABLE users ADD COLUMN subsonic_password_enc TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE users DROP COLUMN subsonic_password_enc;
