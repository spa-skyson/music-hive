-- 00002_users_disabled.sql — управление пользователями (F2.3, GitLab #21):
-- блокировка учётных записей и префиксы API-токенов для списка «своих токенов».
--
-- disabled: отключённый пользователь не проходит логин, а его серверные
-- сессии и Bearer-токены перестают резолвиться немедленно (запросы lookups
-- фильтруют NOT disabled). При выключении PATCH-хендлер дополнительно
-- помечает сессии revoked_at=now(), чтобы повторное включение не оживило
-- старые cookie. Владелец (is_owner) защищён от disabled приложением.
--
-- token_prefix: первые символы секрета (mht_ + 7), чтобы в списке токенов
-- можно было узнать, какой из них в менеджере паролей; сам секрет по-прежнему
-- не хранится (только sha256-hex). Существующие токены — пустой префикс.

-- +goose Up
ALTER TABLE users ADD COLUMN disabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE api_tokens ADD COLUMN token_prefix TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE api_tokens DROP COLUMN token_prefix;
ALTER TABLE users DROP COLUMN disabled;
